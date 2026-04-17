package search_test

import (
	"math"
	"testing"

	"github.com/baekenough/second-brain/internal/search"
)

const floatTol = 1e-9

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < floatTol
}

// ---------------------------------------------------------------------------
// NDCGK tests
// ---------------------------------------------------------------------------

func TestNDCGK_PerfectRanking(t *testing.T) {
	t.Parallel()

	// All relevant docs at the top → NDCG = 1.0
	results := []string{"a", "b", "c", "d"}
	relevant := map[string]bool{"a": true, "b": true, "c": true}

	got := search.NDCGK(results, relevant, 3)
	if !almostEqual(got, 1.0) {
		t.Fatalf("perfect ranking: want 1.0, got %f", got)
	}
}

func TestNDCGK_ReverseRanking(t *testing.T) {
	t.Parallel()

	// Relevant doc at the bottom → score lower than perfect.
	results := []string{"x", "y", "a"}
	relevant := map[string]bool{"a": true}

	got := search.NDCGK(results, relevant, 3)
	if got >= 1.0 || got <= 0 {
		t.Fatalf("reverse ranking: want 0 < score < 1.0, got %f", got)
	}

	// DCG = 1/log2(4) ≈ 0.5, IDCG = 1/log2(2) = 1.0 → NDCG ≈ 0.5
	want := 1.0 / math.Log2(4)
	if !almostEqual(got, want) {
		t.Fatalf("reverse ranking: want %f, got %f", want, got)
	}
}

func TestNDCGK_EmptyRelevant(t *testing.T) {
	t.Parallel()

	results := []string{"a", "b", "c"}
	got := search.NDCGK(results, map[string]bool{}, 3)
	if got != 0 {
		t.Fatalf("empty relevant: want 0, got %f", got)
	}
}

func TestNDCGK_NilRelevant(t *testing.T) {
	t.Parallel()

	results := []string{"a", "b"}
	got := search.NDCGK(results, nil, 5)
	if got != 0 {
		t.Fatalf("nil relevant: want 0, got %f", got)
	}
}

func TestNDCGK_KLargerThanResults(t *testing.T) {
	t.Parallel()

	// k exceeds the result list length; should evaluate over all results.
	results := []string{"a", "b"}
	relevant := map[string]bool{"a": true}

	got := search.NDCGK(results, relevant, 100)
	// Only "a" is relevant and it is first → NDCG = 1.0
	if !almostEqual(got, 1.0) {
		t.Fatalf("k > len(results): want 1.0, got %f", got)
	}
}

func TestNDCGK_KZeroMeansNoLimit(t *testing.T) {
	t.Parallel()

	results := []string{"a", "b", "c"}
	relevant := map[string]bool{"a": true, "b": true, "c": true}

	got := search.NDCGK(results, relevant, 0)
	if !almostEqual(got, 1.0) {
		t.Fatalf("k=0 (no limit): want 1.0, got %f", got)
	}
}

func TestNDCGK_NoRelevantInResults(t *testing.T) {
	t.Parallel()

	results := []string{"x", "y", "z"}
	relevant := map[string]bool{"a": true}

	got := search.NDCGK(results, relevant, 3)
	if got != 0 {
		t.Fatalf("no relevant in results: want 0, got %f", got)
	}
}

// ---------------------------------------------------------------------------
// MRRK tests
// ---------------------------------------------------------------------------

func TestMRRK_FirstPositionRelevant(t *testing.T) {
	t.Parallel()

	results := []string{"a", "b", "c"}
	relevant := map[string]bool{"a": true}

	got := search.MRRK(results, relevant, 10)
	if !almostEqual(got, 1.0) {
		t.Fatalf("rank 1: want 1.0, got %f", got)
	}
}

func TestMRRK_ThirdPositionRelevant(t *testing.T) {
	t.Parallel()

	results := []string{"x", "y", "a", "b"}
	relevant := map[string]bool{"a": true}

	got := search.MRRK(results, relevant, 10)
	want := 1.0 / 3.0
	if !almostEqual(got, want) {
		t.Fatalf("rank 3: want %f, got %f", want, got)
	}
}

func TestMRRK_RelevantBeyondK(t *testing.T) {
	t.Parallel()

	// Relevant doc is at position 5 but k=3 → should return 0.
	results := []string{"x", "y", "z", "w", "a"}
	relevant := map[string]bool{"a": true}

	got := search.MRRK(results, relevant, 3)
	if got != 0 {
		t.Fatalf("relevant beyond k: want 0, got %f", got)
	}
}

func TestMRRK_EmptyRelevant(t *testing.T) {
	t.Parallel()

	results := []string{"a", "b", "c"}
	got := search.MRRK(results, map[string]bool{}, 10)
	if got != 0 {
		t.Fatalf("empty relevant: want 0, got %f", got)
	}
}

func TestMRRK_EmptyResults(t *testing.T) {
	t.Parallel()

	relevant := map[string]bool{"a": true}
	got := search.MRRK([]string{}, relevant, 10)
	if got != 0 {
		t.Fatalf("empty results: want 0, got %f", got)
	}
}

func TestMRRK_KZeroMeansNoLimit(t *testing.T) {
	t.Parallel()

	results := []string{"x", "y", "z", "a"}
	relevant := map[string]bool{"a": true}

	got := search.MRRK(results, relevant, 0)
	want := 1.0 / 4.0
	if !almostEqual(got, want) {
		t.Fatalf("k=0 no limit: want %f, got %f", want, got)
	}
}

// ---------------------------------------------------------------------------
// Aggregate tests
// ---------------------------------------------------------------------------

func TestAggregate_Empty(t *testing.T) {
	t.Parallel()

	m := search.Aggregate(nil, nil)
	if m.Pairs != 0 || m.NDCG5 != 0 || m.NDCG10 != 0 || m.MRR10 != 0 {
		t.Fatalf("empty input: want zero metrics, got %+v", m)
	}
}

func TestAggregate_LengthMismatch(t *testing.T) {
	t.Parallel()
	results := [][]string{{"a"}, {"b"}}
	relevant := []map[string]bool{{"a": true}} // length 1 vs 2
	m := search.Aggregate(results, relevant)
	if m.Pairs != 0 {
		t.Fatalf("mismatched input: want zero metrics, got %+v", m)
	}
}

func TestAggregate_SinglePerfect(t *testing.T) {
	t.Parallel()

	results := [][]string{{"a", "b", "c"}}
	relevant := []map[string]bool{{"a": true, "b": true, "c": true}}

	m := search.Aggregate(results, relevant)
	if m.Pairs != 1 {
		t.Fatalf("pairs: want 1, got %d", m.Pairs)
	}
	if !almostEqual(m.NDCG5, 1.0) {
		t.Fatalf("NDCG5: want 1.0, got %f", m.NDCG5)
	}
	if !almostEqual(m.MRR10, 1.0) {
		t.Fatalf("MRR10: want 1.0, got %f", m.MRR10)
	}
}
