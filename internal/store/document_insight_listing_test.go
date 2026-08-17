package store

import (
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
)

// ---------------------------------------------------------------------------
// Insight documents must not re-enter LLM processing.
//
// ListUnsummarized and ListWithoutEntities had no source_type filter, so every
// insight document was picked up by the SummarizerWorker (an LLM summary of an
// LLM inference) and by the EntityWorker when enabled. Both are unbudgeted
// extra LLM calls per note, and the entity links the second one produces put
// insights into the entity RRF lane — routing an inference back into retrieval
// through a lane the insight-exclusion guard did not previously cover.
//
// These tests assert on the real query constants, not on inline copies, so
// deleting a filter fails the suite.
// ---------------------------------------------------------------------------

func TestListingQueries_ExcludeInsightDocuments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		query  string
		worker string
	}{
		{"listUnsummarizedQuery", listUnsummarizedQuery, "SummarizerWorker"},
		{"listWithoutEntitiesQuery", listWithoutEntitiesQuery, "EntityWorker"},
	}

	want := "source_type <> '" + string(model.SourceInsight) + "'"
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(tt.query, want) {
				t.Errorf("%s is missing %q — every insight document would be fed to %s, one paid LLM call each:\n%s",
					tt.name, want, tt.worker, tt.query)
			}
			// Both listings drive backfill workers, so the existing
			// soft-delete and ordering guarantees must survive the change.
			for _, keep := range []string{"status = 'active'", "ORDER BY collected_at ASC", "LIMIT $1"} {
				if !strings.Contains(tt.query, keep) {
					t.Errorf("%s lost %q:\n%s", tt.name, keep, tt.query)
				}
			}
		})
	}
}

// TestListingQueries_KeepTheirOwnPredicates guards against the two constants
// drifting into each other during an edit — they select on different columns
// and are consumed by different workers.
func TestListingQueries_KeepTheirOwnPredicates(t *testing.T) {
	t.Parallel()

	if !strings.Contains(listUnsummarizedQuery, "title_summary IS NULL") {
		t.Errorf("listUnsummarizedQuery lost its title_summary predicate:\n%s", listUnsummarizedQuery)
	}
	if !strings.Contains(listWithoutEntitiesQuery, "entities_processed_at IS NULL") {
		t.Errorf("listWithoutEntitiesQuery lost its entities_processed_at predicate:\n%s", listWithoutEntitiesQuery)
	}
}
