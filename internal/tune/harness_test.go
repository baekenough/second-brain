package tune

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/baekenough/second-brain/internal/dataset"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

type stubSearcher struct {
	calls   []model.SearchQuery
	results []*model.SearchResult
	err     error
}

func (s *stubSearcher) Search(_ context.Context, q model.SearchQuery) ([]*model.SearchResult, error) {
	s.calls = append(s.calls, q)
	if s.err != nil {
		return nil, s.err
	}
	return s.results, nil
}

func resultWithID(id uuid.UUID) *model.SearchResult {
	r := &model.SearchResult{}
	r.ID = id
	return r
}

// TestRunnerFor_AppliesWeightsPerCandidate is the contamination guard. Two
// candidates evaluated in sequence must reach the searcher with their own
// weights; if the second run inherited the first candidate's weights the whole
// comparison would be measuring the wrong thing while looking perfectly healthy.
func TestRunnerFor_AppliesWeightsPerCandidate(t *testing.T) {
	t.Parallel()
	stub := &stubSearcher{}
	a := model.SearchWeights{FTSWeight: 0.25, VecWeight: 2.0, BigmWeight: 1.0, SummaryVec: 0.05, EntityWeight: 0.05, RRFK: 20}
	b := model.SearchWeights{FTSWeight: 2.0, VecWeight: 0.25, BigmWeight: 1.0, SummaryVec: 1.5, EntityWeight: 1.5, RRFK: 120}

	for _, w := range []model.SearchWeights{a, b, a} {
		if _, err := RunnerFor(stub, w)(context.Background(), "dummy query", 10); err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if len(stub.calls) != 3 {
		t.Fatalf("searcher called %d times, want 3", len(stub.calls))
	}
	want := []model.SearchWeights{a.Defaults(), b.Defaults(), a.Defaults()}
	for i, w := range want {
		if stub.calls[i].Weights != w {
			t.Errorf("call %d weights = %+v, want %+v", i, stub.calls[i].Weights, w)
		}
	}
}

// TestRunnerFor_ForcesRerankAndHyDEOff pins reproducibility. HyDE calls a remote
// LLM, so the same query would return different candidates on every sweep and
// each evaluation would cost money; the reranker is an HTTP service the batch
// host is not guaranteed to reach at all.
func TestRunnerFor_ForcesRerankAndHyDEOff(t *testing.T) {
	t.Parallel()
	stub := &stubSearcher{}
	w := model.SearchWeights{FTSWeight: 1, VecWeight: 1, BigmWeight: 1, SummaryVec: 0.8, EntityWeight: 0.5, RRFK: 60}
	if _, err := RunnerFor(stub, w)(context.Background(), "dummy query", 10); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := stub.calls[0]
	if got.UseRerank {
		t.Error("UseRerank is true; the batch host cannot be assumed to reach the reranker")
	}
	if got.UseHyDE {
		t.Error("UseHyDE is true; evaluation would stop being reproducible")
	}
	if got.Limit != 10 {
		t.Errorf("Limit = %d, want the requested k (10)", got.Limit)
	}
	if got.Query != "dummy query" {
		t.Errorf("Query = %q, want it passed through unchanged", got.Query)
	}
}

func TestRunnerFor_ReturnsDocumentIDsInRankOrder(t *testing.T) {
	t.Parallel()
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	stub := &stubSearcher{results: []*model.SearchResult{
		resultWithID(ids[0]), resultWithID(ids[1]), resultWithID(ids[2]),
	}}
	got, err := RunnerFor(stub, model.SearchWeights{})(context.Background(), "dummy query", 10)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d ids, want 3", len(got))
	}
	for i, id := range ids {
		if got[i] != id.String() {
			t.Errorf("position %d = %s, want %s", i, got[i], id)
		}
	}
}

func TestRunnerFor_TruncatesToK(t *testing.T) {
	t.Parallel()
	stub := &stubSearcher{}
	for i := 0; i < 5; i++ {
		stub.results = append(stub.results, resultWithID(uuid.New()))
	}
	got, err := RunnerFor(stub, model.SearchWeights{})(context.Background(), "dummy query", 3)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d ids, want 3", len(got))
	}
}

// TestRunnerFor_PropagatesSearchError keeps a broken database from being scored
// as a bad set of weights.
func TestRunnerFor_PropagatesSearchError(t *testing.T) {
	t.Parallel()
	boom := errors.New("db down")
	_, err := RunnerFor(&stubSearcher{err: boom}, model.SearchWeights{})(context.Background(), "dummy query", 10)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

func TestObjective_PenalisesFalsePositives(t *testing.T) {
	t.Parallel()
	prev := Objective(dataset.Metrics{NDCG10: 0.8, FPPenalty10: 0})
	for _, fp := range []float64{0.1, 0.2, 0.4, 0.8} {
		got := Objective(dataset.Metrics{NDCG10: 0.8, FPPenalty10: fp})
		if got >= prev {
			t.Fatalf("objective did not decrease as FP grew: fp=%v gave %v, previous %v", fp, got, prev)
		}
		prev = got
	}
	if got, want := Objective(dataset.Metrics{NDCG10: 0.8, FPPenalty10: 0.2}), 0.8-0.5*0.2; math.Abs(got-want) > 1e-12 {
		t.Errorf("Objective = %v, want %v", got, want)
	}
}

// TestObjective_MatchesDatasetDefinition keeps the tuner and the dataset
// package from drifting into two different notions of "better".
func TestObjective_MatchesDatasetDefinition(t *testing.T) {
	t.Parallel()
	m := dataset.Metrics{NDCG10: 0.61, FPPenalty10: 0.13}
	if got, want := Objective(m), dataset.ObjectiveOf(m.NDCG10, m.FPPenalty10); got != want {
		t.Fatalf("Objective = %v, dataset.ObjectiveOf = %v", got, want)
	}
}
