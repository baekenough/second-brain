package tune

import "fmt"

// Promotion thresholds (plan §2.4, §2.5). They are exported so that the batch
// command can report them, but they are constants: an operator who wants a
// looser gate has to change this file and re-read the reasoning above it.
const (
	// MinHoldoutQueries is the sample size below which no automatic promotion
	// happens at all. Thirty distinct queries is not a statistically comfortable
	// sample — it is the point below which NDCG differences are visibly
	// dominated by which individual query happened to be labelled.
	MinHoldoutQueries = 30
	// MinNDCGGain is the improvement a candidate must show on the holdout set.
	MinNDCGGain = 0.01
	// MaxFPRegression is how much worse the false-positive penalty may get
	// while still promoting. Surfacing known-bad documents is the failure the
	// user actually notices, so a large NDCG win does not buy the right to it.
	MaxFPRegression = 0.02
)

// gateTolerance absorbs float64 representation error so that a candidate that
// is exactly at a threshold on paper is not rejected because 0.56-0.50 is
// 0.060000000000000005.
const gateTolerance = 1e-9

// Gate reason codes.
//
// These are a closed set on purpose: the code is written to the
// tune.gate_reason span attribute, which is exported to Langfuse. A free-text
// reason would eventually carry a fragment of a user's query into an external
// service by accident. The numbers behind a decision go to Postgres via
// Describe, not to the trace.
const (
	ReasonEligible         = "ok"
	ReasonHoldoutTooSmall  = "holdout_too_small"
	ReasonNDCGGainTooSmall = "ndcg_gain_below_threshold"
	ReasonFPRegression     = "fp_penalty_regression"
)

// Eligible reports whether the holdout sample is large enough for an automatic
// promotion decision to mean anything.
//
// It is a separate exported function rather than an `if` inside the promotion
// path because a lone `if` is exactly the kind of thing that acquires an "…
// || os.Getenv("JUST_THIS_ONCE") != """ six months from now. As its own
// function with its own tests, weakening it is a visible act.
func Eligible(holdoutQueries int) (bool, string) {
	if holdoutQueries < MinHoldoutQueries {
		return false, ReasonHoldoutTooSmall
	}
	return true, ReasonEligible
}

// GateInput is everything the promotion decision is allowed to look at:
// aggregate holdout metrics for the incumbent and the candidate, plus the
// sample size. No query, no document, no per-query detail.
type GateInput struct {
	BaseNDCG10     float64
	CandNDCG10     float64
	BaseFP10       float64
	CandFP10       float64
	HoldoutQueries int
}

// NDCGGain is the holdout NDCG@10 improvement of the candidate over the
// incumbent. Negative means the candidate is worse.
func (in GateInput) NDCGGain() float64 { return in.CandNDCG10 - in.BaseNDCG10 }

// FPDelta is the change in false-positive penalty. Positive means the candidate
// surfaces more known-bad documents than the incumbent.
func (in GateInput) FPDelta() float64 { return in.CandFP10 - in.BaseFP10 }

// PassesPromotion applies the three conditions in order and returns the reason
// code for the first one that fails.
//
// Sample size is checked first so that a spectacular-looking win on twelve
// queries is reported as "too few queries" rather than as a metric verdict —
// the metrics are not trustworthy at that size, and recording a metric reason
// would make the history row read as if the numbers had been believed.
func PassesPromotion(in GateInput) (bool, string) {
	if ok, reason := Eligible(in.HoldoutQueries); !ok {
		return false, reason
	}
	if in.NDCGGain() < MinNDCGGain-gateTolerance {
		return false, ReasonNDCGGainTooSmall
	}
	if in.FPDelta() > MaxFPRegression+gateTolerance {
		return false, ReasonFPRegression
	}
	return true, ReasonEligible
}

// Describe renders a gate outcome as the sentence stored in
// search_weights_history.gate_reason. It is the database-side counterpart of
// the reason code: the code says which rule fired, this says with what numbers,
// so that "why did the weights not move in August" has an answer that does not
// require re-running anything.
func Describe(reason string, in GateInput) string {
	switch reason {
	case ReasonHoldoutTooSmall:
		return fmt.Sprintf("%s: holdout distinct queries %d < %d",
			reason, in.HoldoutQueries, MinHoldoutQueries)
	case ReasonNDCGGainTooSmall:
		return fmt.Sprintf("%s: holdout ndcg@10 gain %+.4f < %+.4f (queries %d)",
			reason, in.NDCGGain(), MinNDCGGain, in.HoldoutQueries)
	case ReasonFPRegression:
		return fmt.Sprintf("%s: holdout fp@10 delta %+.4f > %+.4f (ndcg gain %+.4f, queries %d)",
			reason, in.FPDelta(), MaxFPRegression, in.NDCGGain(), in.HoldoutQueries)
	case ReasonEligible:
		return fmt.Sprintf("%s: holdout ndcg@10 gain %+.4f, fp@10 delta %+.4f, queries %d",
			reason, in.NDCGGain(), in.FPDelta(), in.HoldoutQueries)
	default:
		return reason
	}
}
