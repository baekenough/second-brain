package dataset

import (
	"fmt"
	"testing"
)

// The split rule is a frozen contract: it decides, permanently, which labels a
// weight optimiser is allowed to see. These tests exist to make any change to
// it loud.

func TestSplitOf_Deterministic(t *testing.T) {
	t.Parallel()
	const q = "dummy query about scheduling"
	want := SplitOf(q)
	if want == "" {
		t.Fatalf("SplitOf returned empty for a non-empty query")
	}
	for i := 0; i < 1000; i++ {
		if got := SplitOf(q); got != want {
			t.Fatalf("iteration %d: SplitOf = %q, want %q", i, got, want)
		}
	}
}

func TestSplitOf_NormalizationInsensitive(t *testing.T) {
	t.Parallel()
	cases := []struct{ a, b string }{
		{" Foo ", "foo"},
		{"BAR", "bar"},
		{"  mixed Case Query  ", "mixed case query"},
	}
	for _, c := range cases {
		if got, want := SplitOf(c.a), SplitOf(c.b); got != want {
			t.Errorf("SplitOf(%q)=%q != SplitOf(%q)=%q", c.a, got, c.b, want)
		}
	}
}

func TestSplitOf_EmptyQueryIsUnsplittable(t *testing.T) {
	t.Parallel()
	for _, q := range []string{"", "   "} {
		if got := SplitOf(q); got != "" {
			t.Errorf("SplitOf(%q) = %q, want %q", q, got, "")
		}
	}
}

func TestSplitOf_RoughlySeventyThirty(t *testing.T) {
	t.Parallel()
	const n = 10000
	train := 0
	for i := 0; i < n; i++ {
		// Deterministic synthetic queries — no personal data.
		if SplitOf(fmt.Sprintf("synthetic probe %d", i)) == SplitTrain {
			train++
		}
	}
	ratio := float64(train) / float64(n)
	if ratio < 0.65 || ratio > 0.75 {
		t.Fatalf("train ratio = %.4f, want within [0.65, 0.75]", ratio)
	}
}

// TestSplitOf_KnownVectors pins the hash rule to values produced by PostgreSQL
// for the identical expression in migrations/025_feedback_evidence.sql. If this
// fails, Go and SQL no longer agree and the holdout set is contaminated the
// moment a row is inserted by one side and backfilled by the other.
func TestSplitOf_KnownVectors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		query string
		want  string
	}{
		{"a", SplitHoldout},           // md5 prefix c01cfb8b -> 3223124875 % 100 = 75
		{"b", SplitTrain},             // 0b49b11f -> 189378847 % 100 = 47
		{"c", SplitTrain},             // aed43c56 -> 2933144662 % 100 = 62
		{"hello world", SplitHoldout}, // 2f00aa1b -> 788572699 % 100 = 99
		{"zzz", SplitTrain},           // 85d8a445 -> 2245567557 % 100 = 57
	}
	for _, c := range cases {
		if got := SplitOf(c.query); got != c.want {
			t.Errorf("SplitOf(%q) = %q, want %q", c.query, got, c.want)
		}
	}
}
