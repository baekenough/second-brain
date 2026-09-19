package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigration033SQL_ContainsSafetyGuards is a structural (no-DB) test,
// mirroring migration_027_sql_test.go: it pins the literal safety-guard
// clauses in migrations/033_call_unify.sql so a future edit cannot silently
// drop the volume threshold, the soft-delete-not-hard-delete guarantee, the
// FK-conflict-avoidance pattern, or the reversibility marker without a test
// failure — independent of whether TEST_DATABASE_URL is set.
func TestMigration033SQL_ContainsSafetyGuards(t *testing.T) {
	t.Parallel()

	sql := readMigration033(t)

	required := []struct {
		fragment string
		reason   string
	}{
		{"RAISE EXCEPTION", "an aborted safety check must actually abort the transaction, not just log"},
		{"v_threshold", "the pairing volume must be checked against a safety threshold before any soft-delete"},
		{"status     = 'deleted'", "the loser transcript must be soft-deleted, never hard-deleted"},
		{"legacy_source_type", "the original source_type must be preserved in metadata for reversibility"},
		{"merged_into", "a soft-deleted loser must record which document it was merged into"},
		{"source_type IN ('call-log', 'call-transcript')", "the final rename must be scoped to the two legacy types only"},
		{"ON CONFLICT (document_id, entity_id) DO NOTHING", "document_entities reassignment must not violate its composite PK"},
		{"ON CONFLICT (source_type, source_id) DO NOTHING", "transcription_ledger reassignment must not violate its composite PK"},
	}
	for _, tc := range required {
		tc := tc
		t.Run(tc.reason, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(sql, tc.fragment) {
				t.Errorf("migration 033 missing %q (%s)", tc.fragment, tc.reason)
			}
		})
	}
}

// TestMigration033SQL_NeverHardDeletesDocuments pins that migrations/
// 033_call_unify.sql contains no "DELETE FROM documents" — every documents-
// table mutation must be a soft-delete (status='deleted') or a plain
// UPDATE, matching every other destructive-looking migration in this repo
// (019, 027).
func TestMigration033SQL_NeverHardDeletesDocuments(t *testing.T) {
	t.Parallel()

	sql := readMigration033(t)
	if strings.Contains(sql, "DELETE FROM documents") {
		t.Error("migration 033 must never hard-delete from documents — use a soft-delete (status='deleted') instead")
	}
}

// TestMigration033SQL_BackupInstructionPresent pins that the migration file
// documents the pg_dump backup step a human operator must run before
// deploying it — this migration touches every historical call-log/
// call-transcript document in one transaction, and the comment is the only
// place that procedure is written down.
func TestMigration033SQL_BackupInstructionPresent(t *testing.T) {
	t.Parallel()

	sql := readMigration033(t)
	if !strings.Contains(sql, "pg_dump") {
		t.Error("migration 033 must document the pg_dump backup step before deployment")
	}
}

// readMigration033 reads migrations/033_call_unify.sql relative to this test
// file's package directory (internal/store -> ../../migrations), mirroring
// migration_027_sql_test.go's approach: exercising the real file rather than
// a re-typed copy that could drift.
func readMigration033(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "migrations", "033_call_unify.sql")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration 033: %v", err)
	}
	return string(b)
}
