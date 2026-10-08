package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// 동점 처리의 실DB 검사. TEST_DATABASE_URL 이 없으면 건너뛰며 일회용 DB 에만 쓴다.
//
// 문서 40건이 문구·임베딩이 완전히 같아 모든 레인(fts·vec·bigm·청크 FTS/벡터)에서
// 점수가 동점이다. 사건 시각은 10개이고 각 시각을 4건이 공유하므로 두 단계의 동점
// 처리(시각 → id)가 모두 쓰인다. LIMIT 보다 동점 묶음이 훨씬 커서, 동점 처리가
// 없으면 잘리는 부분집합이 임의이고 기대 집합과 어긋난다.
//
// 벡터 레인(vec, summvec, chunk_vector)은 HNSW 인덱스 순서로 후보를 자르므로(동점 키를 그
// ORDER BY 에 넣으면 인덱스를 못 쓴다) 같은 거리의 행이 LIMIT 경계에 걸릴 때 어느 쪽이
// 들어오는지는 이 테스트가 보장하는 범위가 아니다. 그래서 문서 레인 테스트는 문서의
// 임베딩을 NULL 로 비워 fts/bigm 만 경계에서 동점을 겪게 하고, 청크 벡터 테스트는 LIMIT 을
// 청크 수 이상으로 잡아 절단 없이 전체 순서(동점 처리)를 본다.
//
// 격리: 문서를 2033-02 의 전용 시간창에 넣고 모든 질의에 그 창을 건다.

var (
	tieWinFrom = time.Date(2033, 2, 1, 0, 0, 0, 0, time.UTC)
	tieWinTo   = time.Date(2033, 2, 2, 0, 0, 0, 0, time.UTC)
)

const (
	tieDocs  = 40
	tieLimit = 5
	tieRuns  = 15
)

func newTieFixture(t *testing.T) *Postgres {
	t.Helper()
	pg := sparseDB(t)
	for i := 0; i < tieDocs; i++ {
		at := tieWinFrom.Add(time.Hour + time.Duration(i/4)*time.Minute)
		seedSparseDoc(t, pg, model.SourceCalendar, at, "zz 동점", "zz 동점 문장", `{}`)
	}
	return pg
}

// clearDocEmbeddings 는 픽스처 문서의 임베딩을 비운다(문서 벡터 레인이 비도록).
func clearDocEmbeddings(t *testing.T, pg *Postgres) {
	t.Helper()
	if _, err := pg.pool.Exec(context.Background(),
		`UPDATE documents SET embedding = NULL WHERE source_id LIKE $1`, sparseDBPrefix+"%"); err != nil {
		t.Fatal(err)
	}
}

func tieQuery() model.SearchQuery {
	from, to := tieWinFrom, tieWinTo
	return model.SearchQuery{
		Query:        "zz 동점",
		Limit:        tieLimit,
		SourceTypes:  []model.SourceType{model.SourceCalendar},
		OccurredFrom: &from,
		OccurredTo:   &to,
		Weights:      model.SearchWeights{DisableSummaryVec: true},
	}
}

// expectedTieDocs 는 후보 풀(레인당 LIMIT = 요청 limit*2)을 같은 동점 규칙으로 뽑은 뒤
// outer 순서로 정렬해 앞 tieLimit 건을 돌려준다.
func expectedTieDocs(t *testing.T, pg *Postgres, outer string) []uuid.UUID {
	t.Helper()
	rows, err := pg.pool.Query(context.Background(), `
		SELECT id FROM (
			SELECT id, occurred_at FROM documents WHERE source_id LIKE $1
			ORDER BY occurred_at DESC NULLS LAST, id ASC LIMIT $2
		) pool ORDER BY `+outer+` LIMIT $3`,
		sparseDBPrefix+"%", tieLimit*2, tieLimit)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

func expectedTieChunks(t *testing.T, pg *Postgres, n int) []int64 {
	t.Helper()
	rows, err := pg.pool.Query(context.Background(), `
		SELECT c.id FROM chunks c JOIN documents d ON d.id = c.document_id
		WHERE d.source_id LIKE $1 ORDER BY d.occurred_at DESC NULLS LAST, c.id ASC LIMIT $2`,
		sparseDBPrefix+"%", n)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

// repeatEqual 은 같은 질의를 여러 번 돌려 결과가 항상 want 와 같은지 본다.
func repeatEqual[T any](t *testing.T, name string, want []T, run func() []T) {
	t.Helper()
	if len(want) < tieLimit {
		t.Fatalf("%s: 기대 집합이 %d건 — 픽스처가 동점 묶음을 만들지 못했다", name, len(want))
	}
	for i := 0; i < tieRuns; i++ {
		if got := run(); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s 실행 %d: 동점 순서가 기대(최신 시각 → id)와 다르다\n got: %v\nwant: %v", name, i, got, want)
		}
	}
}

func TestDB_TieBreak_DocumentLanesDeterministic(t *testing.T) {
	pg := newTieFixture(t)
	clearDocEmbeddings(t, pg)
	ds := NewDocumentStore(pg)
	ctx := context.Background()
	want := expectedTieDocs(t, pg, "occurred_at DESC NULLS LAST, id ASC")

	ids := func(rs []*model.SearchResult, err error) []uuid.UUID {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		out := make([]uuid.UUID, len(rs))
		for i, r := range rs {
			out[i] = r.ID
		}
		return out
	}

	hybrid := tieQuery()
	hybrid.Embedding = testEmbedding()
	repeatEqual(t, "hybrid", want, func() []uuid.UUID { return ids(ds.hybridSearch(ctx, hybrid)) })
	repeatEqual(t, "fulltext", want, func() []uuid.UUID { return ids(ds.fulltextSearch(ctx, tieQuery())) })

	// 최신순 정렬 요청도 같은 시각 안에서는 id 로 끝난다. 시간창이 미래라
	// 가까운 순(occurred_at ASC)이다.
	recent := hybrid
	recent.Sort = model.SortRecent
	wantRecent := expectedTieDocs(t, pg, "occurred_at ASC, id ASC")
	repeatEqual(t, "hybrid/recent", wantRecent, func() []uuid.UUID { return ids(ds.hybridSearch(ctx, recent)) })
}

func TestDB_TieBreak_ChunkLanesDeterministic(t *testing.T) {
	pg := newTieFixture(t)
	cs := NewChunkStore(pg)
	ctx := context.Background()
	cut := expectedTieChunks(t, pg, tieLimit)
	full := expectedTieChunks(t, pg, tieDocs)

	ids := func(rs []ChunkSearchResult, err error) []int64 {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		out := make([]int64, len(rs))
		for i, r := range rs {
			out[i] = r.ID
		}
		return out
	}

	q := tieQuery()
	q.Embedding = testEmbedding()
	// 절단이 없는 전체 순서(동점 처리만의 검사).
	repeatEqual(t, "chunk_vector/full", full, func() []int64 { return ids(cs.SearchVectorFiltered(ctx, q, tieDocs)) })
	// 문서 레인과 같은 방식으로 절단되는 청크 FTS 계열.
	repeatEqual(t, "chunk_fts", cut, func() []int64 { return ids(cs.SearchFTSFiltered(ctx, q, tieLimit)) })
	repeatEqual(t, "chunk_sparse_ctx", cut, func() []int64 {
		return ids(cs.SearchSparseContextFiltered(ctx, q, tieLimit, model.ChunkSparseCtxV1TP))
	})
}
