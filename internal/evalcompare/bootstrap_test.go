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
	m := groupMetric("g", "ndcg10", values, Options{Seed: 1, Iterations: 100, MinN: 3}, false)
	if !m.HasCI {
		t.Fatal("HasCI = false, want true (n >= MinN)")
	}
	if m.CILow != -0.5 || m.CIHigh != -0.5 {
		t.Fatalf("CI = [%v, %v], want [-0.5, -0.5] for a constant delta", m.CILow, m.CIHigh)
	}
	if m.Verdict != VerdictRegressed {
		t.Fatalf("verdict = %s, want regressed", m.Verdict)
	}
}

func TestGroupMetric_BelowMinNIsInconclusive(t *testing.T) {
	values := []float64{10, 10, 10} // a huge, unambiguous improvement...
	m := groupMetric("g", "ndcg10", values, Options{Seed: 1, Iterations: 100, MinN: 20}, false)
	if m.HasCI {
		t.Fatal("HasCI = true below MinN, want false")
	}
	if m.Verdict != VerdictInconclusive {
		t.Fatalf("verdict = %s, want inconclusive regardless of how large the delta looks", m.Verdict)
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
