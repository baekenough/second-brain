package dataset

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/baekenough/second-brain/internal/store"
)

// stubSource records which splits were asked for and serves canned pairs.
// It stands in for store.EvalStore so these tests need no database.
type stubSource struct {
	asked []string
	pairs map[string][]store.EvalPair
	err   error
}

func (s *stubSource) EvalPairsBySplit(_ context.Context, split string) ([]store.EvalPair, error) {
	s.asked = append(s.asked, split)
	if s.err != nil {
		return nil, s.err
	}
	return s.pairs[split], nil
}

func newStubSource() *stubSource {
	return &stubSource{pairs: map[string][]store.EvalPair{
		SplitTrain: {
			{ID: 1, Query: "train query one", RelevantDocIDs: []string{"d1"}},
			{ID: 2, Query: "train query two", RelevantDocIDs: []string{"d2"}, IrrelevantDocIDs: []string{"d9"}},
		},
		SplitHoldout: {
			{ID: 3, Query: "holdout query one", RelevantDocIDs: []string{"d1"}, IrrelevantDocIDs: []string{"d9"}},
		},
	}}
}

func TestLoad_QueriesEachSplitSeparately(t *testing.T) {
	t.Parallel()
	src := newStubSource()
	if _, _, err := Load(context.Background(), src); err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{SplitTrain, SplitHoldout}
	if len(src.asked) != len(want) {
		t.Fatalf("Source called with %v, want exactly %v", src.asked, want)
	}
	for i := range want {
		if src.asked[i] != want[i] {
			t.Fatalf("Source call %d asked for %q, want %q", i, src.asked[i], want[i])
		}
	}
}

func TestLoad_NoQueryAppearsInBothSets(t *testing.T) {
	t.Parallel()
	src := newStubSource()
	train, _, err := Load(context.Background(), src)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The holdout side is deliberately unreadable, so the cross-check is made
	// against what the stub served for the holdout split.
	holdoutQueries := map[string]bool{}
	for _, p := range src.pairs[SplitHoldout] {
		holdoutQueries[Normalize(p.Query)] = true
	}
	for _, p := range train.Pairs() {
		if holdoutQueries[Normalize(p.Query)] {
			t.Fatalf("query present in both splits: %q", p.Query)
		}
	}
}

func TestLoad_RejectsOverlappingSplits(t *testing.T) {
	t.Parallel()
	src := newStubSource()
	src.pairs[SplitHoldout] = append(src.pairs[SplitHoldout],
		store.EvalPair{ID: 4, Query: " Train Query One ", RelevantDocIDs: []string{"d1"}})
	if _, _, err := Load(context.Background(), src); err == nil {
		t.Fatal("Load accepted a query present in both splits; contamination must be fatal")
	}
}

func TestLoad_PropagatesSourceError(t *testing.T) {
	t.Parallel()
	src := &stubSource{err: errors.New("boom")}
	if _, _, err := Load(context.Background(), src); err == nil {
		t.Fatal("Load swallowed the source error")
	}
}

func constantRun(order []string) RunFunc {
	return func(_ context.Context, _ string, _ int) ([]string, error) { return order, nil }
}

func TestHoldout_EvaluateReturnsScalarsOnly(t *testing.T) {
	t.Parallel()
	_, holdout, err := Load(context.Background(), newStubSource())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Relevant d1 at rank 1 -> NDCG@10 = 1.0, MRR@10 = 1.0.
	// Irrelevant d9 inside the top-10 -> FP penalty = 1/10 = 0.1.
	m, err := holdout.Evaluate(context.Background(), constantRun([]string{"d1", "d9"}))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if math.Abs(m.NDCG10-1.0) > 1e-9 {
		t.Errorf("NDCG10 = %v, want 1.0", m.NDCG10)
	}
	if math.Abs(m.MRR10-1.0) > 1e-9 {
		t.Errorf("MRR10 = %v, want 1.0", m.MRR10)
	}
	if math.Abs(m.FPPenalty10-0.1) > 1e-9 {
		t.Errorf("FPPenalty10 = %v, want 0.1", m.FPPenalty10)
	}
	if math.Abs(m.Objective-(1.0-0.5*0.1)) > 1e-9 {
		t.Errorf("Objective = %v, want %v", m.Objective, 1.0-0.5*0.1)
	}
	if m.Queries != 1 || m.Pairs != 1 {
		t.Errorf("Queries/Pairs = %d/%d, want 1/1", m.Queries, m.Pairs)
	}
}

func TestHoldout_BudgetExhausted(t *testing.T) {
	t.Parallel()
	_, holdout, err := Load(context.Background(), newStubSource())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	run := constantRun([]string{"d1"})
	for i := 0; i < DefaultHoldoutBudget; i++ {
		if _, err := holdout.Evaluate(context.Background(), run); err != nil {
			t.Fatalf("Evaluate %d: %v", i, err)
		}
	}
	if _, err := holdout.Evaluate(context.Background(), run); !errors.Is(err, ErrHoldoutBudgetExhausted) {
		t.Fatalf("third Evaluate error = %v, want ErrHoldoutBudgetExhausted", err)
	}
}

func TestHoldout_EvaluatePropagatesRunError(t *testing.T) {
	t.Parallel()
	_, holdout, err := Load(context.Background(), newStubSource())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	boom := errors.New("search down")
	_, err = holdout.Evaluate(context.Background(), func(context.Context, string, int) ([]string, error) {
		return nil, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Evaluate error = %v, want %v", err, boom)
	}
}

func TestTrainSet_PairsIsACopy(t *testing.T) {
	t.Parallel()
	train, _, err := Load(context.Background(), newStubSource())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := train.Pairs()
	if len(got) == 0 {
		t.Fatal("TrainSet.Pairs is empty")
	}
	got[0].Query = "mutated"
	if train.Pairs()[0].Query == "mutated" {
		t.Fatal("TrainSet.Pairs exposes its backing array")
	}
	if train.Queries() != 2 {
		t.Fatalf("TrainSet.Queries = %d, want 2", train.Queries())
	}
}
