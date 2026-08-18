package graph

import (
	"context"
	"strings"
	"testing"
)

func TestClampLimit(t *testing.T) {
	for _, c := range []struct{ in, def, max, want int }{
		{0, 20, 50, 20}, {-5, 20, 50, 20}, {10, 20, 50, 10}, {999, 20, 50, 50},
	} {
		if got := clampLimit(c.in, c.def, c.max); got != c.want {
			t.Errorf("clampLimit(%d,%d,%d) = %d, want %d", c.in, c.def, c.max, got, c.want)
		}
	}
}

// TestSanitizeRelTypes_DropsUnknown: relationship types are only ever used in
// a parameterisable position here (`type(r) IN $relTypes`), but they still go
// through the whitelist so a typo or a probe is cut at the edge instead of
// silently returning nothing.
func TestSanitizeRelTypes_DropsUnknown(t *testing.T) {
	if got := sanitizeRelTypes([]string{"COMMUNICATED_WITH", "DROP_EVERYTHING", "MENTIONS"}); len(got) != 2 {
		t.Fatalf("kept %d entries, want 2 (%v)", len(got), got)
	}
	if got := sanitizeRelTypes(nil); len(got) != 0 {
		t.Errorf("nil input kept %d entries, want 0", len(got))
	}
	if got := sanitizeRelTypes([]string{"communicated_with"}); len(got) != 0 {
		t.Errorf("lowercase input kept %d entries, want 0", len(got))
	}
}

func TestSanitizeEntityTypes_DropsUnknown(t *testing.T) {
	got := sanitizeEntityTypes([]string{"PERSON", "ORG", "ROBOT", ""})
	if len(got) != 2 {
		t.Fatalf("kept %d entries, want 2 (%v)", len(got), got)
	}
}

// TestEvidence_RejectsUnknownRelType keeps an unvalidated relationship type
// from reaching the query at all.
func TestEvidence_RejectsUnknownRelType(t *testing.T) {
	r := &Reader{} // no client: the guard must fire before any query runs
	if _, err := r.Evidence(context.Background(), 1, 2, "DROP_EVERYTHING", 10); err == nil {
		t.Fatal("unknown rel type: want error, got nil")
	}
}

// TestSearchEntities_RejectsEmptyPrefix avoids a prefix scan that would return
// the entire entity table.
func TestSearchEntities_RejectsEmptyPrefix(t *testing.T) {
	r := &Reader{}
	if _, err := r.SearchEntities(context.Background(), "   ", 10); err == nil {
		t.Fatal("blank prefix: want error, got nil")
	}
}

// TestFixedQueriesAreReadOnly is a static guard on the query catalogue: no
// fixed read query may contain a write clause. The read API exposes these
// four strings and nothing else — there is no path that executes
// caller-supplied Cypher.
func TestFixedQueriesAreReadOnly(t *testing.T) {
	writeClauses := []string{"CREATE", "MERGE", "DELETE", "SET ", "REMOVE", "DROP", "LOAD CSV", "IN TRANSACTIONS", "CALL"}
	queries := fixedQueries()
	if len(queries) != 4 {
		t.Fatalf("fixedQueries() has %d entries, want 4", len(queries))
	}
	for name, q := range queries {
		upper := strings.ToUpper(q)
		for _, clause := range writeClauses {
			if strings.Contains(upper, clause) {
				t.Errorf("query %q contains write clause %q", name, clause)
			}
		}
	}
}
