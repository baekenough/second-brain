package model

import (
	"testing"
)

// ---------------------------------------------------------------------------
// Multi-source include filter.
//
// SearchQuery historically carried a single optional include filter
// (SourceType *SourceType). A query planner emits a LIST of source types, so a
// plural field is required. The two must resolve to ONE set, computed in ONE
// place — every retrieval lane (five SQL lanes in the store, two chunk lanes in
// the service) has to agree on what "included" means, and a per-lane
// re-derivation is exactly how #196 happened.
//
// These tests pin that single definition.
// ---------------------------------------------------------------------------

func TestIncludeSourceTypes_Empty(t *testing.T) {
	t.Parallel()

	q := SearchQuery{Query: "q"}
	if got := q.IncludeSourceTypes(); len(got) != 0 {
		t.Errorf("IncludeSourceTypes() = %v, want empty (no include filter = all sources)", got)
	}
}

func TestIncludeSourceTypes_SingularOnly(t *testing.T) {
	t.Parallel()

	st := SourceCalendar
	q := SearchQuery{Query: "q", SourceType: &st}

	got := q.IncludeSourceTypes()
	if len(got) != 1 || got[0] != SourceCalendar {
		t.Errorf("IncludeSourceTypes() = %v, want [calendar]", got)
	}
}

func TestIncludeSourceTypes_PluralOnly(t *testing.T) {
	t.Parallel()

	q := SearchQuery{Query: "q", SourceTypes: []SourceType{SourceGmail, SourceSMS}}

	got := q.IncludeSourceTypes()
	if len(got) != 2 || got[0] != SourceGmail || got[1] != SourceSMS {
		t.Errorf("IncludeSourceTypes() = %v, want [gmail sms] in order", got)
	}
}

// TestIncludeSourceTypes_BothAreUnioned pins the chosen relationship between
// the two fields: UNION, not intersection and not precedence.
//
// Both fields are include filters, and an include set is OR-semantics
// (`source_type = ANY(...)`), so the union is the natural lift of the singular
// field into the plural one. It is also the only combination rule that is
// monotone: a caller that sets only the legacy singular field can never lose a
// document because some other code path also set SourceTypes. Intersection and
// precedence both silently drop one of the two stated intents.
func TestIncludeSourceTypes_BothAreUnioned(t *testing.T) {
	t.Parallel()

	st := SourceCalendar
	q := SearchQuery{Query: "q", SourceType: &st, SourceTypes: []SourceType{SourceGmail}}

	got := q.IncludeSourceTypes()
	if len(got) != 2 {
		t.Fatalf("IncludeSourceTypes() = %v, want 2 entries (union of singular and plural)", got)
	}
	seen := map[SourceType]bool{}
	for _, s := range got {
		seen[s] = true
	}
	if !seen[SourceCalendar] || !seen[SourceGmail] {
		t.Errorf("IncludeSourceTypes() = %v, want the union {calendar, gmail}", got)
	}
}

// TestIncludeSourceTypes_Deduplicates keeps the bound parameter free of
// redundant values; `= ANY(...)` is set membership, so duplicates are noise
// that only shows up in logs and query plans.
func TestIncludeSourceTypes_Deduplicates(t *testing.T) {
	t.Parallel()

	st := SourceGmail
	q := SearchQuery{
		Query:       "q",
		SourceType:  &st,
		SourceTypes: []SourceType{SourceGmail, SourceSMS, SourceGmail},
	}

	got := q.IncludeSourceTypes()
	if len(got) != 2 {
		t.Fatalf("IncludeSourceTypes() = %v, want 2 distinct entries", got)
	}
}

// TestIncludeSourceTypes_DoesNotAliasCallerSlice guards the boundary-copy rule:
// the returned slice must not share backing storage with the caller's field, or
// a downstream filter step could mutate the request it was given.
func TestIncludeSourceTypes_DoesNotAliasCallerSlice(t *testing.T) {
	t.Parallel()

	orig := []SourceType{SourceGmail, SourceSMS}
	q := SearchQuery{Query: "q", SourceTypes: orig}

	got := q.IncludeSourceTypes()
	if len(got) == 0 {
		t.Fatal("IncludeSourceTypes() returned nothing")
	}
	got[0] = SourceSlack
	if orig[0] != SourceGmail {
		t.Errorf("mutating the returned slice changed the caller's SourceTypes: %v", orig)
	}
}
