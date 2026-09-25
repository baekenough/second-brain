package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// #290 ingest/messages 실DB 테스트. TEST_DATABASE_URL 이 없으면 건너뛴다.
// 운영 DB 를 가리키면 안 된다 — 마이그레이션을 적용하고, 트리거·제약을 잠깐
// 만들고, 테스트 행을 쓴다(끝나면 표식으로 모두 지운다).
//
// 일시 오류는 실제로 만든다: 표식 행에서만 pg_sleep 하는 행 트리거를 걸고,
// 별도 연결에서 pg_stat_activity 로 그 백엔드를 찾아 pg_terminate_backend 한다
// (DB 연결이 처리 도중 끊긴 모양). COPY 에도 행 트리거가 발동하므로 청크 교체
// 단계도 같은 방법으로 끊을 수 있다.

// ingestDBSleepToken 이 본문에 들어간 행만 트리거가 재운다.
const ingestDBSleepToken = "zzprsleeptoken"

type ingestDBEnv struct {
	t       *testing.T
	ctx     context.Context
	pg      *store.Postgres
	monitor *pgx.Conn
	srv     *Server
	marker  string
}

// newIngestDBEnv 는 실DB·관측 연결·서버를 만든다. 표식은 숫자 없이 만든다 —
// SMS 매핑이 4~8자리 숫자열을 [REDACTED] 로 바꿔 정리 쿼리가 행을 놓칠 수 있다.
func newIngestDBEnv(t *testing.T) *ingestDBEnv {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-database ingest test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)

	pg, err := store.NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pg.Close)
	if err := pg.RunMigrations(ctx, filepath.Join("..", "..", "migrations"), 1536); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	monitor, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("monitor connect: %v", err)
	}
	t.Cleanup(func() { _ = monitor.Close(context.Background()) })

	marker := "zzprtwoninety" + strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9':
			return 'g' + (r - '0')
		case r == '-':
			return -1
		}
		return r
	}, uuid.NewString())
	t.Cleanup(func() {
		if _, err := monitor.Exec(context.Background(),
			`DELETE FROM documents WHERE content LIKE $1`, "%"+marker+"%"); err != nil {
			t.Errorf("cleanup documents: %v", err)
		}
	})

	srv := NewServer(nil, nil, nil, nil, nil, "", "").
		WithIngestMessages(store.NewDocumentStore(pg), 0, time.Time{})
	return &ingestDBEnv{t: t, ctx: ctx, pg: pg, monitor: monitor, srv: srv, marker: marker}
}

func (e *ingestDBEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.monitor.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

// installSleepTrigger 는 table 의 BEFORE INSERT(문서는 UPDATE 도) 행 트리거를
// 건다. content 에 ingestDBSleepToken 이 든 행에서만 30초 잔다.
func (e *ingestDBEnv) installSleepTrigger(table string) (drop func()) {
	e.t.Helper()
	fn := "zz_pr290_sleep_" + table
	trg := "zz_pr290_sleep_" + table
	e.exec(`CREATE OR REPLACE FUNCTION ` + fn + `() RETURNS trigger AS $$
BEGIN
	IF NEW.content LIKE '%` + ingestDBSleepToken + `%' THEN PERFORM pg_sleep(30); END IF;
	RETURN NEW;
END $$ LANGUAGE plpgsql`)
	events := "INSERT"
	if table == "documents" {
		events = "INSERT OR UPDATE"
	}
	e.exec(`CREATE TRIGGER ` + trg + ` BEFORE ` + events + ` ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION ` + fn + `()`)
	var once sync.Once
	drop = func() {
		once.Do(func() {
			_, _ = e.monitor.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+trg+` ON `+table)
			_, _ = e.monitor.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+fn+`()`)
		})
	}
	e.t.Cleanup(drop)
	return drop
}

// terminateSleeper 는 별도 연결에서 table 문장을 실행하며 pg_sleep 중인
// 백엔드를 찾아 끊는다. 돌려준 함수는 끊은 백엔드 수를 기다려 돌려준다.
func (e *ingestDBEnv) terminateSleeper(table string) (wait func() int) {
	e.t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	killer, err := pgx.Connect(e.ctx, dsn)
	if err != nil {
		e.t.Fatalf("killer connect: %v", err)
	}
	done := make(chan int, 1)
	go func() {
		defer func() { _ = killer.Close(context.Background()) }()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			var n int
			err := killer.QueryRow(context.Background(), `
				SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
				WHERE datname = current_database()
				  AND pid <> pg_backend_pid()
				  AND wait_event = 'PgSleep'
				  AND query ILIKE '%' || $1 || '%'`, table).Scan(&n)
			if err == nil && n > 0 {
				done <- n
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		done <- 0
	}()
	return func() int { return <-done }
}

func (e *ingestDBEnv) post(payload map[string]any) (*httptest.ResponseRecorder, IngestMessagesResponse) {
	e.t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		e.t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(w, req)
	var resp IngestMessagesResponse
	if w.Code == http.StatusCreated {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			e.t.Fatalf("decode 201 body: %v", err)
		}
	}
	return w, resp
}

// sms 는 표식을 붙인 SMS 레코드다. 주소는 숫자를 써도 된다(해시·metadata 만).
func (e *ingestDBEnv) sms(label string, i int) (rec map[string]any, content string) {
	content = e.marker + "-" + label
	return map[string]any{
		"address": "zz-pr290-addr",
		"body":    content,
		"date_ms": time.Now().Add(-2*time.Hour).UnixMilli() + int64(i),
		"type":    1,
	}, content
}

// docChunks 는 content 가 정확히 같은 활성 SMS 문서 수와 (문서가 하나면) 그
// 문서의 청크 id 를 돌려준다.
func (e *ingestDBEnv) docChunks(content string) (docs int, chunkIDs []int64) {
	e.t.Helper()
	rows, err := e.monitor.Query(e.ctx, `SELECT id FROM documents WHERE source_type = 'sms' AND content = $1`, content)
	if err != nil {
		e.t.Fatalf("query documents: %v", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		e.t.Fatalf("collect documents: %v", err)
	}
	if len(ids) != 1 {
		return len(ids), nil
	}
	rows, err = e.monitor.Query(e.ctx, `SELECT id FROM chunks WHERE document_id = $1 ORDER BY chunk_index`, ids[0])
	if err != nil {
		e.t.Fatalf("query chunks: %v", err)
	}
	chunkIDs, err = pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		e.t.Fatalf("collect chunks: %v", err)
	}
	return 1, chunkIDs
}

// TestIngestMessages_ChunkFailureRollsBack_RealDB (계획 DB1): 청크 교체 도중
// 연결이 끊기면 503 이고 그 문서 행도 롤백된다. 트리거를 지우고 다시 보내면
// 201 이고 모든 문서에 청크가 있다. 수정 전 코드는 첫 요청이 201 이었고, 문서만
// 남은 채 재전송이 "변경 없음" 으로 청크 교체를 건너뛰어 청크가 영구히 0 이었다.
func TestIngestMessages_ChunkFailureRollsBack_RealDB(t *testing.T) {
	e := newIngestDBEnv(t)
	recA, contentA := e.sms("alpha", 0)
	recB, contentB := e.sms("bravo-"+ingestDBSleepToken, 1)
	recC, contentC := e.sms("charlie", 2)
	payload := map[string]any{"sms": []any{recA, recB, recC}}

	drop := e.installSleepTrigger("chunks")
	killed := e.terminateSleeper("chunks")
	w, _ := e.post(payload)
	if n := killed(); n != 1 {
		t.Fatalf("terminated %d sleeping chunk backends, want 1 (fault not injected)", n)
	}
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("first request: status=%d Retry-After=%q, want 503 with Retry-After; body=%s",
			w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	docsA, chunksA := e.docChunks(contentA)
	if docsA != 1 || len(chunksA) != 1 {
		t.Errorf("record before the failure: docs=%d chunks=%d, want 1/1 (committed)", docsA, len(chunksA))
	}
	if docsB, _ := e.docChunks(contentB); docsB != 0 {
		t.Errorf("failed record: %d document rows remain, want 0 (upsert must roll back with the chunk failure)", docsB)
	}
	if docsC, _ := e.docChunks(contentC); docsC != 0 {
		t.Errorf("record after the failure: %d rows, want 0 (batch must stop at the first transient error)", docsC)
	}

	drop()
	w, resp := e.post(payload)
	if w.Code != http.StatusCreated || resp.Accepted != 3 || len(resp.Errors) != 0 {
		t.Fatalf("resend: status=%d accepted=%d errors=%v, want 201/3/[]", w.Code, resp.Accepted, resp.Errors)
	}
	for _, c := range []string{contentA, contentB, contentC} {
		if docs, chunks := e.docChunks(c); docs != 1 || len(chunks) != 1 {
			t.Errorf("after resend: docs=%d chunks=%d for one record, want 1/1", docs, len(chunks))
		}
	}
	if _, again := e.docChunks(contentA); !slices.Equal(again, chunksA) {
		t.Errorf("unchanged record's chunks were rewritten: %v -> %v (resend must take the unchanged fast path)", chunksA, again)
	}
}

// TestIngestMessages_UpsertConnKilled_RealDB (계획 DB2): 문서 upsert 도중 연결이
// 끊기면 503 이고, 재전송하면 201 로 중복 없이 N 건이 된다. 앞서 커밋된 레코드는
// 재전송 때 청크가 바뀌지 않는다. 수정 전 코드는 201 + errors[] 였다(유실).
func TestIngestMessages_UpsertConnKilled_RealDB(t *testing.T) {
	e := newIngestDBEnv(t)
	recA, contentA := e.sms("alpha", 0)
	recB, contentB := e.sms("bravo-"+ingestDBSleepToken, 1)
	recC, contentC := e.sms("charlie", 2)
	payload := map[string]any{"sms": []any{recA, recB, recC}}

	drop := e.installSleepTrigger("documents")
	killed := e.terminateSleeper("documents")
	w, _ := e.post(payload)
	if n := killed(); n != 1 {
		t.Fatalf("terminated %d sleeping document backends, want 1 (fault not injected)", n)
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("first request: status=%d, want 503; body=%s", w.Code, w.Body.String())
	}
	_, chunksA := e.docChunks(contentA)
	if docsB, _ := e.docChunks(contentB); docsB != 0 {
		t.Errorf("failed record stored %d rows, want 0", docsB)
	}
	if docsC, _ := e.docChunks(contentC); docsC != 0 {
		t.Errorf("record after the failure stored %d rows, want 0", docsC)
	}

	drop()
	w, resp := e.post(payload)
	if w.Code != http.StatusCreated || resp.Accepted != 3 {
		t.Fatalf("resend: status=%d accepted=%d, want 201/3", w.Code, resp.Accepted)
	}
	var n int
	if err := e.monitor.QueryRow(e.ctx, `SELECT count(*) FROM documents WHERE content LIKE $1`, "%"+e.marker+"%").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("documents after resend = %d, want 3 (no duplicates)", n)
	}
	if _, again := e.docChunks(contentA); len(chunksA) != 1 || !slices.Equal(again, chunksA) {
		t.Errorf("committed record's chunks changed on resend: %v -> %v", chunksA, again)
	}
}

// TestIngestMessages_BudgetExpiryUnderLock_RealDB (계획 DB3): 청크 테이블이
// 잠겨 예산(테스트용 2초)이 끝나면 503 이고 문서는 롤백된다. 잠금을 풀고 다시
// 보내면 201.
func TestIngestMessages_BudgetExpiryUnderLock_RealDB(t *testing.T) {
	e := newIngestDBEnv(t)
	e.srv.messagesBudget = 2 * time.Second
	rec, content := e.sms("locked", 0)
	payload := map[string]any{"sms": []any{rec}}

	locker, err := pgx.Connect(e.ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("locker connect: %v", err)
	}
	defer func() { _ = locker.Close(context.Background()) }()
	lockTx, err := locker.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockTx.Exec(e.ctx, `LOCK TABLE chunks IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	// 요청은 다른 goroutine 에서 보낸다. 예산이 걸려 있지 않으면 잠금을 푸는
	// 쪽이 없어 영원히 기다리므로, 8초 안에 응답이 없으면 잠금을 풀고 실패로 본다.
	body, _ := json.Marshal(payload)
	codeCh := make(chan int, 1)
	start := time.Now()
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/messages", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rec, req)
		codeCh <- rec.Code
	}()
	var code int
	select {
	case code = <-codeCh:
	case <-time.After(8 * time.Second):
		t.Errorf("no response within 8s under the lock (request budget not applied)")
	}
	elapsed := time.Since(start)
	if err := lockTx.Rollback(e.ctx); err != nil {
		t.Fatal(err)
	}
	if code == 0 {
		code = <-codeCh
	}
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", code)
	}
	if elapsed < 1500*time.Millisecond || elapsed > 10*time.Second {
		t.Errorf("503 after %v, want about the 2s budget", elapsed)
	}
	if docs, _ := e.docChunks(content); docs != 0 {
		t.Errorf("document stored %d rows under the lock, want 0 (rolled back)", docs)
	}

	w, resp := e.post(payload)
	if w.Code != http.StatusCreated || resp.Accepted != 1 {
		t.Fatalf("resend: status=%d accepted=%d, want 201/1", w.Code, resp.Accepted)
	}
	if docs, chunks := e.docChunks(content); docs != 1 || len(chunks) != 1 {
		t.Errorf("after resend docs=%d chunks=%d, want 1/1", docs, len(chunks))
	}
}

// TestIngestMessages_PermanentConstraint_RealDB (계획 DB4): 레코드 내용 때문에
// 결정적으로 실패하면(23514) 그 레코드만 errors[] 에 들어가고 201 이다.
func TestIngestMessages_PermanentConstraint_RealDB(t *testing.T) {
	e := newIngestDBEnv(t)
	const poison = "zzprtwoninetypoison"
	e.exec(`ALTER TABLE documents ADD CONSTRAINT zz_pr290_poison CHECK (content NOT LIKE '%` + poison + `%') NOT VALID`)
	t.Cleanup(func() {
		_, _ = e.monitor.Exec(context.Background(), `ALTER TABLE documents DROP CONSTRAINT IF EXISTS zz_pr290_poison`)
	})

	recA, contentA := e.sms("alpha", 0)
	recP, contentP := e.sms(poison, 1)
	recC, contentC := e.sms("charlie", 2)
	w, resp := e.post(map[string]any{"sms": []any{recA, recP, recC}})
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d, want 201; body=%s", w.Code, w.Body.String())
	}
	if resp.Accepted != 2 || len(resp.Errors) != 1 || resp.Errors[0] != "sms[1]: rejected by database (sqlstate 23514)" {
		t.Errorf("accepted=%d errors=%v, want 2 and [sms[1]: rejected by database (sqlstate 23514)]", resp.Accepted, resp.Errors)
	}
	if strings.Contains(w.Body.String(), e.marker) {
		t.Errorf("response leaks record content: %s", w.Body.String())
	}
	if docs, _ := e.docChunks(contentP); docs != 0 {
		t.Errorf("poison record stored %d rows, want 0", docs)
	}
	for _, c := range []string{contentA, contentC} {
		if docs, chunks := e.docChunks(c); docs != 1 || len(chunks) != 1 {
			t.Errorf("good record docs=%d chunks=%d, want 1/1", docs, len(chunks))
		}
	}
}

// TestIngestMessages_ResendIdempotent_RealDB (계획 DB5): 같은 배치를 두 번
// 보내도 문서·청크 수와 청크 id 가 그대로이고 collected_at 만 갱신된다. 그리고
// 결정 D2 확인: 요청 경로는 임베딩하지 않으므로 새 문서·청크가 collector
// 백필이 고르는 목록(ListDocumentsNeedingEmbedding·ListChunksNeedingEmbedding,
// scheduler.backfillEmbeddings·backfillChunkEmbeddings 가 부르는 그 함수)에
// 들어 있어야 하고, 백필이 쓰는 저장 함수로 벡터를 채우면 목록에서 빠져야 한다.
func TestIngestMessages_ResendIdempotent_RealDB(t *testing.T) {
	e := newIngestDBEnv(t)
	recA, contentA := e.sms("alpha", 0)
	recB, contentB := e.sms("bravo", 1)
	payload := map[string]any{"sms": []any{recA, recB}}

	w, resp := e.post(payload)
	if w.Code != http.StatusCreated || resp.Accepted != 2 {
		t.Fatalf("first: status=%d accepted=%d, want 201/2", w.Code, resp.Accepted)
	}
	_, chunksA := e.docChunks(contentA)
	_, chunksB := e.docChunks(contentB)
	collected := func() time.Time {
		var ts time.Time
		if err := e.monitor.QueryRow(e.ctx, `SELECT collected_at FROM documents WHERE content = $1`, contentA).Scan(&ts); err != nil {
			t.Fatal(err)
		}
		return ts
	}
	before := collected()
	time.Sleep(20 * time.Millisecond)

	w, resp = e.post(payload)
	if w.Code != http.StatusCreated || resp.Accepted != 2 || len(resp.Errors) != 0 {
		t.Fatalf("second: status=%d accepted=%d errors=%v, want 201/2/[]", w.Code, resp.Accepted, resp.Errors)
	}
	docsA, againA := e.docChunks(contentA)
	docsB, againB := e.docChunks(contentB)
	if docsA != 1 || docsB != 1 || len(chunksA) != 1 || !slices.Equal(againA, chunksA) || !slices.Equal(againB, chunksB) {
		t.Errorf("resend changed state: docs=%d/%d chunks %v->%v, %v->%v", docsA, docsB, chunksA, againA, chunksB, againB)
	}
	if after := collected(); !after.After(before) {
		t.Errorf("collected_at not refreshed on resend: %v -> %v", before, after)
	}

	// D2: 백필 대상에 들어 있는가.
	docStore := store.NewDocumentStore(e.pg)
	chunkStore := store.NewChunkStore(e.pg)
	docIDs := map[uuid.UUID]bool{}
	for _, c := range []string{contentA, contentB} {
		var id uuid.UUID
		if err := e.monitor.QueryRow(e.ctx, `SELECT id FROM documents WHERE content = $1`, c).Scan(&id); err != nil {
			t.Fatal(err)
		}
		docIDs[id] = true
	}
	wantChunks := map[int64]bool{chunksA[0]: true, chunksB[0]: true}
	pendingDocs := func() int {
		docs, err := docStore.ListDocumentsNeedingEmbedding(e.ctx, 1_000_000, "")
		if err != nil {
			t.Fatalf("ListDocumentsNeedingEmbedding: %v", err)
		}
		n := 0
		for _, d := range docs {
			if docIDs[d.ID] {
				n++
			}
		}
		return n
	}
	pendingChunks := func() []store.UnembeddedChunk {
		chunks, err := chunkStore.ListChunksNeedingEmbedding(e.ctx, 1_000_000, "")
		if err != nil {
			t.Fatalf("ListChunksNeedingEmbedding: %v", err)
		}
		var mine []store.UnembeddedChunk
		for _, c := range chunks {
			if wantChunks[c.ID] {
				mine = append(mine, c)
			}
		}
		return mine
	}
	if n := pendingDocs(); n != 2 {
		t.Errorf("documents pending document-embedding backfill = %d, want 2", n)
	}
	mine := pendingChunks()
	if len(mine) != 2 {
		t.Fatalf("chunks pending chunk-embedding backfill = %d, want 2", len(mine))
	}

	// 백필의 쓰기 단계를 같은 저장 함수로 흉내 낸다.
	vec := make([]float32, 1536)
	vec[0] = 1
	var embs []store.ChunkEmbedding
	for _, c := range mine {
		embs = append(embs, store.ChunkEmbedding{ChunkID: c.ID, Embedding: vec, Version: "test"})
	}
	if err := chunkStore.UpdateChunkEmbeddings(e.ctx, embs); err != nil {
		t.Fatalf("UpdateChunkEmbeddings: %v", err)
	}
	for id := range docIDs {
		if err := docStore.UpdateEmbedding(e.ctx, &model.Document{ID: id, Embedding: vec, EmbeddingVersion: "test"}); err != nil {
			t.Fatalf("UpdateEmbedding: %v", err)
		}
	}
	if n := pendingDocs(); n != 0 {
		t.Errorf("documents still pending after backfill write = %d, want 0", n)
	}
	if n := len(pendingChunks()); n != 0 {
		t.Errorf("chunks still pending after backfill write = %d, want 0", n)
	}
}

// TestIngestMessages_CallDuplicateTranscript_RealDB (계획 DB6): 다른 source_id
// 로 같은 내용의 활성 통화 문서가 있으면 skipped 1, errors 0, 201 이다(수정
// 전: errors[] 1건). 일시 오류로 분류했다면 503 이 영원히 반복됐을 것이다.
func TestIngestMessages_CallDuplicateTranscript_RealDB(t *testing.T) {
	e := newIngestDBEnv(t)
	call := map[string]any{
		"number":       "zz-pr290-num",
		"date_ms":      time.Now().Add(-3 * time.Hour).UnixMilli(),
		"duration_sec": 7,
		"type":         1,
		"contact_name": e.marker + "-dup",
	}
	raw, _ := json.Marshal(call)
	prepared, verr := e.srv.prepareCallRecord(0, raw, newIngestMessagesResult())
	if verr != nil || prepared.doc == nil {
		t.Fatalf("prepare call: %v", verr)
	}
	mapped := prepared.doc
	existing := *mapped
	existing.ID = uuid.New()
	existing.SourceID = "call-transcript:" + e.marker
	if err := store.NewDocumentStore(e.pg).Upsert(e.ctx, &existing); err != nil {
		t.Fatalf("seed duplicate: %v", err)
	}

	w, resp := e.post(map[string]any{"calls": []any{call}})
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d, want 201; body=%s", w.Code, w.Body.String())
	}
	if resp.Skipped != 1 || resp.Accepted != 0 || len(resp.Errors) != 0 {
		t.Errorf("accepted=%d skipped=%d errors=%v, want 0/1/[]", resp.Accepted, resp.Skipped, resp.Errors)
	}
	var n int
	if err := e.monitor.QueryRow(e.ctx, `SELECT count(*) FROM documents WHERE source_id = $1`, mapped.SourceID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("duplicate call stored %d rows under its own source_id, want 0", n)
	}
}
