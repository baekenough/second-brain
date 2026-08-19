package tune

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/baekenough/second-brain/internal/dataset"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// stubSource serves canned pairs so these tests need neither a database nor a
// search service. The queries are invented strings, never real user input.
type stubSource struct {
	pairs map[string][]store.EvalPair
	err   error
}

func (s *stubSource) EvalPairsBySplit(_ context.Context, split string) ([]store.EvalPair, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.pairs[split], nil
}

// loadStub builds real dataset sets (including the holdout budget) from
// synthetic queries. n queries are generated per split; the split rule is
// bypassed because the stub answers by split name directly, which is exactly
// how store.EvalStore behaves.
func loadStub(t *testing.T, trainN, holdoutN int) (*dataset.TrainSet, *dataset.HoldoutSet) {
	t.Helper()
	mk := func(prefix string, n int) []store.EvalPair {
		out := make([]store.EvalPair, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, store.EvalPair{
				ID:               int64(i + 1),
				Query:            fmt.Sprintf("%s synthetic question %d", prefix, i),
				RelevantDocIDs:   []string{fmt.Sprintf("doc-good-%d", i)},
				IrrelevantDocIDs: []string{fmt.Sprintf("doc-bad-%d", i)},
			})
		}
		return out
	}
	train, holdout, err := dataset.Load(context.Background(), &stubSource{pairs: map[string][]store.EvalPair{
		dataset.SplitTrain:   mk("train", trainN),
		dataset.SplitHoldout: mk("holdout", holdoutN),
	}})
	if err != nil {
		t.Fatalf("dataset.Load: %v", err)
	}
	return train, holdout
}

// metricsFor turns a plain objective value into a Metrics with that objective,
// keeping the false-positive term at zero so Objective(m) == want.
func metricsFor(objective float64) dataset.Metrics {
	return dataset.Metrics{NDCG10: objective, FPPenalty10: 0}
}

// TestCoordinate_FindsKnownOptimum uses a synthetic quadratic whose maximum
// sits exactly on the grid. If coordinate search cannot find a separable
// optimum, it cannot find anything.
func TestCoordinate_FindsKnownOptimum(t *testing.T) {
	t.Parallel()
	train, _ := loadStub(t, 4, 1)
	eval := func(_ context.Context, w model.SearchWeights) (dataset.Metrics, error) {
		obj := -sq(w.FTSWeight-1.5) - sq(w.VecWeight-0.5)
		return metricsFor(obj), nil
	}
	res, err := Coordinate(context.Background(), train, model.SearchWeights{}, DefaultSpace(), eval)
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	if res.Best.FTSWeight != 1.5 {
		t.Errorf("Best.FTSWeight = %v, want 1.5", res.Best.FTSWeight)
	}
	if res.Best.VecWeight != 0.5 {
		t.Errorf("Best.VecWeight = %v, want 0.5", res.Best.VecWeight)
	}
	if res.Evals == 0 {
		t.Error("Evals = 0, want the number of eval calls to be reported")
	}
}

// TestCoordinate_NeverProposesZeroSummaryVec pins plan §2.4: SummaryVec == 0 is
// not "no summary lane", it is "let the coverage gate decide". A tuner that
// emits 0 is reporting a weight it did not actually measure.
func TestCoordinate_NeverProposesZeroSummaryVec(t *testing.T) {
	t.Parallel()
	train, _ := loadStub(t, 4, 1)
	var seen []model.SearchWeights
	eval := func(_ context.Context, w model.SearchWeights) (dataset.Metrics, error) {
		seen = append(seen, w)
		return metricsFor(w.SummaryVec), nil // rewards larger SummaryVec
	}
	res, err := Coordinate(context.Background(), train, model.SearchWeights{}, DefaultSpace(), eval)
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	for i, w := range seen {
		if w.SummaryVec == 0 && !w.DisableSummaryVec {
			t.Fatalf("candidate %d was evaluated with SummaryVec == 0 and DisableSummaryVec == false", i)
		}
		if w.EntityWeight <= 0 {
			t.Fatalf("candidate %d was evaluated with EntityWeight %v; negative/zero means 'explicitly disabled'", i, w.EntityWeight)
		}
	}
	if res.Best.SummaryVec == 0 && !res.Best.DisableSummaryVec {
		t.Fatalf("Best has SummaryVec == 0 with the lane still enabled")
	}
}

// A flat objective offers no improvement, so the search must stop after the
// first sweep rather than grinding through the cap.
func TestCoordinate_TerminatesWithinThreeSweeps(t *testing.T) {
	t.Parallel()
	train, _ := loadStub(t, 4, 1)
	eval := func(_ context.Context, _ model.SearchWeights) (dataset.Metrics, error) {
		return metricsFor(0.42), nil
	}
	res, err := Coordinate(context.Background(), train, model.SearchWeights{}, DefaultSpace(), eval)
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	if res.Sweeps != 1 {
		t.Errorf("Sweeps = %d, want 1 on a flat objective", res.Sweeps)
	}
	if res.Sweeps > maxSweeps {
		t.Errorf("Sweeps = %d, want <= %d", res.Sweeps, maxSweeps)
	}
}

// An objective that improves on every single call would run forever without the
// sweep cap. The cap is what makes the batch job bounded.
func TestCoordinate_StopsAtSweepCap(t *testing.T) {
	t.Parallel()
	train, _ := loadStub(t, 4, 1)
	n := 0.0
	eval := func(_ context.Context, _ model.SearchWeights) (dataset.Metrics, error) {
		n++
		return metricsFor(n), nil
	}
	res, err := Coordinate(context.Background(), train, model.SearchWeights{}, DefaultSpace(), eval)
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	if res.Sweeps != maxSweeps {
		t.Errorf("Sweeps = %d, want the cap %d on an always-improving objective", res.Sweeps, maxSweeps)
	}
}

// Determinism matters because a promotion decision has to be reproducible from
// the history row alone.
func TestCoordinate_IsDeterministic(t *testing.T) {
	t.Parallel()
	train, _ := loadStub(t, 4, 1)
	eval := func(_ context.Context, w model.SearchWeights) (dataset.Metrics, error) {
		// Deliberately full of ties: the tie-break rule is what is under test.
		return metricsFor(float64(int(w.FTSWeight*4+w.VecWeight*4) % 3)), nil
	}
	first, err := Coordinate(context.Background(), train, model.SearchWeights{}, DefaultSpace(), eval)
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	for i := 0; i < 4; i++ {
		got, err := Coordinate(context.Background(), train, model.SearchWeights{}, DefaultSpace(), eval)
		if err != nil {
			t.Fatalf("Coordinate run %d: %v", i, err)
		}
		if got.Best != first.Best || got.BestObjective != first.BestObjective || got.Evals != first.Evals {
			t.Fatalf("run %d differs: %+v vs %+v", i, got, first)
		}
	}
}

func TestCoordinate_PropagatesEvalError(t *testing.T) {
	t.Parallel()
	train, _ := loadStub(t, 4, 1)
	sentinel := errors.New("search unavailable")
	_, err := Coordinate(context.Background(), train, model.SearchWeights{}, DefaultSpace(),
		func(context.Context, model.SearchWeights) (dataset.Metrics, error) {
			return dataset.Metrics{}, sentinel
		})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want it to wrap %v", err, sentinel)
	}
}

// A space that contains 0 for SummaryVec (or a non-positive entity weight) has
// a different meaning than the tuner intends. Refuse it at the door instead of
// silently proposing a configuration nobody measured.
func TestCoordinate_RejectsUnusableSpace(t *testing.T) {
	t.Parallel()
	train, _ := loadStub(t, 4, 1)
	ok := func(context.Context, model.SearchWeights) (dataset.Metrics, error) { return metricsFor(1), nil }

	bad := DefaultSpace()
	bad.SummaryVec = []float64{0, 0.5}
	if _, err := Coordinate(context.Background(), train, model.SearchWeights{}, bad, ok); err == nil {
		t.Error("Coordinate accepted a space containing SummaryVec == 0")
	}

	empty := DefaultSpace()
	empty.FTS = nil
	if _, err := Coordinate(context.Background(), train, model.SearchWeights{}, empty, ok); err == nil {
		t.Error("Coordinate accepted a space with an empty axis")
	}
}

func TestCoordinate_RejectsEmptyTrainSet(t *testing.T) {
	t.Parallel()
	train, _ := loadStub(t, 0, 1)
	_, err := Coordinate(context.Background(), train, model.SearchWeights{}, DefaultSpace(),
		func(context.Context, model.SearchWeights) (dataset.Metrics, error) { return metricsFor(1), nil })
	if err == nil {
		t.Fatal("Coordinate ran on an empty training set; there is nothing to learn from")
	}
}

// DefaultSpace is written out in the plan; drift here silently changes what
// "tuned" means.
func TestDefaultSpace_MatchesPlan(t *testing.T) {
	t.Parallel()
	s := DefaultSpace()
	wantLinear := []float64{0.25, 0.5, 0.75, 1.0, 1.25, 1.5, 1.75, 2.0}
	for name, got := range map[string][]float64{"FTS": s.FTS, "Vec": s.Vec, "Bigm": s.Bigm} {
		if !equalFloats(got, wantLinear) {
			t.Errorf("%s axis = %v, want %v", name, got, wantLinear)
		}
	}
	wantSmall := []float64{0.05, 0.3, 0.55, 0.8, 1.05, 1.3}
	if !equalFloats(s.SummaryVec, wantSmall) {
		t.Errorf("SummaryVec axis = %v, want %v", s.SummaryVec, wantSmall)
	}
	if !equalFloats(s.Entity, wantSmall) {
		t.Errorf("Entity axis = %v, want %v", s.Entity, wantSmall)
	}
	if !equalFloats(s.RRFK, []float64{20, 40, 60, 80, 120}) {
		t.Errorf("RRFK axis = %v", s.RRFK)
	}
}

func sq(x float64) float64 { return x * x }

func equalFloats(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
