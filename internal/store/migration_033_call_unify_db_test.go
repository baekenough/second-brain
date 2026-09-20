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
// TestMain applies the full migration set once before any tests run. These
// tests seed legacy rows and explicitly reapply migrations to verify upgrades.
// TEST_DATABASE_URL must point to a throwaway database with pgvector + pg_bigm
// (deploy/postgres/Dockerfile). An incompatible database fails bootstrap.
// ---------------------------------------------------------------------------

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
