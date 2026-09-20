package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Real-database verification of DocumentStore.AttachTranscript (migration
// 033's call-unify live counterpart — internal/collector/whisper.go merges a
// transcript into an existing call-log document through this method).
//
// Uses the full migration set bootstrapped by TestMain.
//
// This project has already shipped SQL that compiled, passed stub tests, and
// failed at runtime (EXCLUDED referenced inside RETURNING — see agent memory
// feedback_postgres_returning_excluded); AttachTranscript's CTE-based change
// detection is exactly that kind of logic, so it is verified here against a
// real PostgreSQL instance rather than only in Go.
// ---------------------------------------------------------------------------

// attachTestPrefix scopes both the seeded rows and this file's cleanup DELETE.
const attachTestPrefix = "zz-dummy-attach033-"

func attachTestDB(t *testing.T) *Postgres {
	t.Helper()
	pg := srcTestDB(t)

	t.Cleanup(func() {
		_, _ = pg.pool.Exec(context.Background(),
			`DELETE FROM documents WHERE source_id LIKE $1`, attachTestPrefix+"%")
	})
	return pg
}

// TestDB_AttachTranscript_MergesMetadataAndPreservesCallLogFields verifies
// the three defining properties of a merge (existing row, different from a
// plain Upsert): content/embedding are REPLACED, metadata is MERGED (the
// pre-existing call-log-only fields like contact_name/retention survive),
// and title/occurred_at are left untouched when they were already set.
func TestDB_AttachTranscript_MergesMetadataAndPreservesCallLogFields(t *testing.T) {
	pg := attachTestDB(t)
	store := NewDocumentStore(pg)
	ctx := context.Background()

	sourceID := attachTestPrefix + uuid.NewString()
	callLogOccurred := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Seed the call-log document exactly like smsmap.MapCall/ingest_recording.go
	// would: short summary content, call-log-only metadata, retention tag
	// (simulating a later classification worker having already tagged it).
	seed := &model.Document{
		SourceType: model.SourceCall,
		SourceID:   sourceID,
		Title:      "incoming 통화 상대1",
		Content:    "상대방: 상대1\n통화 방향: incoming\n시각: 2026-01-01\n통화 시간: 30s",
		Metadata: map[string]any{
			"contact_name":     "상대1",
			"direction":        "incoming",
			"duration_seconds": 30,
			"retention":        "keep",
			"transcription":    "pending",
		},
		OccurredAt:  &callLogOccurred,
		CollectedAt: callLogOccurred,
	}
	if err := store.Upsert(ctx, seed); err != nil {
		t.Fatalf("seed call-log document: %v", err)
	}

	// AttachTranscript with the transcript's own content + a METADATA PATCH
	// (transcript-only fields) and a DIFFERENT occurred_at (must be ignored —
	// the call-log's own occurred_at is authoritative). Content is unique per
	// test run (embeds sourceID) so the call-content dedup guard (issue #134)
	// never collides with a leftover fixture from a different test file
	// sharing this database.
	whisperOccurred := time.Date(2026, 1, 1, 12, 30, 0, 0, time.UTC) // wrong on purpose
	transcriptContent := "실제 통화 전사 내용입니다: " + sourceID
	merge := &model.Document{
		SourceType: model.SourceCall,
		SourceID:   sourceID,
		Title:      "01012340001_20260101120000", // filename-stem title — must NOT overwrite
		Content:    transcriptContent,
		Metadata: map[string]any{
			"transcript_source_id": "transcript:call/01012340001_20260101120000.m4a",
			"transcription":        "done",
			"model":                "whisper-1",
			"language":             "ko",
		},
		OccurredAt:  &whisperOccurred,
		CollectedAt: whisperOccurred,
	}
	changed, err := store.AttachTranscript(ctx, merge)
	if err != nil {
		t.Fatalf("AttachTranscript: %v", err)
	}
	if !changed {
		t.Error("contentChanged = false, want true (content actually changed)")
	}

	got, err := store.GetByID(ctx, merge.ID)
	if err != nil {
		t.Fatalf("GetByID after merge: %v", err)
	}

	if got.Content != transcriptContent {
		t.Errorf("Content = %q, want the transcript content %q", got.Content, transcriptContent)
	}
	if got.Title != "incoming 통화 상대1" {
		t.Errorf("Title = %q, want the call-log title to survive (not overwritten by the filename-stem title)", got.Title)
	}
	if !got.OccurredAt.Equal(callLogOccurred) {
		t.Errorf("OccurredAt = %v, want the call-log's own %v (authoritative, must not be overwritten by whisper's guess)", got.OccurredAt, callLogOccurred)
	}

	// Metadata: call-log-only fields must survive; transcript fields must be added.
	if got.Metadata["contact_name"] != "상대1" {
		t.Errorf("metadata.contact_name = %v, want 상대1 (must survive the merge)", got.Metadata["contact_name"])
	}
	if got.Metadata["retention"] != "keep" {
		t.Errorf("metadata.retention = %v, want keep (must survive the merge)", got.Metadata["retention"])
	}
	if got.Metadata["direction"] != "incoming" {
		t.Errorf("metadata.direction = %v, want incoming (must survive the merge)", got.Metadata["direction"])
	}
	if got.Metadata["transcription"] != "done" {
		t.Errorf("metadata.transcription = %v, want done", got.Metadata["transcription"])
	}
	if got.Metadata["model"] != "whisper-1" {
		t.Errorf("metadata.model = %v, want whisper-1", got.Metadata["model"])
	}
}

// TestDB_AttachTranscript_InsertsWhenNoExistingDocument verifies that when no
// document exists at (source_type, source_id) yet, AttachTranscript falls
// back to a plain INSERT using its own Title/OccurredAt/Metadata — the same
// outcome Upsert would have produced for a standalone transcript.
func TestDB_AttachTranscript_InsertsWhenNoExistingDocument(t *testing.T) {
	pg := attachTestDB(t)
	store := NewDocumentStore(pg)
	ctx := context.Background()

	sourceID := attachTestPrefix + uuid.NewString()
	occurred := time.Now().UTC().Truncate(time.Second)

	doc := &model.Document{
		SourceType:  model.SourceCall,
		SourceID:    sourceID,
		Title:       "call-2026",
		Content:     "혼자 남은 전사 문서.",
		Metadata:    map[string]any{"transcription": "done"},
		OccurredAt:  &occurred,
		CollectedAt: occurred,
	}
	changed, err := store.AttachTranscript(ctx, doc)
	if err != nil {
		t.Fatalf("AttachTranscript (insert branch): %v", err)
	}
	if !changed {
		t.Error("contentChanged = false, want true for a fresh insert")
	}
	if doc.ID == uuid.Nil {
		t.Error("doc.ID was not populated after insert")
	}

	got, err := store.GetByID(ctx, doc.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Content != "혼자 남은 전사 문서." {
		t.Errorf("Content = %q, want the inserted content", got.Content)
	}
	if got.Metadata["transcription"] != "done" {
		t.Errorf("metadata.transcription = %v, want done", got.Metadata["transcription"])
	}
}

// TestDB_AttachTranscript_UnchangedContent_ReportsFalse verifies that
// re-calling AttachTranscript with byte-identical content on the SAME
// source_id reports contentChanged=false, mirroring UpsertTracked's
// idempotency guarantee (so the scheduler can skip re-chunking/re-embedding).
func TestDB_AttachTranscript_UnchangedContent_ReportsFalse(t *testing.T) {
	pg := attachTestDB(t)
	store := NewDocumentStore(pg)
	ctx := context.Background()

	sourceID := attachTestPrefix + uuid.NewString()
	occurred := time.Now().UTC().Truncate(time.Second)

	doc := &model.Document{
		SourceType: model.SourceCall, SourceID: sourceID,
		Title: "call", Content: "동일한 내용.", Metadata: map[string]any{},
		OccurredAt: &occurred, CollectedAt: occurred,
	}
	if _, err := store.AttachTranscript(ctx, doc); err != nil {
		t.Fatalf("first AttachTranscript: %v", err)
	}

	doc2 := &model.Document{
		SourceType: model.SourceCall, SourceID: sourceID,
		Title: "call", Content: "동일한 내용.", Metadata: map[string]any{"transcription": "done"},
		OccurredAt: &occurred, CollectedAt: occurred,
	}
	changed, err := store.AttachTranscript(ctx, doc2)
	if err != nil {
		t.Fatalf("second AttachTranscript: %v", err)
	}
	if changed {
		t.Error("contentChanged = true on a re-call with byte-identical content, want false")
	}
}

// TestDB_AttachTranscript_DuplicateContentDifferentSourceID_Rejected verifies
// that the same call-content dedup guard used by Upsert/UpsertTracked (issue
// #134, callDupCheckQuery) also protects AttachTranscript: merging identical
// transcript content under a second, different source_id must be rejected
// with ErrDuplicateTranscript rather than silently duplicating the content.
func TestDB_AttachTranscript_DuplicateContentDifferentSourceID_Rejected(t *testing.T) {
	pg := attachTestDB(t)
	store := NewDocumentStore(pg)
	ctx := context.Background()

	occurred := time.Now().UTC().Truncate(time.Second)
	const dupContent = "완전히 동일한 전사 내용입니다."

	first := &model.Document{
		SourceType: model.SourceCall, SourceID: attachTestPrefix + uuid.NewString(),
		Title: "first", Content: dupContent, Metadata: map[string]any{},
		OccurredAt: &occurred, CollectedAt: occurred,
	}
	if _, err := store.AttachTranscript(ctx, first); err != nil {
		t.Fatalf("seed first document: %v", err)
	}

	second := &model.Document{
		SourceType: model.SourceCall, SourceID: attachTestPrefix + uuid.NewString(),
		Title: "second", Content: dupContent, Metadata: map[string]any{},
		OccurredAt: &occurred, CollectedAt: occurred,
	}
	_, err := store.AttachTranscript(ctx, second)
	if !errors.Is(err, ErrDuplicateTranscript) {
		t.Errorf("AttachTranscript with duplicate content under a different source_id: err = %v, want ErrDuplicateTranscript", err)
	}
}

func TestDB_AttachTranscriptRedactedFieldsReplaceLegacyContact(t *testing.T) {
	pg := attachTestDB(t)
	s := NewDocumentStore(pg)
	ctx := context.Background()
	seed := &model.Document{SourceType: model.SourceCall, SourceID: attachTestPrefix + uuid.NewString(), Title: "legacy person", Content: "old transcript", Metadata: map[string]any{"contact_name": "legacy person", "number": "01012345678", "direction": "incoming"}, CollectedAt: time.Now()}
	if err := s.Upsert(ctx, seed); err != nil {
		t.Fatal(err)
	}
	_, err := pg.pool.Exec(ctx, `UPDATE documents SET title_summary='legacy person' WHERE id=$1`, seed.ID)
	if err != nil {
		t.Fatal(err)
	}
	transcript := &model.Document{SourceType: model.SourceCall, SourceID: seed.SourceID, Title: "protected title", Content: "protected transcript", Metadata: map[string]any{"pii_name_redacted": true, "contact_name": "[REDACTED]", "number": "[REDACTED]"}, CollectedAt: time.Now()}
	if _, err := s.AttachTranscript(ctx, transcript); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.AttachTranscript(ctx, transcript); err != nil || !changed {
		t.Fatalf("protected identical body must trigger downstream rebuild: changed=%v err=%v", changed, err)
	}
	got, err := s.GetByID(ctx, transcript.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "protected title" || got.Metadata["contact_name"] != "[REDACTED]" || got.Metadata["number"] != "[REDACTED]" || got.Metadata["direction"] != "incoming" {
		t.Fatalf("protected merge mismatch: title=%q", got.Title)
	}
	var cleared bool
	if err := pg.pool.QueryRow(ctx, `SELECT title_summary IS NULL AND summary_embedding IS NULL FROM documents WHERE id=$1`, got.ID).Scan(&cleared); err != nil || !cleared {
		t.Fatalf("legacy summary retained: %v", err)
	}
}
