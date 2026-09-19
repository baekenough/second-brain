package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Real-database verification of migrations/033_call_unify.sql.
//
// Unlike the other *_db_test.go files in this package, this test does not use
// ensureSrcTestSchema's minimal hand-rolled schema — that helper deliberately
// skips real migrations because pg_bigm (migration 006) is unavailable on a
// vanilla pgvector/pgvector image (see its doc comment). Migration 033
// touches six real tables (documents, chunks, actions, feedback,
// document_entities, entity_relations, golden_judgments, transcription_ledger)
// with real FK/UNIQUE constraints, so this test instead runs the FULL
// migration set via (*Postgres).RunMigrations against a database that
// actually has pg_bigm — e.g. a container built from
// deploy/postgres/Dockerfile (pgvector/pgvector:0.8.2-pg16 + pg_bigm compiled
// from source):
//
//	docker build -t sb-migration-test-pg -f deploy/postgres/Dockerfile deploy/postgres/
//	docker run -d --name sb-call-unify-test -e POSTGRES_PASSWORD=testpass \
//	    -e POSTGRES_DB=testdb -p 15544:5432 sb-migration-test-pg
//	TEST_DATABASE_URL='postgres://postgres:testpass@localhost:15544/testdb?sslmode=disable' \
//	    go test ./internal/store/... -run TestDB_Migration033 -v
//
// Skipped (not failed) when TEST_DATABASE_URL is unset OR when pg_bigm is not
// installable on the target database — this keeps `go test ./...` green on a
// vanilla postgres/pgvector instance while still giving a fully automated,
// reproducible check against the real migration file whenever the right
// container is available.
// ---------------------------------------------------------------------------

// migration033TestDB connects to TEST_DATABASE_URL, runs the FULL migration
// set (migrations/ — not a hand-rolled schema), and returns the *Postgres.
// Skips when TEST_DATABASE_URL is unset or pg_bigm cannot be installed.
func migration033TestDB(t *testing.T) *Postgres {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-database migration 033 test")
	}

	pg, err := NewPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(pg.Close)

	// Preflight 1: migration 006 requires pg_bigm. A vanilla pgvector image
	// does not ship it — skip cleanly rather than fail every run of
	// `go test ./...` on a database that was never meant to run this test.
	var available bool
	if err := pg.pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM pg_available_extensions WHERE name = 'pg_bigm')`,
	).Scan(&available); err != nil || !available {
		t.Skip("pg_bigm not available on TEST_DATABASE_URL; run against deploy/postgres/Dockerfile's image to exercise migration 033 end-to-end")
	}

	// Preflight 2: ensureSrcTestSchema (document_source_types_db_test.go,
	// used by this package's OTHER db tests, including
	// document_attach_transcript_db_test.go) installs a STUB bigm_similarity
	// function so its hand-rolled minimal schema can run without the real
	// extension. That stub's CREATE FUNCTION has no OR REPLACE, so migration
	// 006's own CREATE FUNCTION collides with it ("already exists with same
	// argument types") if a test using that lightweight bootstrap has
	// already touched this same database earlier in this test run — the two
	// schema-bootstrap conventions are mutually exclusive within one shared
	// database. Detect the stub by its exact (distinctive, always-zero)
	// function body and skip rather than fail: this combination genuinely
	// cannot be tested together, and failing loudly here would make an
	// unrelated test's run order decide whether this one passes. Run this
	// test in isolation (`-run TestDB_Migration033`) or against its own
	// dedicated database to avoid the conflict entirely.
	var stubPresent bool
	if err := pg.pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM pg_proc WHERE proname = 'bigm_similarity' AND prosrc = 'SELECT 0.0::float8')`,
	).Scan(&stubPresent); err == nil && stubPresent {
		t.Skip("ensureSrcTestSchema's stub bigm_similarity is already installed on TEST_DATABASE_URL (another test in this run used the lightweight schema bootstrap) — migration 033's real-DB test needs a database untouched by that bootstrap; run with -run TestDB_Migration033 in isolation, or point TEST_DATABASE_URL at a dedicated database")
	}

	migrationsDir := filepath.Join("..", "..", "migrations")
	if err := pg.RunMigrations(context.Background(), migrationsDir, 1536); err != nil {
		t.Fatalf("run full migration set: %v", err)
	}

	return pg
}

// call033Fixture is one (call-log, call-transcript) seed pair, or a
// standalone document when one side is the zero value.
type call033Fixture struct {
	id         uuid.UUID
	sourceType string
	sourceID   string
	title      string
	content    string
	metadata   map[string]any
	occurredAt time.Time
	updatedAt  time.Time
}

func seedCall033Doc(t *testing.T, pg *Postgres, f call033Fixture) {
	t.Helper()
	meta, err := json.Marshal(f.metadata)
	if err != nil {
		t.Fatalf("marshal metadata for %s: %v", f.sourceID, err)
	}
	_, err = pg.pool.Exec(context.Background(), `
		INSERT INTO documents (id, source_type, source_id, title, content, metadata, occurred_at, collected_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7, now(), $8)`,
		f.id, f.sourceType, f.sourceID, f.title, f.content, meta, f.occurredAt, f.updatedAt,
	)
	if err != nil {
		t.Fatalf("seed %s document %s: %v", f.sourceType, f.sourceID, err)
	}
}

func docState033(t *testing.T, pg *Postgres, id uuid.UUID) (sourceType, status, content string, metadata map[string]any) {
	t.Helper()
	var metaRaw []byte
	err := pg.pool.QueryRow(context.Background(),
		`SELECT source_type, status, content, metadata FROM documents WHERE id = $1`, id,
	).Scan(&sourceType, &status, &content, &metaRaw)
	if err != nil {
		t.Fatalf("query document %s: %v", id, err)
	}
	metadata = map[string]any{}
	if err := json.Unmarshal(metaRaw, &metadata); err != nil {
		t.Fatalf("unmarshal metadata for %s: %v", id, err)
	}
	return sourceType, status, content, metadata
}

// TestDB_Migration033_MergesPairAndPreservesCallLogMetadata seeds a clean
// call-log/call-transcript pair (fixture 1 from the manual verification this
// test codifies) and verifies: the winner becomes source_type='call' with the
// transcript's content, transcription="done", and the call-log-only fields
// (contact_name/direction/duration_seconds) surviving the metadata merge; the
// loser is soft-deleted with metadata.merged_into pointing at the winner.
func TestDB_Migration033_MergesPairAndPreservesCallLogMetadata(t *testing.T) {
	pg := migration033TestDB(t)
	ctx := context.Background()

	logID := uuid.New()
	trID := uuid.New()
	audioFile := "zz033-" + logID.String() + ".m4a"
	t.Cleanup(func() {
		_, _ = pg.pool.Exec(ctx, `DELETE FROM documents WHERE id IN ($1, $2)`, logID, trID)
	})

	occurred := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	seedCall033Doc(t, pg, call033Fixture{
		id: logID, sourceType: "call-log", sourceID: "call-log:zz033:aaa:bbb",
		title:   "incoming 통화 상대1",
		content: "상대방: 상대1\n통화 방향: incoming\n시각: 2026-01-01\n통화 시간: 30s",
		metadata: map[string]any{
			"contact_name": "상대1", "direction": "incoming",
			"duration_seconds": 30, "audio_file": audioFile,
		},
		occurredAt: occurred, updatedAt: occurred.Add(5 * time.Second),
	})
	seedCall033Doc(t, pg, call033Fixture{
		id: trID, sourceType: "call-transcript", sourceID: "transcript:call/" + audioFile,
		title:   "transcript",
		content: "실제 통화 전사 내용입니다.",
		metadata: map[string]any{
			"relative_path": "call/" + audioFile, "model": "whisper-1",
		},
		occurredAt: occurred, updatedAt: occurred.Add(5 * time.Minute),
	})

	if err := pg.RunMigrations(ctx, filepath.Join("..", "..", "migrations"), 1536); err != nil {
		t.Fatalf("re-run migrations: %v", err)
	}

	winSrc, winStatus, winContent, winMeta := docState033(t, pg, logID)
	if winSrc != "call" {
		t.Errorf("winner source_type = %q, want %q", winSrc, "call")
	}
	if winStatus != "active" {
		t.Errorf("winner status = %q, want %q", winStatus, "active")
	}
	if winContent != "실제 통화 전사 내용입니다." {
		t.Errorf("winner content = %q, want transcript content", winContent)
	}
	if winMeta["transcription"] != "done" {
		t.Errorf("winner metadata.transcription = %v, want \"done\"", winMeta["transcription"])
	}
	if winMeta["contact_name"] != "상대1" {
		t.Errorf("winner metadata.contact_name = %v, want \"상대1\" (call-log field must survive the merge)", winMeta["contact_name"])
	}
	if winMeta["legacy_source_type"] != "call-log" {
		t.Errorf("winner metadata.legacy_source_type = %v, want \"call-log\"", winMeta["legacy_source_type"])
	}

	loseSrc, loseStatus, _, loseMeta := docState033(t, pg, trID)
	if loseSrc != "call" {
		t.Errorf("loser source_type = %q, want %q", loseSrc, "call")
	}
	if loseStatus != "deleted" {
		t.Errorf("loser status = %q, want %q (soft-delete, never hard-delete)", loseStatus, "deleted")
	}
	if loseMeta["merged_into"] != logID.String() {
		t.Errorf("loser metadata.merged_into = %v, want %q", loseMeta["merged_into"], logID.String())
	}

	// Idempotency: re-running must not change anything (no matching
	// source_type rows left).
	if err := pg.RunMigrations(ctx, filepath.Join("..", "..", "migrations"), 1536); err != nil {
		t.Fatalf("second re-run migrations: %v", err)
	}
	winSrc2, winStatus2, _, _ := docState033(t, pg, logID)
	if winSrc2 != winSrc || winStatus2 != winStatus {
		t.Errorf("second migration run changed winner state: (%q,%q) -> (%q,%q)", winSrc, winStatus, winSrc2, winStatus2)
	}
}

// TestDB_Migration033_PicksMostRecentTranscript_OnOneToManyMatch seeds one
// call-log document matched by TWO call-transcript documents (a
// re-transcription under a renamed path) and verifies the migration merges
// the MOST RECENTLY UPDATED transcript, leaving the older one as its own
// independent 'call' document rather than deleting or force-combining it.
func TestDB_Migration033_PicksMostRecentTranscript_OnOneToManyMatch(t *testing.T) {
	pg := migration033TestDB(t)
	ctx := context.Background()

	logID := uuid.New()
	oldTrID := uuid.New()
	newTrID := uuid.New()
	audioFile := "zz033-1n-" + logID.String() + ".m4a"
	t.Cleanup(func() {
		_, _ = pg.pool.Exec(ctx, `DELETE FROM documents WHERE id IN ($1, $2, $3)`, logID, oldTrID, newTrID)
	})

	occurred := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	seedCall033Doc(t, pg, call033Fixture{
		id: logID, sourceType: "call-log", sourceID: "call-log:zz033-1n:ccc:ddd",
		title: "incoming 통화 상대2", content: "상대방: 상대2\n통화 방향: incoming\n시각: 2026-01-02\n통화 시간: 20s",
		metadata:   map[string]any{"contact_name": "상대2", "direction": "incoming", "duration_seconds": 20, "audio_file": audioFile},
		occurredAt: occurred, updatedAt: occurred.Add(5 * time.Second),
	})
	seedCall033Doc(t, pg, call033Fixture{
		id: oldTrID, sourceType: "call-transcript", sourceID: "transcript:call/old/" + audioFile,
		title: "old-path", content: "오래된 경로의 전사(구버전).",
		metadata:   map[string]any{"relative_path": "call/old/" + audioFile},
		occurredAt: occurred, updatedAt: occurred.Add(5 * time.Minute),
	})
	seedCall033Doc(t, pg, call033Fixture{
		id: newTrID, sourceType: "call-transcript", sourceID: "transcript:call/new/" + audioFile,
		title: "new-path", content: "새 경로로 재전사된 최신 내용.",
		metadata:   map[string]any{"relative_path": "call/new/" + audioFile},
		occurredAt: occurred, updatedAt: occurred.Add(10 * time.Minute),
	})

	if err := pg.RunMigrations(ctx, filepath.Join("..", "..", "migrations"), 1536); err != nil {
		t.Fatalf("re-run migrations: %v", err)
	}

	_, winStatus, winContent, _ := docState033(t, pg, logID)
	if winStatus != "active" {
		t.Errorf("winner status = %q, want active", winStatus)
	}
	if winContent != "새 경로로 재전사된 최신 내용." {
		t.Errorf("winner content = %q, want the MOST RECENT transcript's content", winContent)
	}

	_, newStatus, _, _ := docState033(t, pg, newTrID)
	if newStatus != "deleted" {
		t.Errorf("newer transcript status = %q, want deleted (merged into winner)", newStatus)
	}

	oldSrc, oldStatus, oldContent, oldMeta := docState033(t, pg, oldTrID)
	if oldSrc != "call" || oldStatus != "active" {
		t.Errorf("older transcript (log_id, status) = (%q, %q), want (call, active) — a loser transcript must stay standalone, not be deleted or merged into", oldSrc, oldStatus)
	}
	if oldContent != "오래된 경로의 전사(구버전)." {
		t.Errorf("older transcript content changed unexpectedly: %q", oldContent)
	}
	if oldMeta["transcription"] != "done" {
		t.Errorf("older standalone transcript metadata.transcription = %v, want \"done\"", oldMeta["transcription"])
	}
}

// TestDB_Migration033_RebuildsPendingPlaceholderContent seeds a call-log
// document carrying the pre-fix "[TRANSCRIPTION PENDING]" placeholder (the
// ~608-document production shape this migration also cleans up) with no
// matching transcript, and verifies the content is rebuilt into the same
// 4-line summary shape smsmap.MapCall produces, while
// metadata.transcription stays "pending" (a recording really is still
// outstanding for this call).
func TestDB_Migration033_RebuildsPendingPlaceholderContent(t *testing.T) {
	pg := migration033TestDB(t)
	ctx := context.Background()

	id := uuid.New()
	t.Cleanup(func() {
		_, _ = pg.pool.Exec(ctx, `DELETE FROM documents WHERE id = $1`, id)
	})

	occurred := time.Date(2026, 1, 4, 12, 0, 0, 0, time.UTC)
	seedCall033Doc(t, pg, call033Fixture{
		id: id, sourceType: "call-log", sourceID: "call-log:zz033-pending:fff:ggg",
		title:   "incoming 통화 상대4",
		content: "상대방: 상대4\n통화 방향: incoming\n통화 시간: 45s\n[TRANSCRIPTION PENDING]",
		metadata: map[string]any{
			"contact_name": "상대4", "direction": "incoming", "duration_seconds": 45,
			"audio_file": "zz033-missing.m4a", "transcription": "pending",
		},
		occurredAt: occurred, updatedAt: occurred.Add(5 * time.Second),
	})

	if err := pg.RunMigrations(ctx, filepath.Join("..", "..", "migrations"), 1536); err != nil {
		t.Fatalf("re-run migrations: %v", err)
	}

	src, status, content, meta := docState033(t, pg, id)
	if src != "call" || status != "active" {
		t.Fatalf("(source_type, status) = (%q, %q), want (call, active)", src, status)
	}
	if content == "" || content == "상대방: 상대4\n통화 방향: incoming\n통화 시간: 45s\n[TRANSCRIPTION PENDING]" {
		t.Errorf("content was not rebuilt: %q", content)
	}
	for _, want := range []string{"상대4", "incoming", "45s"} {
		if !strings.Contains(content, want) {
			t.Errorf("rebuilt content %q missing expected fragment %q", content, want)
		}
	}
	if strings.Contains(content, "TRANSCRIPTION PENDING") {
		t.Errorf("rebuilt content still contains the PENDING placeholder: %q", content)
	}
	if meta["transcription"] != "pending" {
		t.Errorf("metadata.transcription = %v, want \"pending\" (a recording genuinely never arrived)", meta["transcription"])
	}
}
