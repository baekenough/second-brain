package search

import (
	"context"
	"errors"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// traceDocSearcher 는 고정된 문서 집합을 돌려주는 문서 스토어 대역이다.
type traceDocSearcher struct{ ids []uuid.UUID }

func (d traceDocSearcher) Search(_ context.Context, _ model.SearchQuery) ([]*model.SearchResult, error) {
	out := make([]*model.SearchResult, 0, len(d.ids))
	for i, id := range d.ids {
		out = append(out, &model.SearchResult{
			Document: model.Document{ID: id, SourceType: model.SourceGmail},
			Score:    float64(len(d.ids) - i),
		})
	}
	return out, nil
}

// traceReranker 는 순서를 뒤집는 리랭커 대역이다. failing 이면 항상 실패한다.
type traceReranker struct{ failing bool }

func (r traceReranker) Enabled() bool { return true }
func (r traceReranker) Rerank(_ context.Context, _ string, docs []string) ([]RerankResult, error) {
	if r.failing {
		return nil, errors.New("synthetic rerank failure")
	}
	out := make([]RerankResult, 0, len(docs))
	for i := len(docs) - 1; i >= 0; i-- {
		out = append(out, RerankResult{Index: i, Score: float64(len(docs) - i)})
	}
	return out, nil
}

func traceIDSet(n int) []uuid.UUID {
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = uuid.New()
	}
	return ids
}

// 추적은 관찰일 뿐이어야 한다. 같은 질의를 Search 와 SearchTraced 로 돌렸을 때
// 돌아오는 순위가 다르면 진단 결과로 운영 검색을 논할 수 없게 된다.
func TestSearchTracedReturnsSameRankingAsSearch(t *testing.T) {
	ids := traceIDSet(5)
	svc := NewService(traceDocSearcher{ids: ids}, disabledEmbedderForTrace{}).
		WithReranker(traceReranker{})
	query := model.SearchQuery{Query: "q", Limit: 3, UseRerank: true}

	plain, err := svc.Search(context.Background(), query)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	traced, trace, err := svc.SearchTraced(context.Background(), query)
	if err != nil {
		t.Fatalf("SearchTraced: %v", err)
	}
	if len(plain) != len(traced) {
		t.Fatalf("result count diverged: %d vs %d", len(plain), len(traced))
	}
	for i := range plain {
		if plain[i].ID != traced[i].ID {
			t.Fatalf("rank %d diverged: %s vs %s", i+1, plain[i].ID, traced[i].ID)
		}
	}

	// 오버페치 풀은 페이지(3건)보다 커야 "회수는 됐지만 10위 밖" 을 구분할 수 있다.
	if len(trace.PoolIDs) != len(ids) {
		t.Fatalf("pool size = %d, want %d", len(trace.PoolIDs), len(ids))
	}
	if len(trace.PreRerankIDs) != len(ids) {
		t.Fatalf("pre-rerank order = %d entries, want %d", len(trace.PreRerankIDs), len(ids))
	}
	if !trace.RerankRequested || !trace.RerankAttempted || trace.RerankFailed {
		t.Fatalf("rerank flags: %+v", trace)
	}
	for _, id := range ids {
		lanes := trace.LaneHits[id]
		if len(lanes) != 1 || lanes[0] != LaneDocumentStore {
			t.Fatalf("lanes for %s = %v, want [%s]", id, lanes, LaneDocumentStore)
		}
	}
	// 리랭커가 순서를 뒤집었으므로 리랭크 전후 1위는 달라야 한다 —
	// PreRerankIDs 가 사후 순서의 복사본이면 이 검사가 실패한다.
	if trace.PreRerankIDs[0] == trace.PoolIDs[0] {
		t.Fatal("pre-rerank order equals post-rerank order; the snapshot was taken too late")
	}
}

// 리랭커 실패는 경고 로그로만 남고 순서가 조용히 원상복구된다. 그 사실이
// 계수기와 추적 기록에 남지 않으면 리포트는 리랭크된 점수라고 착각한다.
func TestRerankFailureIsCountedAndTraced(t *testing.T) {
	ids := traceIDSet(4)
	svc := NewService(traceDocSearcher{ids: ids}, disabledEmbedderForTrace{}).
		WithReranker(traceReranker{failing: true})

	_, trace, err := svc.SearchTraced(context.Background(), model.SearchQuery{Query: "q", Limit: 2, UseRerank: true})
	if err != nil {
		t.Fatalf("SearchTraced: %v", err)
	}
	if !trace.RerankAttempted || !trace.RerankFailed {
		t.Fatalf("failed rerank not traced: %+v", trace)
	}
	attempts, failures := svc.RerankStats()
	if attempts != 1 || failures != 1 {
		t.Fatalf("rerank stats = (%d, %d), want (1, 1)", attempts, failures)
	}

	// 리랭크를 요청하지 않은 질의는 계수기를 건드리지 않는다.
	if _, _, err := svc.SearchTraced(context.Background(), model.SearchQuery{Query: "q", Limit: 2}); err != nil {
		t.Fatalf("SearchTraced: %v", err)
	}
	attempts, failures = svc.RerankStats()
	if attempts != 1 || failures != 1 {
		t.Fatalf("stats moved without a rerank request: (%d, %d)", attempts, failures)
	}
}

// disabledEmbedderForTrace 는 임베딩이 꺼진 배포를 흉내 낸다. 청크 벡터 레인이
// 돌지 않으므로 문서 스토어 레인만 추적에 남는다.
type disabledEmbedderForTrace struct{}

func (disabledEmbedderForTrace) Embed(context.Context, string) ([]float32, error) { return nil, nil }
func (disabledEmbedderForTrace) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	return make([][]float32, len(texts)), nil
}
func (disabledEmbedderForTrace) Enabled() bool  { return false }
func (disabledEmbedderForTrace) Dimension() int { return 0 }
