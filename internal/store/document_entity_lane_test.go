package store

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Entity RRF lane filter coverage (#139 follow-up).
//
// The entity CTE used to select straight out of document_entities with no
// status or source-type filter, and the outer `JOIN documents d ON d.id =
// rrf.id` re-applied nothing. Any document reachable by entity name alone
// therefore bypassed BOTH the soft-delete filter and every source-type
// exclusion — so a deleted document, or an insight document the caller
// explicitly excluded, could still be returned whenever the entity lane was
// active (ENTITY_EXTRACTION_ENABLED).
// ---------------------------------------------------------------------------

// TestBuildEntityCTE_AppliesAllFilters pins that every filter handed to the
// lane reaches the generated SQL. A lane that silently drops one of them is
// indistinguishable at runtime from a lane that has none.
func TestBuildEntityCTE_AppliesAllFilters(t *testing.T) {
	t.Parallel()

	got := buildEntityCTE("$4", "AND d.status = 'active'", "AND d.source_type = $5", "AND d.source_type <> ALL($6)")

	fragments := []struct {
		name string
		text string
	}{
		{"joins documents so the filters have a table to filter on", "JOIN documents d ON d.id = de.document_id"},
		{"soft-delete filter", "AND d.status = 'active'"},
		{"source-type filter", "AND d.source_type = $5"},
		{"exclusion filter (insight guard)", "AND d.source_type <> ALL($6)"},
		{"entity name match still bound to its own parameter", "e.normalized_name LIKE"},
		{"candidate cap", "LIMIT $3"},
	}
	for _, f := range fragments {
		if !strings.Contains(got, f.text) {
			t.Errorf("entity CTE missing %s (%q):\n%s", f.name, f.text, got)
		}
	}
}

// TestBuildEntityCTE_FiltersPrecedeTheLimit pins the ordering that makes the
// filters worth applying here at all: the lane is capped by LIMIT $3, so
// filtering has to happen before the cap or excluded documents would consume
// candidate slots and shrink the lane's real contribution.
func TestBuildEntityCTE_FiltersPrecedeTheLimit(t *testing.T) {
	t.Parallel()

	got := buildEntityCTE("$4", "AND d.status = 'active'", "", "AND d.source_type <> ALL($5)")

	limitAt := strings.Index(got, "LIMIT $3")
	if limitAt < 0 {
		t.Fatalf("entity CTE has no LIMIT:\n%s", got)
	}
	for _, filter := range []string{"AND d.status = 'active'", "AND d.source_type <> ALL($5)"} {
		if at := strings.Index(got, filter); at < 0 || at > limitAt {
			t.Errorf("filter %q must appear before LIMIT $3 (filter at %d, limit at %d):\n%s", filter, at, limitAt, got)
		}
	}
}

// TestBuildEntityCTE_IncludeDeleted_OmitsStatusFilter verifies the lane honours
// an explicit include_deleted request rather than hardcoding the filter.
func TestBuildEntityCTE_IncludeDeleted_OmitsStatusFilter(t *testing.T) {
	t.Parallel()

	got := buildEntityCTE("$4", "", "", "")
	if strings.Contains(got, "status") {
		t.Errorf("entity CTE applied a status filter when none was requested:\n%s", got)
	}
}

// TestEmptyEntityCTE_ContributesNothing pins the disabled-lane shape: when the
// entity weight is zero the CTE must still be a valid relation with the same
// (id, rank) columns, or the FULL OUTER JOIN chain in hybridSearch breaks.
func TestEmptyEntityCTE_ContributesNothing(t *testing.T) {
	t.Parallel()

	for _, want := range []string{"entity AS (", "NULL::uuid AS id", "NULL::bigint AS rank", "WHERE false"} {
		if !strings.Contains(emptyEntityCTE, want) {
			t.Errorf("emptyEntityCTE missing %q: %q", want, emptyEntityCTE)
		}
	}
}
