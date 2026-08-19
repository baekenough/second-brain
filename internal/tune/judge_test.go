package tune

import (
	"context"
	"errors"
	"testing"

	"github.com/baekenough/second-brain/internal/dataset"
)

// runFor returns a RunFunc that answers every query with the same document
// list. The point of these tests is the holdout budget, not retrieval quality.
func runFor(docIDs ...string) dataset.RunFunc {
	return func(context.Context, string, int) ([]string, error) { return docIDs, nil }
}

// TestJudge_SpendsExactlyTwoHoldoutEvaluations is the isolation test that
// matters most: the holdout set may be measured twice per tuning run — once for
// the incumbent, once for the candidate — and a third look must be impossible,
// not merely discouraged.
func TestJudge_SpendsExactlyTwoHoldoutEvaluations(t *testing.T) {
	t.Parallel()
	_, holdout := loadStub(t, 4, 3)

	if _, err := Judge(context.Background(), holdout, runFor("doc-good-0"), runFor("doc-good-0")); err != nil {
		t.Fatalf("first Judge: %v", err)
	}
	_, err := Judge(context.Background(), holdout, runFor("doc-good-0"), runFor("doc-good-0"))
	if !errors.Is(err, dataset.ErrHoldoutBudgetExhausted) {
		t.Fatalf("second Judge err = %v, want dataset.ErrHoldoutBudgetExhausted", err)
	}
}

// A failed candidate measurement still consumed a look at the holdout set, so
// the run must stop rather than retry with different weights.
func TestJudge_FailedCandidateStillEndsTheRun(t *testing.T) {
	t.Parallel()
	_, holdout := loadStub(t, 4, 3)
	boom := errors.New("search unavailable")
	failing := func(context.Context, string, int) ([]string, error) { return nil, boom }

	if _, err := Judge(context.Background(), holdout, runFor("doc-good-0"), failing); err == nil {
		t.Fatal("Judge returned no error for a failing candidate run")
	}
	_, err := Judge(context.Background(), holdout, runFor("doc-good-0"), runFor("doc-good-0"))
	if !errors.Is(err, dataset.ErrHoldoutBudgetExhausted) {
		t.Fatalf("after a failed run the budget must still be spent; err = %v", err)
	}
}

func TestJudge_ReportsBaselineAndCandidateSeparately(t *testing.T) {
	t.Parallel()
	_, holdout := loadStub(t, 4, 3)

	// Baseline retrieves the known-bad document, candidate the relevant one.
	res, err := Judge(context.Background(), holdout,
		func(_ context.Context, _ string, _ int) ([]string, error) { return []string{"doc-bad-0"}, nil },
		func(_ context.Context, _ string, _ int) ([]string, error) { return []string{"doc-good-0"}, nil })
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if res.Candidate.NDCG10 <= res.Baseline.NDCG10 {
		t.Errorf("candidate NDCG %v not above baseline %v", res.Candidate.NDCG10, res.Baseline.NDCG10)
	}
	if res.HoldoutQueries != 3 {
		t.Errorf("HoldoutQueries = %d, want 3", res.HoldoutQueries)
	}
}

func TestJudge_RejectsNilHoldout(t *testing.T) {
	t.Parallel()
	if _, err := Judge(context.Background(), nil, runFor(), runFor()); err == nil {
		t.Fatal("Judge accepted a nil holdout set")
	}
}
