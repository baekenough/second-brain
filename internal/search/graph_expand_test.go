package search

import (
	"context"
	"errors"
	"math"
	"sort"
	"sync"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// 결과 시드 그래프 확장(GraphExpandBoost)의 승수 계산과 서비스 배선 검사.
// 실DB 지지 집계는 internal/store/graph_expand_db_test.go.

func TestGraphSupportStrength_BoundedMonotone(t *testing.T) {
	t.Parallel()
	for _, maxN := range []int{1, 2, 7, 100} {
		prev := 0.0
		for n := 0; n <= maxN+3; n++ {
			s := graphSupportStrength(n, maxN)
			if s < 0 || s > 1 || math.IsNaN(s) {
				t.Fatalf("strength(%d,%d) = %v, want [0,1]", n, maxN, s)
			}
			if s < prev {
				t.Fatalf("단조가 아니다: strength(%d,%d)=%v < %v", n, maxN, s, prev)
			}
			prev = s
		}
		if graphSupportStrength(maxN, maxN) != 1 || graphSupportStrength(0, maxN) != 0 {
			t.Errorf("maxN=%d: 끝점이 0/1 이 아니다", maxN)
		}
	}
	if graphSupportStrength(3, 0) != 0 {
		t.Error("maxN=0 이면 0 이어야 한다")
	}
}

func boostFixture() ([]*model.SearchResult, []uuid.UUID) {
	ids := make([]uuid.UUID, 5)
	results := make([]*model.SearchResult, 5)
	for i := range ids {
		ids[i] = uuid.New()
		// 0.050, 0.049, ... 서로 1/50 차이: 작은 boost 로는 순서가 안 바뀐다.
		results[i] = makeSearchResult(ids[i], "r", 0.050-float64(i)*0.001)
	}
	return results, ids
}

func TestBoostByGraphSupport_BoundedAndReorderOnly(t *testing.T) {
	t.Parallel()
	results, ids := boostFixture()
	before := map[uuid.UUID]float64{}
	for _, r := range results {
		before[r.ID] = r.Score
	}
	outsider := uuid.New() // 후보가 아닌 지지 문서: 무시돼야 한다
	support := map[uuid.UUID]int{ids[4]: 6, ids[2]: 1, outsider: 50}

	const boost = 0.5
	n := boostByGraphSupport(results, support, boost)
	if n != 2 {
		t.Fatalf("boosted = %d, want 2", n)
	}
	if len(results) != len(ids) {
		t.Fatalf("길이가 바뀌었다: %d", len(results))
	}
	seen := map[uuid.UUID]bool{}
	for _, r := range results {
		seen[r.ID] = true
		if r.ID == outsider {
			t.Fatal("후보가 아닌 문서가 결과에 들어왔다")
		}
		ratio := r.Score / before[r.ID]
		if ratio < 1 || ratio > 1+boost+1e-12 {
			t.Errorf("승수 %v 가 [1, 1+boost] 밖이다", ratio)
		}
	}
	if len(seen) != len(ids) {
		t.Error("원소 집합이 바뀌었다")
	}
	// maxN 이 outsider(50)가 아니라 후보 안의 최대(6)로 정해져 ids[4] 가 최대 승수를 받는다.
	if got := results[0].ID; got != ids[4] {
		t.Errorf("가장 많이 지지받은 꼴찌가 1위로 올라가지 않았다")
	}
	if math.Abs(results[0].Score-before[ids[4]]*(1+boost)) > 1e-15 {
		t.Errorf("최대 지지 승수 = %v, want %v", results[0].Score/before[ids[4]], 1+boost)
	}
	if !sort.SliceIsSorted(results, func(i, j int) bool { return resultBefore(results[i], results[j]) }) {
		t.Error("sortByScore 전순서로 정렬되지 않았다")
	}
}

func TestBoostByGraphSupport_Monotone(t *testing.T) {
	t.Parallel()
	// 같은 점수의 두 후보: 지지가 더 많은 쪽이 앞선다.
	a, b := uuid.New(), uuid.New()
	results := []*model.SearchResult{makeSearchResult(a, "a", 0.02), makeSearchResult(b, "b", 0.02)}
	boostByGraphSupport(results, map[uuid.UUID]int{a: 1, b: 3}, 0.3)
	if results[0].ID != b || results[0].Score <= results[1].Score {
		t.Error("지지가 많은 후보가 앞서지 않았다")
	}
}

func TestBoostByGraphSupport_NoOps(t *testing.T) {
	t.Parallel()
	results, ids := boostFixture()
	snapshot := func() []float64 {
		out := make([]float64, len(results))
		for i, r := range results {
			out[i] = r.Score
		}
		return out
	}
	base := snapshot()
	for name, tc := range map[string]struct {
		support map[uuid.UUID]int
		boost   float64
	}{
		"boost 0":   {map[uuid.UUID]int{ids[4]: 2}, 0},
		"boost NaN": {map[uuid.UUID]int{ids[4]: 2}, math.NaN()},
		"지지 없음":     {nil, 0.5},
		"후보 밖 지지만":  {map[uuid.UUID]int{uuid.New(): 9}, 0.5},
		"boost 음수":  {map[uuid.UUID]int{ids[4]: 2}, -1},
		"boost 무한대": {map[uuid.UUID]int{ids[4]: 2}, math.Inf(1)},
		"지지 0 인 후보": {map[uuid.UUID]int{ids[4]: 0}, 0.5},
	} {
		if n := boostByGraphSupport(results, tc.support, tc.boost); n != 0 {
			t.Errorf("%s: boosted=%d", name, n)
		}
		for i, s := range snapshot() {
			if s != base[i] || results[i].ID != ids[i] {
				t.Fatalf("%s: 결과가 바뀌었다", name)
			}
		}
	}

	// 0 이하·NaN 점수는 곱하지 않는다.
	zero := makeSearchResult(uuid.New(), "z", 0)
	nan := makeSearchResult(uuid.New(), "n", math.NaN())
	boostByGraphSupport([]*model.SearchResult{zero, nan}, map[uuid.UUID]int{zero.ID: 1, nan.ID: 1}, 1)
	if zero.Score != 0 || !math.IsNaN(nan.Score) {
		t.Error("0/NaN 점수가 바뀌었다")
	}

	// boost 가 1 을 넘어도 승수는 2 를 넘지 않는다.
	one := makeSearchResult(uuid.New(), "o", 0.01)
	boostByGraphSupport([]*model.SearchResult{one}, map[uuid.UUID]int{one.ID: 4}, 5)
	if one.Score > 0.02+1e-15 {
		t.Errorf("승수가 상한 2 를 넘었다: %v", one.Score/0.01)
	}
}

// graphSupportDocSearcher 는 GraphSupportCounter 를 만족하는 문서 저장소 더블이다.
type graphSupportDocSearcher struct {
	sparseDocSearcher
	mu      sync.Mutex
	calls   int
	seeds   []uuid.UUID
	cands   []uuid.UUID
	q       model.SearchQuery
	support map[uuid.UUID]int
	err     error
}

func (g *graphSupportDocSearcher) GraphSupportCounts(_ context.Context, q model.SearchQuery, seeds, cands []uuid.UUID) (map[uuid.UUID]int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	g.q, g.seeds, g.cands = q, append([]uuid.UUID(nil), seeds...), append([]uuid.UUID(nil), cands...)
	return g.support, g.err
}

func graphExpandResults(n int) []*model.SearchResult {
	out := make([]*model.SearchResult, n)
	for i := range out {
		out[i] = makeSearchResult(uuid.New(), "r", 1-float64(i)*0.01)
	}
	return out
}

func TestSearch_GraphExpandBoost_Wiring(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("off: 저장소를 부르지 않는다", func(t *testing.T) {
		t.Parallel()
		docs := &graphSupportDocSearcher{sparseDocSearcher: sparseDocSearcher{results: graphExpandResults(3)}}
		if _, err := NewService(docs, disabledEmbedder{}).Search(ctx, model.SearchQuery{Query: "q", Limit: 10}); err != nil {
			t.Fatal(err)
		}
		if docs.calls != 0 {
			t.Errorf("노브가 꺼졌는데 GraphSupportCounts 를 %d회 불렀다", docs.calls)
		}
	})

	t.Run("on: 상위 10건 시드·전체 후보·재정렬", func(t *testing.T) {
		t.Parallel()
		results := graphExpandResults(15)
		last := results[14].ID
		docs := &graphSupportDocSearcher{
			sparseDocSearcher: sparseDocSearcher{results: results},
			support:           map[uuid.UUID]int{last: 2, uuid.New(): 7},
		}
		svc := NewService(docs, disabledEmbedder{}).WithTuning(model.SearchTuning{GraphExpandBoost: 1})
		got, err := svc.Search(ctx, model.SearchQuery{Query: "q", Limit: 15})
		if err != nil {
			t.Fatal(err)
		}
		if docs.calls != 1 || len(docs.seeds) != store.MaxGraphExpandSeedDocs || len(docs.cands) != 15 {
			t.Fatalf("calls=%d seeds=%d cands=%d", docs.calls, len(docs.seeds), len(docs.cands))
		}
		for i := range docs.seeds {
			if docs.seeds[i] != docs.cands[i] {
				t.Fatal("시드가 융합 순서의 상위 문서가 아니다")
			}
		}
		if len(got) != 15 {
			t.Fatalf("결과 %d건: 새 문서가 들어왔거나 빠졌다", len(got))
		}
		// 0.86 * 2 = 1.72 > 1.0 → 꼴찌가 1위.
		if got[0].ID != last {
			t.Error("지지받은 후보가 올라가지 않았다")
		}
	})

	t.Run("최신순 질의·저장소 실패는 원 순서", func(t *testing.T) {
		t.Parallel()
		for name, tc := range map[string]struct {
			q   model.SearchQuery
			err error
		}{
			"recent": {model.SearchQuery{Query: "q", Limit: 10, Sort: "recent"}, nil},
			"error":  {model.SearchQuery{Query: "q", Limit: 10}, errors.New("boom")},
		} {
			results := graphExpandResults(4)
			order := []uuid.UUID{results[0].ID, results[1].ID, results[2].ID, results[3].ID}
			docs := &graphSupportDocSearcher{
				sparseDocSearcher: sparseDocSearcher{results: results},
				support:           map[uuid.UUID]int{results[3].ID: 5},
				err:               tc.err,
			}
			svc := NewService(docs, disabledEmbedder{}).WithTuning(model.SearchTuning{GraphExpandBoost: 1})
			got, err := svc.Search(ctx, tc.q)
			if err != nil {
				t.Fatalf("%s: 검색이 실패했다: %v", name, err)
			}
			for i, r := range got {
				if r.ID != order[i] {
					t.Fatalf("%s: 순서가 바뀌었다", name)
				}
			}
		}
	})
}
