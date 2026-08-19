package tune

import (
	"context"
	"errors"
	"fmt"

	"github.com/baekenough/second-brain/internal/dataset"
)

// JudgeResult is the entire holdout verdict: two sets of aggregate metrics and
// the sample size behind them.
type JudgeResult struct {
	Baseline       dataset.Metrics
	Candidate      dataset.Metrics
	HoldoutQueries int
}

// GateInput converts the verdict into the promotion gate's input.
func (r JudgeResult) GateInput() GateInput {
	return GateInput{
		BaseNDCG10:     r.Baseline.NDCG10,
		CandNDCG10:     r.Candidate.NDCG10,
		BaseFP10:       r.Baseline.FPPenalty10,
		CandFP10:       r.Candidate.FPPenalty10,
		HoldoutQueries: r.HoldoutQueries,
	}
}

// Judge measures the incumbent and the candidate on the holdout set, in that
// order, and returns both.
//
// This function exists so that there is exactly one place in the codebase where
// the holdout set is touched, and it touches it exactly twice. dataset's budget
// (DefaultHoldoutBudget = 2) makes a third look impossible; keeping the two
// permitted looks together here makes it obvious that a caller cannot smuggle
// in a third by evaluating "just the candidate again".
//
// Because a failed evaluation still spends budget by design (an attempt that
// reached the search path has already observed the holdout set), an error from
// either run ends the tuning run rather than inviting a retry with different
// weights — a retry loop would be indistinguishable from tuning on the holdout
// set.
func Judge(ctx context.Context, h *dataset.HoldoutSet, baseline, candidate dataset.RunFunc) (JudgeResult, error) {
	if h == nil {
		return JudgeResult{}, errors.New("tune: judge requires a holdout set")
	}
	if baseline == nil || candidate == nil {
		return JudgeResult{}, errors.New("tune: judge requires both a baseline and a candidate runner")
	}

	base, err := h.Evaluate(ctx, baseline)
	if err != nil {
		return JudgeResult{}, fmt.Errorf("tune: holdout baseline: %w", err)
	}
	cand, err := h.Evaluate(ctx, candidate)
	if err != nil {
		return JudgeResult{}, fmt.Errorf("tune: holdout candidate: %w", err)
	}

	return JudgeResult{Baseline: base, Candidate: cand, HoldoutQueries: h.Queries()}, nil
}
