package tune

import (
	"strings"
	"testing"
)

// The promotion gate is the one place where "this candidate looks better" turns
// into "the system changes behaviour". Every test here is about refusing to let
// that happen on thin evidence.

func TestEligible_Under30Rejects(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1, 29} {
		ok, reason := Eligible(n)
		if ok {
			t.Errorf("Eligible(%d) = true, want false: promotion must not be possible on %d holdout queries", n, n)
		}
		if reason != ReasonHoldoutTooSmall {
			t.Errorf("Eligible(%d) reason = %q, want %q", n, reason, ReasonHoldoutTooSmall)
		}
	}
}

func TestEligible_At30Accepts(t *testing.T) {
	t.Parallel()
	for _, n := range []int{30, 31, 500} {
		ok, reason := Eligible(n)
		if !ok {
			t.Errorf("Eligible(%d) = false, want true", n)
		}
		if reason != ReasonEligible {
			t.Errorf("Eligible(%d) reason = %q, want %q", n, reason, ReasonEligible)
		}
	}
}

// A gain of +0.005 is inside the noise of a 30-query sample. Accepting it would
// mean promoting on measurement jitter.
func TestPassesPromotion_RejectsSmallGain(t *testing.T) {
	t.Parallel()
	ok, reason := PassesPromotion(GateInput{
		BaseNDCG10: 0.500, CandNDCG10: 0.505,
		BaseFP10: 0.10, CandFP10: 0.10,
		HoldoutQueries: 40,
	})
	if ok {
		t.Fatal("PassesPromotion accepted a +0.005 NDCG gain; the threshold is +0.01")
	}
	if reason != ReasonNDCGGainTooSmall {
		t.Fatalf("reason = %q, want %q", reason, ReasonNDCGGainTooSmall)
	}
}

// A large NDCG win does not buy the right to surface more known-bad documents:
// false positives are what the user actually complains about.
func TestPassesPromotion_RejectsFPRegression(t *testing.T) {
	t.Parallel()
	ok, reason := PassesPromotion(GateInput{
		BaseNDCG10: 0.500, CandNDCG10: 0.700,
		BaseFP10: 0.10, CandFP10: 0.13,
		HoldoutQueries: 40,
	})
	if ok {
		t.Fatal("PassesPromotion accepted a +0.03 false-positive regression; the tolerance is +0.02")
	}
	if reason != ReasonFPRegression {
		t.Fatalf("reason = %q, want %q", reason, ReasonFPRegression)
	}
}

func TestPassesPromotion_AcceptsClearWin(t *testing.T) {
	t.Parallel()
	ok, reason := PassesPromotion(GateInput{
		BaseNDCG10: 0.500, CandNDCG10: 0.560,
		BaseFP10: 0.10, CandFP10: 0.09,
		HoldoutQueries: 30,
	})
	if !ok {
		t.Fatalf("PassesPromotion rejected a clear win: %s", reason)
	}
	if reason != ReasonEligible {
		t.Fatalf("reason = %q, want %q", reason, ReasonEligible)
	}
}

// The sample-size gate is checked before the metric gates: a big win measured on
// 12 queries must be reported as "too few queries", not as a metric verdict,
// because the metrics are not trustworthy at that size.
func TestPassesPromotion_SampleSizeGateWinsOverMetrics(t *testing.T) {
	t.Parallel()
	ok, reason := PassesPromotion(GateInput{
		BaseNDCG10: 0.400, CandNDCG10: 0.900,
		BaseFP10: 0.20, CandFP10: 0.00,
		HoldoutQueries: 12,
	})
	if ok {
		t.Fatal("PassesPromotion accepted a candidate measured on 12 holdout queries")
	}
	if reason != ReasonHoldoutTooSmall {
		t.Fatalf("reason = %q, want %q", reason, ReasonHoldoutTooSmall)
	}
}

// Every rejection has to say why: the string is written to
// search_weights_history.gate_reason, which is the only record of why the
// weights did not move.
func TestPassesPromotion_ReasonIsNonEmptyOnReject(t *testing.T) {
	t.Parallel()
	cases := []GateInput{
		{HoldoutQueries: 5, CandNDCG10: 0.9, BaseNDCG10: 0.1},
		{HoldoutQueries: 40, BaseNDCG10: 0.5, CandNDCG10: 0.5},
		{HoldoutQueries: 40, BaseNDCG10: 0.5, CandNDCG10: 0.9, BaseFP10: 0.0, CandFP10: 0.5},
	}
	for i, in := range cases {
		ok, reason := PassesPromotion(in)
		if ok {
			t.Fatalf("case %d unexpectedly passed", i)
		}
		if strings.TrimSpace(reason) == "" {
			t.Fatalf("case %d rejected with an empty reason", i)
		}
	}
}

// Describe produces the human sentence stored in the database. The enum code
// alone goes to the trace; the numbers stay in Postgres.
func TestDescribe_CarriesTheNumbers(t *testing.T) {
	t.Parallel()
	got := Describe(ReasonHoldoutTooSmall, GateInput{HoldoutQueries: 12})
	if !strings.Contains(got, "12") || !strings.Contains(got, "30") {
		t.Fatalf("Describe = %q, want it to name both the sample size and the threshold", got)
	}
	if !strings.Contains(got, ReasonHoldoutTooSmall) {
		t.Fatalf("Describe = %q, want it to carry the reason code %q", got, ReasonHoldoutTooSmall)
	}
}
