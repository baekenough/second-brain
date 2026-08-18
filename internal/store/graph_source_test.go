package store

import (
	"context"
	"strings"
	"testing"
)

// The store package has no database harness — these tests cover only the pure
// parts (argument validation and cursor-clause construction). The SQL itself
// is validated separately against a real PostgreSQL (see the Part B plan,
// Task 4 Step 4); a passing unit test here proves nothing about SQL validity.

func TestListRelationsAfter_RejectsNonPositiveLimit(t *testing.T) {
	if _, err := (&GraphSource{}).ListRelationsAfter(context.Background(), 0, 0); err == nil {
		t.Fatal("limit=0: want error, got nil")
	}
	if _, err := (&GraphSource{}).ListRelationsAfter(context.Background(), 0, -1); err == nil {
		t.Fatal("limit=-1: want error, got nil")
	}
}

func TestListMentionsAfter_RejectsNonPositiveLimit(t *testing.T) {
	if _, err := (&GraphSource{}).ListMentionsAfter(context.Background(), "", 0, 0); err == nil {
		t.Fatal("limit=0: want error, got nil")
	}
}

// TestMentionCursorClause pins the reason the mention query is assembled from
// two variants instead of one statement with an `$1 = '' OR ...` guard:
// PostgreSQL does not guarantee OR short-circuiting, so an empty cursor
// reaching a `$1::uuid` cast is a runtime error waiting to happen.
func TestMentionCursorClause(t *testing.T) {
	if got := mentionCursorClause(""); got != "" {
		t.Errorf("empty cursor = %q, want %q", got, "")
	}
	got := mentionCursorClause("00000000-0000-0000-0000-0000000000aa")
	if got == "" {
		t.Fatal("non-empty cursor: want WHERE clause")
	}
	if !strings.Contains(got, "$1::uuid") || !strings.Contains(got, "$2") {
		t.Errorf("cursor clause = %q, want it to bind $1::uuid and $2", got)
	}
}

// TestMentionCursorClause_NoInterpolation guards the clause builder against
// ever embedding the cursor value itself into SQL.
func TestMentionCursorClause_NoInterpolation(t *testing.T) {
	cursor := "'; DROP TABLE documents; --"
	if got := mentionCursorClause(cursor); strings.Contains(got, "DROP") {
		t.Fatalf("cursor value leaked into SQL: %q", got)
	}
}
