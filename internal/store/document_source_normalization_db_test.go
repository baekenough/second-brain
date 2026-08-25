package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Real-database verification of migrations/027_secretary_source_normalization.sql
// and the duplicate-arrival guard query (internal/store/document_source_guard.go).
//
// This repo has already shipped SQL that compiled, passed stub tests, and
// failed at runtime (EXCLUDED referenced inside RETURNING — see agent memory
// feedback_postgres_returning_excluded). Migration 027's safety guards
// (collision re-check, volume threshold, transactional rollback) are exactly
// the kind of logic that a stub test cannot verify: it has to actually run
// against PostgreSQL to prove the abort paths really abort and really roll
// back.
//
// Skipped unless TEST_DATABASE_URL is set — see srcTestDB in
// document_source_types_db_test.go, which this file reuses. TEST_DATABASE_URL
// must NEVER point at production; run a throwaway container instead. Rows are
// scoped by the sentinel source_id prefix below and removed in t.Cleanup.
// ---------------------------------------------------------------------------

// normTestPrefix scopes both the seeded rows and the cleanup DELETE for this
// file's tests, distinct from srcTestPrefix so the two test files' cleanups
// never interfere with each other's fixtures.
const normTestPrefix = "zz-dummy-norm027-"

// normTestDB returns a *Postgres for TEST_DATABASE_URL, reusing srcTestDB's
// schema bootstrap (document_source_types_db_test.go, same package) and
// adding this file's own cleanup scope.
func normTestDB(t *testing.T) *Postgres {
	t.Helper()
	pg := srcTestDB(t) // ensures schema exists; registers its own cleanup for srcTestPrefix rows
	t.Cleanup(func() {
		_, _ = pg.pool.Exec(context.Background(),
			`DELETE FROM documents WHERE source_id LIKE $1`, normTestPrefix+"%")
	})
	return pg
}

// migrationSQL reads a migration file relative to this test file's package
// directory (internal/store -> ../../migrations). Reading the real file,
// rather than re-typing its SQL inline, means this test exercises exactly
// what RunMigrations would execute in production — a copy would drift.
func migrationSQL(t *testing.T, filename string) string {
	t.Helper()
	path := filepath.Join("..", "..", "migrations", filename)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration %q: %v", path, err)
	}
	return string(b)
}

// seedSecretaryDoc inserts a throwaway source_type='secretary' document with
// the given metadata->>'kind' and title. occurredAt may be nil.
func seedSecretaryDoc(t *testing.T, pg *Postgres, kind, titleSuffix string, occurredAt *time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	meta, err := json.Marshal(map[string]any{"kind": kind})
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	_, err = pg.pool.Exec(context.Background(), `
		INSERT INTO documents (id, source_type, source_id, title, content, metadata, occurred_at, collected_at)
		VALUES ($1, 'secretary', $2, $3, 'zzdummy sentinel body', $4, $5, now())`,
		id, normTestPrefix+id.String(), "zzdummy "+titleSuffix, meta, occurredAt,
	)
	if err != nil {
		t.Fatalf("seed secretary document (kind=%s): %v", kind, err)
	}
	return id
}

// TestDB_Migration027_NormalizesKnownKinds verifies the core rewrite: each
// secretary document whose metadata->>'kind' is one of the five known kinds
// is re-typed to that kind, and metadata.legacy_source_type='secretary' is
// stamped so the change is reversible.
func TestDB_Migration027_NormalizesKnownKinds(t *testing.T) {
	pg := normTestDB(t)
	ctx := context.Background()

	gmailID := seedSecretaryDoc(t, pg, "gmail", "gmail-doc", nil)
	smsID := seedSecretaryDoc(t, pg, "sms", "sms-doc", nil)
	callLogID := seedSecretaryDoc(t, pg, "call-log", "call-log-doc", nil)
	callTranscriptID := seedSecretaryDoc(t, pg, "call-transcript", "call-transcript-doc", nil)
	calendarID := seedSecretaryDoc(t, pg, "calendar", "calendar-doc", nil)

	sql := migrationSQL(t, "027_secretary_source_normalization.sql")
	if _, err := pg.pool.Exec(ctx, sql); err != nil {
		t.Fatalf("run migration 027: %v", err)
	}

	cases := []struct {
		id       uuid.UUID
		wantType string
	}{
		{gmailID, "gmail"},
		{smsID, "sms"},
		{callLogID, "call-log"},
		{callTranscriptID, "call-transcript"},
		{calendarID, "calendar"},
	}
	for _, tc := range cases {
		var sourceType string
		var metaRaw []byte
		err := pg.pool.QueryRow(ctx,
			`SELECT source_type, metadata FROM documents WHERE id = $1`, tc.id,
		).Scan(&sourceType, &metaRaw)
		if err != nil {
			t.Fatalf("fetch %s: %v", tc.id, err)
		}
		if sourceType != tc.wantType {
			t.Errorf("document %s: source_type = %q, want %q", tc.id, sourceType, tc.wantType)
		}
		var meta map[string]any
		if err := json.Unmarshal(metaRaw, &meta); err != nil {
			t.Fatalf("unmarshal metadata for %s: %v", tc.id, err)
		}
		if meta["legacy_source_type"] != "secretary" {
			t.Errorf("document %s: metadata.legacy_source_type = %v, want \"secretary\"", tc.id, meta["legacy_source_type"])
		}
		if meta["kind"] != tc.wantType {
			t.Errorf("document %s: metadata.kind = %v, want %q (must be preserved, not overwritten)", tc.id, meta["kind"], tc.wantType)
		}
	}
}

// TestDB_Migration027_LeavesLLMMemoryAndUnknownKindAlone verifies the two
// deliberate exclusions: metadata->>'kind'='llm-memory' and a kind outside
// the five recognised values are both left as source_type='secretary'.
func TestDB_Migration027_LeavesLLMMemoryAndUnknownKindAlone(t *testing.T) {
	pg := normTestDB(t)
	ctx := context.Background()

	llmMemID := seedSecretaryDoc(t, pg, "llm-memory", "llm-memory-doc", nil)
	unknownKindID := seedSecretaryDoc(t, pg, "some-future-kind", "unknown-kind-doc", nil)

	sql := migrationSQL(t, "027_secretary_source_normalization.sql")
	if _, err := pg.pool.Exec(ctx, sql); err != nil {
		t.Fatalf("run migration 027: %v", err)
	}

	for _, id := range []uuid.UUID{llmMemID, unknownKindID} {
		var sourceType string
		if err := pg.pool.QueryRow(ctx,
			`SELECT source_type FROM documents WHERE id = $1`, id,
		).Scan(&sourceType); err != nil {
			t.Fatalf("fetch %s: %v", id, err)
		}
		if sourceType != "secretary" {
			t.Errorf("document %s: source_type = %q, want unchanged \"secretary\"", id, sourceType)
		}
	}
}

// TestDB_Migration027_Idempotent verifies that running the migration a
// second time is a no-op: the WHERE source_type='secretary' predicate no
// longer matches rows the first run already re-typed.
func TestDB_Migration027_Idempotent(t *testing.T) {
	pg := normTestDB(t)
	ctx := context.Background()

	gmailID := seedSecretaryDoc(t, pg, "gmail", "idempotent-doc", nil)

	sql := migrationSQL(t, "027_secretary_source_normalization.sql")
	if _, err := pg.pool.Exec(ctx, sql); err != nil {
		t.Fatalf("run migration 027 (first pass): %v", err)
	}
	if _, err := pg.pool.Exec(ctx, sql); err != nil {
		t.Fatalf("run migration 027 (second pass): %v", err)
	}

	var sourceType, updatedAt1 string
	if err := pg.pool.QueryRow(ctx,
		`SELECT source_type, updated_at::text FROM documents WHERE id = $1`, gmailID,
	).Scan(&sourceType, &updatedAt1); err != nil {
		t.Fatalf("fetch after second pass: %v", err)
	}
	if sourceType != "gmail" {
		t.Errorf("after two migration runs, source_type = %q, want \"gmail\"", sourceType)
	}
}

// TestDB_Migration027_AbortsOnCollision verifies the collision guard: when a
// candidate's (kind, source_id) pair already exists as a real document, the
// entire migration rolls back — the candidate must remain untouched as
// source_type='secretary'.
func TestDB_Migration027_AbortsOnCollision(t *testing.T) {
	pg := normTestDB(t)
	ctx := context.Background()

	// Seed the secretary candidate AND a pre-existing gmail document sharing
	// the exact same source_id — the collision the guard is designed to catch.
	id := uuid.New()
	sourceID := normTestPrefix + id.String()
	meta, _ := json.Marshal(map[string]any{"kind": "gmail"})
	_, err := pg.pool.Exec(ctx, `
		INSERT INTO documents (id, source_type, source_id, title, content, metadata, collected_at)
		VALUES ($1, 'secretary', $2, 'zzdummy collision candidate', 'body', $3, now())`,
		id, sourceID, meta,
	)
	if err != nil {
		t.Fatalf("seed collision candidate: %v", err)
	}
	collidingID := uuid.New()
	_, err = pg.pool.Exec(ctx, `
		INSERT INTO documents (id, source_type, source_id, title, content, metadata, collected_at)
		VALUES ($1, 'gmail', $2, 'zzdummy pre-existing gmail doc', 'body', '{}'::jsonb, now())`,
		collidingID, sourceID,
	)
	if err != nil {
		t.Fatalf("seed pre-existing colliding gmail document: %v", err)
	}

	sql := migrationSQL(t, "027_secretary_source_normalization.sql")
	_, err = pg.pool.Exec(ctx, sql)
	if err == nil {
		t.Fatal("expected migration to fail (RAISE EXCEPTION) on collision, got nil error")
	}

	// The candidate must remain untouched — the abort must have rolled back
	// the whole DO block, not partially applied it.
	var sourceType string
	if scanErr := pg.pool.QueryRow(ctx,
		`SELECT source_type FROM documents WHERE id = $1`, id,
	).Scan(&sourceType); scanErr != nil {
		t.Fatalf("fetch candidate after aborted migration: %v", scanErr)
	}
	if sourceType != "secretary" {
		t.Errorf("candidate source_type = %q after aborted migration, want unchanged \"secretary\" (rollback failed)", sourceType)
	}
}

// TestDB_DuplicateArrivalCheckQuery_DetectsCrossSourceMatch runs the exact
// duplicateArrivalCheckQuery against a live database, proving the (title,
// occurred_at) index-friendly predicate actually matches a same-title,
// near-in-time, different-source_type document — and does NOT match when the
// source_type is the same or the title/time differ.
func TestDB_DuplicateArrivalCheckQuery_DetectsCrossSourceMatch(t *testing.T) {
	pg := normTestDB(t)
	ctx := context.Background()

	occurredAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	title := "zzdummy duplicate-arrival subject"

	// Existing gmail document.
	gmailDocID := uuid.New()
	_, err := pg.pool.Exec(ctx, `
		INSERT INTO documents (id, source_type, source_id, title, content, occurred_at, collected_at)
		VALUES ($1, 'gmail', $2, $3, 'body', $4, now())`,
		gmailDocID, normTestPrefix+gmailDocID.String(), title, occurredAt,
	)
	if err != nil {
		t.Fatalf("seed existing gmail document: %v", err)
	}

	// Case 1: an incoming 'secretary' document with the same title, 60s later
	// (inside the 120s window), different source_type -> must match.
	var existingSource string
	scanErr := pg.pool.QueryRow(ctx, duplicateArrivalCheckQuery,
		title, occurredAt.Add(-duplicateArrivalWindow), occurredAt.Add(duplicateArrivalWindow),
		"secretary",
	).Scan(&existingSource)
	if scanErr != nil {
		t.Fatalf("expected a cross-source match, got error: %v", scanErr)
	}
	if existingSource != "gmail" {
		t.Errorf("existingSource = %q, want \"gmail\"", existingSource)
	}

	// Case 2: same source_type as the existing document ('gmail') -> must NOT
	// match (this is what ON CONFLICT already handles; the guard should not
	// also fire for it).
	scanErr = pg.pool.QueryRow(ctx, duplicateArrivalCheckQuery,
		title, occurredAt.Add(-duplicateArrivalWindow), occurredAt.Add(duplicateArrivalWindow),
		"gmail",
	).Scan(&existingSource)
	if !isNoRows(scanErr) {
		t.Errorf("expected no match for the same source_type, got existingSource=%q err=%v", existingSource, scanErr)
	}

	// Case 3: different title -> must NOT match.
	scanErr = pg.pool.QueryRow(ctx, duplicateArrivalCheckQuery,
		"zzdummy an entirely different subject",
		occurredAt.Add(-duplicateArrivalWindow), occurredAt.Add(duplicateArrivalWindow),
		"secretary",
	).Scan(&existingSource)
	if !isNoRows(scanErr) {
		t.Errorf("expected no match for a different title, got existingSource=%q err=%v", existingSource, scanErr)
	}
}

// TestDB_CheckDuplicateArrival_LogsWithoutBlocking exercises
// (*DocumentStore).checkDuplicateArrival directly, confirming it increments
// the package counter on a match and never panics/blocks regardless of
// outcome.
func TestDB_CheckDuplicateArrival_LogsWithoutBlocking(t *testing.T) {
	pg := normTestDB(t)
	store := NewDocumentStore(pg)
	ctx := context.Background()
	resetSourceGuardCountersForTest()

	occurredAt := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	title := "zzdummy checkDuplicateArrival direct call"

	existingID := uuid.New()
	_, err := pg.pool.Exec(ctx, `
		INSERT INTO documents (id, source_type, source_id, title, content, occurred_at, collected_at)
		VALUES ($1, 'gmail', $2, $3, 'body', $4, now())`,
		existingID, normTestPrefix+existingID.String(), title, occurredAt,
	)
	if err != nil {
		t.Fatalf("seed existing document: %v", err)
	}

	incoming := &model.Document{
		SourceType: model.SourceSecretary,
		Title:      title,
		OccurredAt: &occurredAt,
	}
	store.checkDuplicateArrival(ctx, incoming)

	if hits := DuplicateArrivalGuardHits(); hits != 1 {
		t.Errorf("DuplicateArrivalGuardHits() = %d, want 1 after a matching call", hits)
	}

	// A second call with no title/occurred_at must be a no-op (skipped
	// entirely, no query) and must not increment the counter or error.
	resetSourceGuardCountersForTest()
	store.checkDuplicateArrival(ctx, &model.Document{SourceType: model.SourceUpload})
	if hits := DuplicateArrivalGuardHits(); hits != 0 {
		t.Errorf("DuplicateArrivalGuardHits() = %d, want 0 for a title-less/time-less document", hits)
	}
}

// TestDB_Migration028_IndexCreatesSuccessfully verifies the supporting index
// migration runs cleanly against the same schema.
func TestDB_Migration028_IndexCreatesSuccessfully(t *testing.T) {
	pg := normTestDB(t)
	ctx := context.Background()

	sql := migrationSQL(t, "028_title_occurred_at_index.sql")
	if _, err := pg.pool.Exec(ctx, sql); err != nil {
		t.Fatalf("run migration 028: %v", err)
	}
	// Idempotent: CREATE INDEX IF NOT EXISTS must not error on re-run.
	if _, err := pg.pool.Exec(ctx, sql); err != nil {
		t.Fatalf("run migration 028 (second pass): %v", err)
	}

	var exists bool
	if err := pg.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'idx_documents_title_occurred_at')`,
	).Scan(&exists); err != nil {
		t.Fatalf("check index existence: %v", err)
	}
	if !exists {
		t.Error("idx_documents_title_occurred_at was not created")
	}
}

// TestDB_Migration029_ReportsResidualLLMMemoryWithoutMutating verifies the
// census migration never deletes or reclassifies rows, regardless of count.
func TestDB_Migration029_ReportsResidualLLMMemoryWithoutMutating(t *testing.T) {
	pg := normTestDB(t)
	ctx := context.Background()

	id := uuid.New()
	_, err := pg.pool.Exec(ctx, `
		INSERT INTO documents (id, source_type, source_id, title, content, collected_at)
		VALUES ($1, 'llm-memory', $2, 'zzdummy residual llm-memory doc', 'body', now())`,
		id, normTestPrefix+id.String(),
	)
	if err != nil {
		t.Fatalf("seed residual llm-memory document: %v", err)
	}

	sql := migrationSQL(t, "029_llm_memory_residual_check.sql")
	if _, err := pg.pool.Exec(ctx, sql); err != nil {
		t.Fatalf("run migration 029: %v", err)
	}

	var sourceType, status string
	if err := pg.pool.QueryRow(ctx,
		`SELECT source_type, status FROM documents WHERE id = $1`, id,
	).Scan(&sourceType, &status); err != nil {
		t.Fatalf("fetch after migration 029: %v", err)
	}
	if sourceType != "llm-memory" {
		t.Errorf("source_type = %q after migration 029, want unchanged \"llm-memory\"", sourceType)
	}
	if status != "active" {
		t.Errorf("status = %q after migration 029, want unchanged \"active\" (must not soft-delete)", status)
	}
}

// TestDB_Migration027_AbortsOnVolumeThreshold verifies the second safety
// guard: when the candidate row count exceeds the 18400 threshold, the
// migration aborts (RAISE EXCEPTION) and rolls back rather than normalizing
// an unexpectedly large batch. 18401 rows are bulk-seeded via generate_series
// so the test stays fast even though it deliberately exceeds the threshold.
func TestDB_Migration027_AbortsOnVolumeThreshold(t *testing.T) {
	pg := normTestDB(t)
	ctx := context.Background()

	const seedCount = 18401
	_, err := pg.pool.Exec(ctx, `
		INSERT INTO documents (id, source_type, source_id, title, content, metadata, collected_at)
		SELECT
			gen_random_uuid(),
			'secretary',
			$1 || gen_random_uuid()::text,
			'zzdummy volume-threshold doc',
			'body',
			'{"kind":"gmail"}'::jsonb,
			now()
		FROM generate_series(1, $2)`,
		normTestPrefix, seedCount,
	)
	if err != nil {
		t.Fatalf("bulk-seed %d candidate documents: %v", seedCount, err)
	}

	sql := migrationSQL(t, "027_secretary_source_normalization.sql")
	if _, err := pg.pool.Exec(ctx, sql); err == nil {
		t.Fatal("expected migration to fail (RAISE EXCEPTION) when candidates exceed 18400, got nil error")
	}

	// Rollback must be total: none of the bulk-seeded rows should have been
	// re-typed away from 'secretary'.
	var stillSecretary int64
	if err := pg.pool.QueryRow(ctx,
		`SELECT count(*) FROM documents WHERE source_type = 'secretary' AND source_id LIKE $1`,
		normTestPrefix+"%",
	).Scan(&stillSecretary); err != nil {
		t.Fatalf("count remaining secretary rows: %v", err)
	}
	if stillSecretary != seedCount {
		t.Errorf("after aborted migration, %d/%d rows remain source_type='secretary'; want all %d (full rollback)",
			stillSecretary, seedCount, seedCount)
	}
}
