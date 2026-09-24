package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/sparseq"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
)

// #276 희소 질의 키워드의 실DB 검사. TEST_DATABASE_URL 이 없으면 건너뛰며,
// 그 값은 절대 운영 DB 를 가리키면 안 된다(일회용 컨테이너만). 모든
// 데이터는 가상이다.
//
// 격리: 모든 문서를 2031-03 의 전용 시간창에 넣고, 모든 질의에 그 창을
// 건다. 다른 DB 테스트는 이 기간을 쓰지 않으므로 "raw 에서는 0건" 이라는
// 음성 대조군이 다른 테스트 데이터 때문에 흔들리지 않는다.

const sparseDBPrefix = "zz-dummy-sparseq-"

// sparseSentence 는 #276 의 대표 문장형 질의다. raw 경로(plainto_tsquery
// AND + LIKE '%질문 전체%')로는 아무것도 못 찾는다.
const sparseSentence = "이번 주 회의 일정 알려줘"

var (
	sparseWinFrom = time.Date(2031, 3, 1, 0, 0, 0, 0, time.UTC)
	sparseWinTo   = time.Date(2031, 3, 2, 0, 0, 0, 0, time.UTC)
)

type sparseFixture struct {
	pg          *Postgres
	match       uuid.UUID // "회의를", "일정은" 이 든 문서(청크 문맥 없음 → fuse_ctx raw CTE)
	ctxMatch    uuid.UUID // 본문에는 키워드가 없고 파생 문맥(sparse_text)에만 있는 문서
	noMatch     uuid.UUID // 키워드가 없는 문서
	outside     uuid.UUID // 키워드는 있지만 시간창 밖
	disposable  uuid.UUID // 키워드는 있지만 retention=disposable
	otherSource uuid.UUID // 키워드는 있지만 gmail
}

func sparseDB(t *testing.T) *Postgres {
	t.Helper()
	pg := srcTestDB(t)
	t.Cleanup(func() {
		if _, err := pg.pool.Exec(context.Background(), `DELETE FROM documents WHERE source_id LIKE $1`, sparseDBPrefix+"%"); err != nil {
			t.Errorf("cleanup sparse documents: %v", err)
		}
	})
	return pg
}

func seedSparseDoc(t *testing.T, pg *Postgres, src model.SourceType, occurred time.Time, title, body string, metadata string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if _, err := pg.pool.Exec(ctx, `
		INSERT INTO documents (id, source_type, source_id, title, content, metadata, embedding, occurred_at, collected_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $8)`,
		id, string(src), sparseDBPrefix+id.String(), title, body, metadata,
		pgvector.NewVector(testEmbedding()), occurred,
	); err != nil {
		t.Fatalf("seed document: %v", err)
	}
	if _, err := pg.pool.Exec(ctx, `
		INSERT INTO chunks (document_id, chunk_index, content, byte_size, embedding)
		VALUES ($1, 0, $2, $3, $4)`,
		id, body, len(body), pgvector.NewVector(testEmbedding()),
	); err != nil {
		t.Fatalf("seed chunk: %v", err)
	}
	return id
}

// seedSparseCtx 는 문서의 청크에 현재 fingerprint 로 파생 문맥 행을 넣는다
// (fuse_ctx 의 fresh CTE 가 읽는 행).
func seedSparseCtx(t *testing.T, pg *Postgres, docID uuid.UUID, version, sparseText string) {
	t.Helper()
	if _, err := pg.pool.Exec(context.Background(), `
		INSERT INTO chunk_sparse_context (chunk_id, context_version, fingerprint, sparse_text)
		SELECT c.id, $2, `+chunkSparseFingerprintSQL+`, $3
		FROM chunks c JOIN documents d ON d.id = c.document_id
		WHERE c.document_id = $1`, docID, version, sparseText); err != nil {
		t.Fatalf("seed sparse context: %v", err)
	}
}

func newSparseFixture(t *testing.T) sparseFixture {
	t.Helper()
	pg := sparseDB(t)
	in := sparseWinFrom.Add(3 * time.Hour)
	f := sparseFixture{pg: pg}
	f.match = seedSparseDoc(t, pg, model.SourceCalendar, in, "zz 주간 모임", "다음 주 회의를 목요일로 옮겼다. 일정은 추후 공지한다.", `{}`)
	f.ctxMatch = seedSparseDoc(t, pg, model.SourceCalendar, in, "zz 팀 모임", "장소는 삼층 라운지로 한다.", `{}`)
	seedSparseCtx(t, pg, f.ctxMatch, model.ChunkSparseCtxV1TP, "zz 팀 회의록\n장소는 삼층 라운지로 한다.")
	f.noMatch = seedSparseDoc(t, pg, model.SourceCalendar, in, "zz 점심", "점심 메뉴는 국수로 정했다.", `{}`)
	f.outside = seedSparseDoc(t, pg, model.SourceCalendar, sparseWinTo.Add(time.Hour), "zz 바깥", "회의를 다시 잡자. 일정은 미정.", `{}`)
	f.disposable = seedSparseDoc(t, pg, model.SourceCalendar, in, "zz 광고", "회의를 위한 할인 일정은 오늘까지", `{"retention":"disposable"}`)
	f.otherSource = seedSparseDoc(t, pg, model.SourceGmail, in, "zz 메일", "회의를 잡았고 일정은 목요일이다.", `{}`)
	return f
}

// sparseWindowQuery 는 격리 시간창 + 기본 필터를 건 질의다. withTerms 가
// true 면 sparseq 로 뽑은 키워드를 싣는다(= chunk_doc 모드의 저장소 입력).
func sparseWindowQuery(question string, withTerms bool) model.SearchQuery {
	from, to := sparseWinFrom, sparseWinTo
	q := model.SearchQuery{
		Query:            question,
		Limit:            20,
		SourceTypes:      []model.SourceType{model.SourceCalendar},
		ExcludeRetention: []string{model.RetentionDisposable},
		OccurredFrom:     &from,
		OccurredTo:       &to,
	}
	if withTerms {
		terms := sparseq.Extract(question)
		q.SparseTerms = model.SparseTerms{TSQuery: sparseq.TSQuery(terms.TS), Like: terms.Like}
	}
	return q
}

func chunkDocIDs(rows []ChunkSearchResult) map[uuid.UUID]bool {
	out := map[uuid.UUID]bool{}
	for _, r := range rows {
		out[r.Chunk.DocumentID] = true
	}
	return out
}

func resultDocIDs(rows []*model.SearchResult) map[uuid.UUID]float64 {
	out := map[uuid.UUID]float64{}
	for _, r := range rows {
		out[r.ID] = r.Score
	}
	return out
}

// TestDB_SparseQuery_ChunkFTS_SentenceQuery 는 핵심 주장을 실DB 로 증명한다:
// 문장형 질의가 raw 에서는 0건, 키워드 모드에서는 "회의를"·"일정은" 어절을
// 가진 청크를 찾는다. 시간창·출처·보존 필터는 키워드 모드에서도 그대로 건다.
func TestDB_SparseQuery_ChunkFTS_SentenceQuery(t *testing.T) {
	f := newSparseFixture(t)
	cs := NewChunkStore(f.pg)
	ctx := context.Background()

	raw, err := cs.SearchFTSFiltered(ctx, sparseWindowQuery(sparseSentence, false), 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 0 {
		t.Fatalf("음성 대조군: raw 모드가 %d건을 찾았다 — 이 테스트가 증명하려는 결함이 재현되지 않는다", len(raw))
	}

	got, err := cs.SearchFTSFiltered(ctx, sparseWindowQuery(sparseSentence, true), 20)
	if err != nil {
		t.Fatal(err)
	}
	ids := chunkDocIDs(got)
	if !ids[f.match] {
		t.Errorf("키워드 모드가 '회의를'·'일정은' 청크를 못 찾았다: %d건", len(got))
	}
	for name, id := range map[string]uuid.UUID{"noMatch": f.noMatch, "outside": f.outside, "disposable": f.disposable, "otherSource": f.otherSource, "ctxOnly": f.ctxMatch} {
		if ids[id] {
			t.Errorf("키워드 모드에 %s 문서가 섞였다", name)
		}
	}
	for _, r := range got {
		if r.Rank <= 0 {
			t.Errorf("rank = %v, want > 0", r.Rank)
		}
	}
}

// TestDB_SparseQuery_SparseContext 는 fuse_ctx 의 두 CTE(fresh: 파생 문맥,
// raw: 청크 본문) 모두에 키워드가 걸리는지 본다.
func TestDB_SparseQuery_SparseContext(t *testing.T) {
	f := newSparseFixture(t)
	cs := NewChunkStore(f.pg)
	ctx := context.Background()

	raw, err := cs.SearchSparseContextFiltered(ctx, sparseWindowQuery(sparseSentence, false), 20, model.ChunkSparseCtxV1TP)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 0 {
		t.Fatalf("음성 대조군: raw 모드가 %d건을 찾았다", len(raw))
	}
	got, err := cs.SearchSparseContextFiltered(ctx, sparseWindowQuery(sparseSentence, true), 20, model.ChunkSparseCtxV1TP)
	if err != nil {
		t.Fatal(err)
	}
	ids := chunkDocIDs(got)
	if !ids[f.match] {
		t.Error("raw CTE(문맥 없는 청크)가 키워드로 매칭되지 않았다")
	}
	if !ids[f.ctxMatch] {
		t.Error("fresh CTE(파생 문맥 '회의록')가 접두 키워드로 매칭되지 않았다")
	}
	for name, id := range map[string]uuid.UUID{"noMatch": f.noMatch, "outside": f.outside, "disposable": f.disposable, "otherSource": f.otherSource} {
		if ids[id] {
			t.Errorf("fuse_ctx 키워드 모드에 %s 문서가 섞였다(필터가 CTE 안에서 빠졌다)", name)
		}
	}
	for _, r := range got {
		if r.Chunk.Content == "" || strings.Contains(r.Chunk.Content, "회의록") {
			t.Errorf("fuse_ctx 는 sparse_text 가 아니라 c.content 를 돌려줘야 한다: %q", r.Chunk.Content)
		}
	}
}

// TestDB_SparseQuery_DocumentLanes 는 chunk_doc 모드의 문서 레인을 본다.
// fulltext(임베딩 없음)는 raw 0건 → 키워드 모드 매칭. hybrid 는 벡터 레인이
// 시간창 안 문서를 전부 돌려주므로 건수 대신 점수로 본다: raw 에서는 어떤
// 문서도 벡터 레인 한 개 몫(1/61)을 넘지 못하고, 키워드 모드에서는 매칭
// 문서가 fts·bigm 레인 점수를 더 받는다.
func TestDB_SparseQuery_DocumentLanes(t *testing.T) {
	f := newSparseFixture(t)
	ds := NewDocumentStore(f.pg)
	ctx := context.Background()

	raw, err := ds.Search(ctx, sparseWindowQuery(sparseSentence, false))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 0 {
		t.Fatalf("음성 대조군: fulltext raw 가 %d건을 찾았다", len(raw))
	}
	got, err := ds.Search(ctx, sparseWindowQuery(sparseSentence, true))
	if err != nil {
		t.Fatal(err)
	}
	ids := resultDocIDs(got)
	if _, ok := ids[f.match]; !ok {
		t.Errorf("fulltext 키워드 모드가 매칭 문서를 못 찾았다: %d건", len(got))
	}
	for name, id := range map[string]uuid.UUID{"noMatch": f.noMatch, "outside": f.outside, "disposable": f.disposable, "otherSource": f.otherSource} {
		if _, ok := ids[id]; ok {
			t.Errorf("fulltext 키워드 모드에 %s 문서가 섞였다", name)
		}
	}

	hq := sparseWindowQuery(sparseSentence, false)
	hq.Embedding = testEmbedding()
	hq.Weights = model.SearchWeights{EntityWeight: -1, DisableSummaryVec: true}
	rawH, err := ds.Search(ctx, hq)
	if err != nil {
		t.Fatal(err)
	}
	if len(rawH) == 0 {
		t.Fatal("hybrid raw: 벡터 레인이 아무것도 못 찾았다(픽스처 오류)")
	}
	oneLane := 1.0/61.0 + 1e-9
	for _, r := range rawH {
		if r.Score > oneLane {
			t.Errorf("hybrid raw: 문서 점수 %v 가 한 레인 몫을 넘었다 — fts/bigm 이 원문으로 매칭됐다", r.Score)
		}
	}
	hq.SparseTerms = sparseWindowQuery(sparseSentence, true).SparseTerms
	termsH, err := ds.Search(ctx, hq)
	if err != nil {
		t.Fatal(err)
	}
	hs := resultDocIDs(termsH)
	if hs[f.match] <= oneLane*2 {
		t.Errorf("hybrid 키워드 모드: 매칭 문서 점수 %v, want vec+fts+bigm (> %v)", hs[f.match], oneLane*2)
	}
	if hs[f.noMatch] > oneLane {
		t.Errorf("hybrid 키워드 모드: 비매칭 문서 점수 %v 가 한 레인 몫을 넘었다", hs[f.noMatch])
	}
	for name, id := range map[string]uuid.UUID{"outside": f.outside, "disposable": f.disposable, "otherSource": f.otherSource} {
		if _, ok := hs[id]; ok {
			t.Errorf("hybrid 키워드 모드에 %s 문서가 섞였다", name)
		}
	}
}

// TestDB_SparseQuery_AdversarialInputs 는 tsquery 연산자·따옴표·역슬래시·
// 긴 입력·이모지만 있는 입력이 어떤 레인에서도 SQL 오류를 내지 않는지 본다.
// 원문을 그대로 쓰는 raw 경로와 같은 입력으로 돌린다.
func TestDB_SparseQuery_AdversarialInputs(t *testing.T) {
	pg := sparseDB(t)
	ctx := context.Background()
	cs := NewChunkStore(pg)
	ds := NewDocumentStore(pg)

	inputs := []string{
		`'`, `''`, `\`, `\\'`, `&|!():*<->`, `a & b | !c`, `회의' | '일정`, `'회의':* & !'일정'`,
		`'); DROP TABLE documents; --`, `x:* <-> y:A`, `foo@bar.com:*`, `010-1234-5678'`,
		`😀🎉`, `​`, strings.Repeat("회의를 일정은 ", 150), strings.Repeat("a'b\\c ", 200),
		`İstanbul ǅemal`, `%_%`, `100%`, `under_score_term`,
	}
	checked := 0
	for _, in := range inputs {
		terms := sparseq.Extract(in)
		st := model.SparseTerms{TSQuery: sparseq.TSQuery(terms.TS), Like: terms.Like}
		for _, tsq := range []string{st.TSQuery, sparseq.TSQuery(strings.Fields(in))} {
			if tsq == "" {
				continue
			}
			var s string
			if err := pg.pool.QueryRow(ctx, `SELECT to_tsquery('simple', $1)::text || to_tsquery('english', $1)::text`, tsq).Scan(&s); err != nil {
				t.Errorf("to_tsquery(%q) (입력 %q) 오류: %v", tsq, in, err)
			}
			checked++
		}
		if !st.Active() {
			continue
		}
		q := sparseWindowQuery(in, false)
		q.SparseTerms = st
		if _, err := cs.SearchFTSFiltered(ctx, q, 5); err != nil {
			t.Errorf("chunk FTS (입력 %q): %v", in, err)
		}
		if _, err := cs.SearchSparseContextFiltered(ctx, q, 5, model.ChunkSparseCtxV1Full); err != nil {
			t.Errorf("sparse ctx (입력 %q): %v", in, err)
		}
		if _, err := ds.Search(ctx, q); err != nil {
			t.Errorf("fulltext (입력 %q): %v", in, err)
		}
		q.Embedding = testEmbedding()
		q.Weights = model.SearchWeights{EntityWeight: 1.5}
		if _, err := ds.Search(ctx, q); err != nil {
			t.Errorf("hybrid (입력 %q): %v", in, err)
		}
		checked++
	}
	// 양성 대조군: 대부분의 입력은 키워드가 남아 실제 SQL 을 돌려야 한다.
	if checked < 15 {
		t.Fatalf("실제로 실행한 검사가 %d건뿐이다 — 입력이 전부 빈 키워드로 빠졌다", checked)
	}
}

// TestDB_SparseQuery_BigmIndexUsed 는 키워드별로 펼친 OR LIKE 를 플래너가
// pg_bigm GIN 인덱스로 처리할 수 있는지 본다(F11: LIKE ANY(배열)는 GIN 이
// 못 쓴다). 순차 스캔을 끈 상태에서 플랜에 bigm 인덱스가 나와야 한다.
func TestDB_SparseQuery_BigmIndexUsed(t *testing.T) {
	pg := sparseDB(t)

	q := model.SearchQuery{Query: "zz 회의 일정 예산 보고서 결과 공유 김철수 목요일"}
	terms := sparseq.Extract(q.Query)
	if len(terms.Like) != sparseq.MaxTerms {
		t.Fatalf("fixture: want %d like terms, got %d", sparseq.MaxTerms, len(terms.Like))
	}
	q.SparseTerms = model.SparseTerms{TSQuery: sparseq.TSQuery(terms.TS), Like: terms.Like}

	// 레인 SQL 전체를 EXPLAIN 하면 빈 테이블에서 플래너가 documents 쪽
	// 인덱스로 먼저 좁히는 조인을 골라 술어가 Filter 로 밀려난다 — 인덱스를
	// 못 쓰는 것이 아니라 쓸 필요가 없는 것이다. 그래서 빌더가 만든 매칭
	// 술어(chunkSparseExprs, 레인 SQL 이 그대로 쓰는 조각)만 단일 테이블
	// 질의로 떼어 내 인덱스로 풀리는지 본다.
	args, sp := bindChunkSparse([]interface{}{q.Query}, q.SparseTerms)
	for _, c := range []struct {
		name, table, tsv, text, index string
	}{
		{"chunk_fts", "chunks c", "c.content_tsv", "c.content", "idx_chunks_content_bigm"},
		{"sparse_ctx", "chunk_sparse_context sc", "sc.sparse_tsv", "sc.sparse_text", "idx_chunk_sparse_context_bigm"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := chunkSparseExprs(sp, c.tsv, c.text)
			// $1(질문 원문)은 매칭 술어에 쓰이지 않으므로 타입을 고정해 둔다.
			sql := "SELECT $1::text, " + c.tsv + " FROM " + c.table + " WHERE (" + e.matchTS + " OR " + e.matchLike + ")"
			plan := explainNoSeqscan(t, pg, sql, args)
			if !strings.Contains(plan, "BitmapOr") || strings.Count(plan, c.index) != len(terms.Like) {
				t.Errorf("키워드 %d개가 %s 의 BitmapOr 로 풀리지 않았다:\n%s", len(terms.Like), c.index, plan)
			}
		})
	}
}

func explainNoSeqscan(t *testing.T, pg *Postgres, sql string, args []interface{}) string {
	t.Helper()
	ctx := context.Background()
	tx, err := pg.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, "EXPLAIN "+sql, args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	n := 0
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(&b, line)
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("EXPLAIN 이 플랜을 한 줄도 돌려주지 않았다")
	}
	return b.String()
}
