package askeval

import (
	"testing"
	"time"
)

// TestCitationWithinSupport is a package-level unit test for
// citationWithinSupport (deep-verify #273 LOW finding) — every EXISTING
// exercise of this function ran through computeMetrics with exactly ONE
// gold.support_doc_ids entry (TestComputeMetrics_CitationWithinSupportGatesPass,
// runner_test.go), which cannot distinguish "every cited ID must equal THE
// support doc" from citationWithinSupport's actual, documented contract:
// every cited ID must resolve into ANY entry of gold.support_doc_ids (an
// any-of membership test over a SET, not an equality test against a single
// ID) — a fixture with 2+ support docs is required to tell those two rules
// apart at all.
func TestCitationWithinSupport(t *testing.T) {
	f := Fixture{
		ID: "scratch-citation-within-support", Category: "single_turn", AsOf: "2026-06-10T09:00:00+09:00",
		Corpus: []CorpusDoc{
			{Alias: "support-a", SourceType: "call", Title: "a", Content: "content-a"},
			{Alias: "support-b", SourceType: "gmail", Title: "b", Content: "content-b"},
			{Alias: "non-support", SourceType: "gmail", Title: "c", Content: "content-c"},
		},
		Question: "q?",
		Gold: Gold{
			Answerable:  true,
			SupportDocs: []string{"support-a", "support-b"},
		},
	}
	asOf, err := time.Parse(time.RFC3339, f.AsOf)
	if err != nil {
		t.Fatalf("as_of: %v", err)
	}
	cp, err := buildCorpus(f, asOf)
	if err != nil {
		t.Fatalf("buildCorpus: %v", err)
	}
	idA, ok := cp.resolveAlias("support-a")
	if !ok {
		t.Fatal("resolveAlias(support-a) failed")
	}
	idB, ok := cp.resolveAlias("support-b")
	if !ok {
		t.Fatal("resolveAlias(support-b) failed")
	}
	idOther, ok := cp.resolveAlias("non-support")
	if !ok {
		t.Fatal("resolveAlias(non-support) failed")
	}

	cases := []struct {
		name     string
		citedIDs []string
		want     *bool
	}{
		{
			name:     "cites only the FIRST of 2+ support docs: true (any-of, not equality against a single ID)",
			citedIDs: []string{idA},
			want:     ptrBool(true),
		},
		{
			name:     "cites only the SECOND of 2+ support docs: true (any-of)",
			citedIDs: []string{idB},
			want:     ptrBool(true),
		},
		{
			name:     "cites BOTH support docs: true",
			citedIDs: []string{idA, idB},
			want:     ptrBool(true),
		},
		{
			name:     "cites a real, retrieved document OUTSIDE support_doc_ids: false",
			citedIDs: []string{idOther},
			want:     ptrBool(false),
		},
		{
			name:     "cites one support doc AND one non-support doc: false (every cited ID must resolve, not just one)",
			citedIDs: []string{idA, idOther},
			want:     ptrBool(false),
		},
		{
			name:     "no citations at all: nil (not applicable, never a bare false)",
			citedIDs: nil,
			want:     nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := citationWithinSupport(f, cp, c.citedIDs)
			if (got == nil) != (c.want == nil) {
				t.Fatalf("citationWithinSupport(...) nilness = %v, want %v", got == nil, c.want == nil)
			}
			if got != nil && *got != *c.want {
				t.Errorf("citationWithinSupport(...) = %v, want %v", *got, *c.want)
			}
		})
	}

	t.Run("empty support_doc_ids: nil regardless of citations", func(t *testing.T) {
		empty := f
		empty.Gold.SupportDocs = nil
		if got := citationWithinSupport(empty, cp, []string{idA}); got != nil {
			t.Errorf("want nil when gold.support_doc_ids is empty, got %v", *got)
		}
	})
}
