package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigration027SQL_ContainsSafetyGuards is a structural (no-DB) test,
// mirroring the existing callTranscriptDupCheckQuery / advisory-lock SQL
// tests in this package: it pins the literal safety-guard clauses in
// migrations/027_secretary_source_normalization.sql so a future edit cannot
// silently drop the collision check, the volume threshold, or the
// reversibility marker without a test failure — independent of whether
// TEST_DATABASE_URL is set.
func TestMigration027SQL_ContainsSafetyGuards(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "migrations", "027_secretary_source_normalization.sql")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration 027: %v", err)
	}
	sql := string(b)

	required := []struct {
		fragment string
		reason   string
	}{
		{"RAISE EXCEPTION", "an aborted safety check must actually abort the transaction, not just log"},
		{"18400", "the volume safety threshold must be present and match the documented margin over 18328"},
		{"legacy_source_type", "the original source_type must be preserved in metadata for reversibility"},
		{"'gmail', 'sms', 'call-log', 'call-transcript', 'calendar'", "the kind allowlist must be the exact five normalized kinds, excluding llm-memory"},
		{"source_type = 'secretary'", "the UPDATE must be scoped to secretary documents only"},
	}
	for _, tc := range required {
		tc := tc
		t.Run(tc.reason, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(sql, tc.fragment) {
				t.Errorf("migration 027 missing %q (%s)", tc.fragment, tc.reason)
			}
		})
	}

	// llm-memory must never appear inside the UPDATE's kind allowlist — it is
	// only referenced in prose comments, never in the executable WHERE/IN list.
	if strings.Contains(sql, "'llm-memory', 'gmail'") || strings.Contains(sql, "'gmail', 'llm-memory'") {
		t.Error("migration 027's kind allowlist must not include llm-memory")
	}
}

// TestMigration028SQL_IsIdempotentAndPartial pins the CREATE INDEX IF NOT
// EXISTS + partial-index clauses that keep the supporting index cheap and
// safe to re-run.
func TestMigration028SQL_IsIdempotentAndPartial(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "migrations", "028_title_occurred_at_index.sql")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration 028: %v", err)
	}
	sql := string(b)

	required := []string{
		"CREATE INDEX IF NOT EXISTS",
		"(title, occurred_at)",
		"WHERE status = 'active'",
	}
	for _, fragment := range required {
		if !strings.Contains(sql, fragment) {
			t.Errorf("migration 028 missing %q", fragment)
		}
	}
}

// TestMigration029SQL_NeverMutates pins that the residual llm-memory check
// contains no UPDATE/DELETE — it must be read-only.
func TestMigration029SQL_NeverMutates(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "migrations", "029_llm_memory_residual_check.sql")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration 029: %v", err)
	}
	sql := string(b)

	// Checked against actual DML statement heads (case-sensitive, this repo's
	// SQL style always uppercases keywords — see migrations/027) rather than a
	// blanket case-insensitive substring search, which would false-positive on
	// prose comments like "this migration does not delete or reclassify them".
	for _, forbidden := range []string{"UPDATE documents", "DELETE FROM documents", "INSERT INTO documents"} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("migration 029 must be read-only but contains %q", forbidden)
		}
	}
	if !strings.Contains(sql, "RAISE NOTICE") {
		t.Error("migration 029 must report its count via RAISE NOTICE")
	}
}
