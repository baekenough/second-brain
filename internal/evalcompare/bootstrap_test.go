package evalcompare

import "testing"

func TestSubSeed_DeterministicAndDistinct(t *testing.T) {
	a := subSeed(1, "overall", "ndcg10", "s1")
	b := subSeed(1, "overall", "ndcg10", "s1")
	if a != b {
		t.Fatalf("subSeed not deterministic: %d != %d", a, b)
	}
	c := subSeed(1, "overall", "ndcg10", "s2")
	if a == c {
		t.Fatal("s1/s2 sub-seeds for the same group/metric must differ (PCG needs two distinct seed words)")
	}
	d := subSeed(1, "answer_source:call", "ndcg10", "s1")
	if a == d {
		t.Fatal("different group names must not collide to the same seed")
	}
	e := subSeed(2, "overall", "ndcg10", "s1")
	if a == e {
		t.Fatal("different base seeds must not collide")
	}
}

func TestGroupMetric_ConstantDeltaCollapsesCI(t *testing.T) {
	values := []float64{-0.5, -0.5, -0.5, -0.5, -0.5}
	m := groupMetric("g", "ndcg10", values, Options{Seed: 1, Iterations: 100, MinN: 3}, false, 0)
	if !m.HasCI {
		t.Fatal("HasCI = false, want true (n >= MinN)")
	}
	if m.CILow != -0.5 || m.CIHigh != -0.5 {
		t.Fatalf("CI = [%v, %v], want [-0.5, -0.5] for a constant delta", m.CILow, m.CIHigh)
	}
	if m.Verdict != VerdictRegressed {
		t.Fatalf("verdict = %s, want regressed", m.Verdict)
	}
	if m.BonferroniSignificant != nil {
		t.Fatalf("BonferroniSignificant = %v, want nil for bonferroniM=0 (a gating/overall call)", *m.BonferroniSignificant)
	}
}

func TestGroupMetric_BelowMinNIsInconclusive(t *testing.T) {
	values := []float64{10, 10, 10} // a huge, unambiguous improvement...
	m := groupMetric("g", "ndcg10", values, Options{Seed: 1, Iterations: 100, MinN: 20}, false, 0)
	if m.HasCI {
		t.Fatal("HasCI = true below MinN, want false")
	}
	if m.Verdict != VerdictInconclusive {
		t.Fatalf("verdict = %s, want inconclusive regardless of how large the delta looks", m.Verdict)
	}
}

// 완료 기준 (HIGH #269): bonferroniM > 0 populates BonferroniSignificant
// from the same resampled distribution, and a constant delta (zero
// bootstrap variance) survives correction regardless of how many groups it
// is corrected against.
func TestGroupMetric_BonferroniSignificantPopulatedOnlyForDiagnosticGroups(t *testing.T) {
	values := []float64{-0.5, -0.5, -0.5, -0.5, -0.5}
	m := groupMetric("g", "ndcg10", values, Options{Seed: 1, Iterations: 100, MinN: 3}, false, 8)
	if m.BonferroniSignificant == nil {
		t.Fatal("BonferroniSignificant = nil, want non-nil for bonferroniM=8 with HasCI=true")
	}
	if !*m.BonferroniSignificant {
		t.Fatal("BonferroniSignificant = false, want true for a constant, zero-variance delta")
	}

	inconclusive := groupMetric("g", "ndcg10", []float64{10, 10, 10}, Options{Seed: 1, Iterations: 100, MinN: 20}, false, 8)
	if inconclusive.BonferroniSignificant != nil {
		t.Fatal("BonferroniSignificant must stay nil when HasCI is false (n < MinN) — nothing to correct")
	}
}

// 완료 기준 (LOW #269): percentile uses linear interpolation (NumPy's
// "linear"/R-7 estimator), not the old truncated nearest-rank indices,
// which made the CI narrower than the nominal 95% at low iteration counts.
// Expected values below are computed by hand for sorted[i] = i+1:
// rank = p*(n-1), interpolating between the two bracketing integers.
func TestPercentile(t *testing.T) {
	sorted40 := make([]float64, 40)
	for i := range sorted40 {
		sorted40[i] = float64(i + 1) // 1..40
	}
	sorted100 := make([]float64, 100)
	for i := range sorted100 {
		sorted100[i] = float64(i + 1) // 1..100
	}

	cases := []struct {
		name   string
		sorted []float64
		p      float64
		want   float64
	}{
		{"n=40 p=0.025", sorted40, 0.025, 1.975},    // rank=0.975 between 1 and 2
		{"n=40 p=0.975", sorted40, 0.975, 39.025},   // rank=38.025 between 39 and 40
		{"n=100 p=0.025", sorted100, 0.025, 3.475},  // rank=2.475 between 3 and 4
		{"n=100 p=0.975", sorted100, 0.975, 97.525}, // rank=96.525 between 97 and 98
		{"n=40 p=0", sorted40, 0, 1},
		{"n=40 p=1", sorted40, 1, 40},
		{"n=0", nil, 0.5, 0},
		{"n=1", []float64{7}, 0.5, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := percentile(tc.sorted, tc.p)
			if diff := got - tc.want; diff > 1e-9 || diff < -1e-9 {
				t.Fatalf("percentile(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestMedian(t *testing.T) {
	if got := median(nil); got != 0 {
		t.Fatalf("median(nil) = %v, want 0", got)
	}
	if got := median([]float64{1, 3, 2}); got != 2 {
		t.Fatalf("median(odd) = %v, want 2", got)
	}
	if got := median([]float64{1, 2, 3, 4}); got != 2.5 {
		t.Fatalf("median(even) = %v, want 2.5", got)
	}
}

func TestWorstByNDCG_TopThreeSortedAscendingWithTieBreak(t *testing.T) {
	dNeg1, dNeg2, dZero := -0.9, -0.9, 0.0
	byID := map[string]queryDelta{
		"b": {ID: "b", NDCG10: &dNeg1},
		"a": {ID: "a", NDCG10: &dNeg2}, // tie with b, but "a" < "b"
		"c": {ID: "c", NDCG10: &dZero},
		"d": {ID: "d", NDCG10: nil}, // must be excluded: nothing to score
	}
	got := worstByNDCG([]string{"a", "b", "c", "d"}, byID)
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("worstByNDCG = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("worstByNDCG[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}
