package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/collector/smsmap"
	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/model"
)

// writeSidecar writes a JSON sidecar file at audioPath + ".meta.json".
// It is a test-only helper that mirrors the ingest-recording handler logic.
func writeSidecar(t *testing.T, audioPath string, fields map[string]any) {
	t.Helper()
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("writeSidecar marshal: %v", err)
	}
	if err := os.WriteFile(audioPath+".meta.json", data, 0o644); err != nil {
		t.Fatalf("writeSidecar write: %v", err)
	}
}

// --- Unit tests for readRecordingSidecar ---

// TestReadRecordingSidecar_PresentAndValid verifies that a well-formed sidecar
// returns the expected fields and true.
func TestReadRecordingSidecar_PresentAndValid(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	audioPath := filepath.Join(dir, "01012345678_20260101120000.m4a")

	writeSidecar(t, audioPath, map[string]any{
		"contact_name":     "Alice",
		"direction":        "incoming",
		"recording_type":   "call",
		"duration_seconds": 120,
	})

	got, ok := readRecordingSidecar(audioPath)
	if !ok {
		t.Fatal("readRecordingSidecar returned ok=false for existing sidecar")
	}

	if got["contact_name"] != "Alice" {
		t.Errorf("contact_name = %v, want Alice", got["contact_name"])
	}
	if got["direction"] != "incoming" {
		t.Errorf("direction = %v, want incoming", got["direction"])
	}
	if got["recording_type"] != "call" {
		t.Errorf("recording_type = %v, want call", got["recording_type"])
	}
	// duration_seconds is decoded as float64 from JSON by default.
	dur, _ := got["duration_seconds"].(int)
	if dur != 120 {
		t.Errorf("duration_seconds = %v (type %T), want 120", got["duration_seconds"], got["duration_seconds"])
	}
}

// TestReadRecordingSidecar_Absent verifies that a missing sidecar returns
// (nil, false) without error.
func TestReadRecordingSidecar_Absent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	audioPath := filepath.Join(dir, "no-sidecar.m4a")

	got, ok := readRecordingSidecar(audioPath)
	if ok {
		t.Errorf("readRecordingSidecar returned ok=true for absent sidecar, got %v", got)
	}
	if got != nil {
		t.Errorf("readRecordingSidecar returned non-nil map for absent sidecar: %v", got)
	}
}

// TestReadRecordingSidecar_Garbage verifies that an unparseable sidecar returns
// (nil, false) rather than an error.
func TestReadRecordingSidecar_Garbage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	audioPath := filepath.Join(dir, "bad.m4a")

	sidecarPath := audioPath + ".meta.json"
	if err := os.WriteFile(sidecarPath, []byte("not-valid-json{{{}"), 0o644); err != nil {
		t.Fatalf("write garbage sidecar: %v", err)
	}

	got, ok := readRecordingSidecar(audioPath)
	if ok {
		t.Errorf("readRecordingSidecar returned ok=true for garbage sidecar, got %v", got)
	}
	if got != nil {
		t.Errorf("readRecordingSidecar returned non-nil map for garbage sidecar: %v", got)
	}
}

// TestReadRecordingSidecar_EmptyFields verifies that a sidecar with all empty
// string fields returns (nil, false) — no useful metadata to merge.
func TestReadRecordingSidecar_EmptyFields(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	audioPath := filepath.Join(dir, "empty.m4a")

	writeSidecar(t, audioPath, map[string]any{
		"contact_name":   "",
		"direction":      "",
		"recording_type": "",
		// duration_seconds omitted → zero value
	})

	got, ok := readRecordingSidecar(audioPath)
	if ok {
		t.Errorf("readRecordingSidecar returned ok=true for all-empty sidecar, got %v", got)
	}
	if got != nil {
		t.Errorf("readRecordingSidecar returned non-nil map for all-empty sidecar: %v", got)
	}
}

// TestReadRecordingSidecar_PartialFields verifies that only non-empty fields
// are included in the returned map (missing/empty fields are omitted).
func TestReadRecordingSidecar_PartialFields(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	audioPath := filepath.Join(dir, "partial.m4a")

	// Voice memo: no direction, no contact_name.
	writeSidecar(t, audioPath, map[string]any{
		"recording_type":   "voice-memo",
		"duration_seconds": 45,
	})

	got, ok := readRecordingSidecar(audioPath)
	if !ok {
		t.Fatal("readRecordingSidecar returned ok=false for partial sidecar")
	}

	if _, present := got["direction"]; present {
		t.Error("direction should not be present in voice-memo sidecar")
	}
	if _, present := got["contact_name"]; present {
		t.Error("contact_name should not be present in voice-memo sidecar with empty name")
	}
	if got["recording_type"] != "voice-memo" {
		t.Errorf("recording_type = %v, want voice-memo", got["recording_type"])
	}
}

// --- Integration test: Collect merges sidecar into transcript metadata ---

// --- Unit tests for callLogMergeSourceID (migration 033 call-unify) ---

// TestCallLogMergeSourceID_CallKindWithNumberHash verifies the formula
// matches smsmap.MapCall/ingest_recording.go exactly when the sidecar was
// written with PII hashing enabled (NumberHash populated, Number empty).
func TestCallLogMergeSourceID_CallKindWithNumberHash(t *testing.T) {
	t.Parallel()

	raw := recordingSidecarRaw{
		Kind: "call", DateMs: 1705311000000, DurationSeconds: 120,
		NumberHash: "abc123", ContactName: "Bob",
	}
	got, ok := callLogMergeSourceID(raw)
	if !ok {
		t.Fatal("callLogMergeSourceID returned ok=false for a well-formed call sidecar")
	}
	want := "call-log:1705311000000:abc123:" + smsmap.BodyShortHash("120")
	if got != want {
		t.Errorf("callLogMergeSourceID = %q, want %q", got, want)
	}
}

// TestCallLogMergeSourceID_CallKindWithRawNumber verifies the formula
// computes the same numHash smsmap.ShortHash(number) would, when hashing is
// disabled (Number populated, NumberHash empty).
func TestCallLogMergeSourceID_CallKindWithRawNumber(t *testing.T) {
	t.Parallel()

	raw := recordingSidecarRaw{
		Kind: "call", DateMs: 1705311000000, DurationSeconds: 30,
		Number: "010-1234-5678",
	}
	got, ok := callLogMergeSourceID(raw)
	if !ok {
		t.Fatal("callLogMergeSourceID returned ok=false for a well-formed call sidecar")
	}
	want := "call-log:1705311000000:" + smsmap.ShortHash("010-1234-5678") + ":" + smsmap.BodyShortHash("30")
	if got != want {
		t.Errorf("callLogMergeSourceID = %q, want %q", got, want)
	}
}

// TestCallLogMergeSourceID_VoiceMemoNeverMerges verifies a voice-memo
// sidecar (Kind != "call") never produces a merge target, even if it happens
// to carry a DateMs/Number — voice-memo has no call-log counterpart to merge
// into.
func TestCallLogMergeSourceID_VoiceMemoNeverMerges(t *testing.T) {
	t.Parallel()

	raw := recordingSidecarRaw{Kind: "voice-memo", DateMs: 1705311000000, Number: "010-0000-0000"}
	if _, ok := callLogMergeSourceID(raw); ok {
		t.Error("callLogMergeSourceID returned ok=true for a voice-memo sidecar")
	}
}

// TestCallLogMergeSourceID_MissingFieldsNeverMerge verifies that a sidecar
// missing DateMs, or missing both NumberHash and Number, never produces a
// merge target — the collector must never guess at the formula's inputs.
func TestCallLogMergeSourceID_MissingFieldsNeverMerge(t *testing.T) {
	t.Parallel()

	cases := []recordingSidecarRaw{
		{Kind: "call", DateMs: 0, NumberHash: "abc"},         // missing DateMs
		{Kind: "call", DateMs: 1705311000000},                // missing NumberHash and Number
		{Kind: "", DateMs: 1705311000000, NumberHash: "abc"}, // missing Kind entirely (pre-migration-033 sidecar)
	}
	for i, raw := range cases {
		if _, ok := callLogMergeSourceID(raw); ok {
			t.Errorf("case %d: callLogMergeSourceID returned ok=true for incomplete sidecar %+v", i, raw)
		}
	}
}

// TestWhisperCollector_Collect_SidecarMerged verifies that when a sidecar file
// exists alongside an audio file, the WhisperCollector merges its fields into
// the transcript document metadata while preserving the existing
// relative_path/audio_size/language/model fields.
func TestWhisperCollector_Collect_SidecarMerged(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const wantTranscript = "통화 전사 내용입니다."

	srv, _ := newWhisperTestServer(t, wantTranscript)

	mtime := time.Now().Add(-1 * time.Hour).UTC().Truncate(time.Second)
	audioPath := writeDummyAudio(t, dir, "01012345678_20260101120000.m4a", mtime)

	// Write sidecar alongside the audio file.
	writeSidecar(t, audioPath, map[string]any{
		"contact_name":     "Bob",
		"direction":        "incoming",
		"recording_type":   "call",
		"duration_seconds": 180,
	})

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Collect() returned %d docs, want 1", len(docs))
	}

	meta := docs[0].Metadata

	// Sidecar fields must be present.
	if meta["contact_name"] != "Bob" {
		t.Errorf("contact_name = %v, want Bob", meta["contact_name"])
	}
	if meta["direction"] != "incoming" {
		t.Errorf("direction = %v, want incoming", meta["direction"])
	}
	if meta["recording_type"] != "call" {
		t.Errorf("recording_type = %v, want call", meta["recording_type"])
	}
	if meta["duration_seconds"] != 180 {
		t.Errorf("duration_seconds = %v, want 180", meta["duration_seconds"])
	}

	// Original metadata fields must be preserved.
	if _, ok := meta["relative_path"]; !ok {
		t.Error("relative_path missing from metadata after sidecar merge")
	}
	if _, ok := meta["audio_size"]; !ok {
		t.Error("audio_size missing from metadata after sidecar merge")
	}
	if meta["language"] != "ko" {
		t.Errorf("language = %v, want ko", meta["language"])
	}
	if meta["model"] != "whisper-1" {
		t.Errorf("model = %v, want whisper-1", meta["model"])
	}
}

// TestWhisperCollector_Collect_NoSidecar_MetadataUnchanged verifies that when
// no sidecar exists (historical file), the transcript metadata contains only
// the standard fields and no recording-metadata keys.
func TestWhisperCollector_Collect_NoSidecar_MetadataUnchanged(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, _ := newWhisperTestServer(t, "전사 결과")

	mtime := time.Now().Add(-1 * time.Hour).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "voice-memo_20260101120000.m4a", mtime)
	// No sidecar written.

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Collect() returned %d docs, want 1", len(docs))
	}

	meta := docs[0].Metadata

	// Standard metadata must be present.
	if _, ok := meta["relative_path"]; !ok {
		t.Error("relative_path missing from metadata")
	}
	if _, ok := meta["audio_size"]; !ok {
		t.Error("audio_size missing from metadata")
	}

	// Recording-metadata keys must NOT be present when no sidecar exists.
	for _, key := range []string{"contact_name", "direction", "recording_type", "duration_seconds"} {
		if _, ok := meta[key]; ok {
			t.Errorf("metadata[%q] should not be present without a sidecar", key)
		}
	}
}

// TestWhisperCollector_Collect_SidecarDoesNotAlterSourceID verifies that the
// presence of a sidecar does not change the SourceID — it is always derived
// from the relative audio path.
func TestWhisperCollector_Collect_SidecarDoesNotAlterSourceID(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, _ := newWhisperTestServer(t, "source id test")

	mtime := time.Now().Add(-1 * time.Hour).UTC().Truncate(time.Second)
	audioPath := writeDummyAudio(t, dir, "01099998888_20260601090000.m4a", mtime)
	writeSidecar(t, audioPath, map[string]any{
		"contact_name":     "Carol",
		"recording_type":   "call",
		"duration_seconds": 60,
	})

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Collect() returned %d docs, want 1", len(docs))
	}

	wantSourceID := "transcript:01099998888_20260601090000.m4a"
	if docs[0].SourceID != wantSourceID {
		t.Errorf("SourceID = %q, want %q", docs[0].SourceID, wantSourceID)
	}
}

// TestWhisperCollector_Collect_SidecarJsonFilesIgnoredByWalk verifies that
// .meta.json sidecar files are NOT treated as audio files by the collector walk
// (they are skipped by the whisperAudioExts guard before the sidecar logic runs).
func TestWhisperCollector_Collect_SidecarJsonFilesIgnoredByWalk(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, _ := newWhisperTestServer(t, "only audio")

	now := time.Now().UTC().Truncate(time.Second)
	audioPath := writeDummyAudio(t, dir, "call.m4a", now.Add(-time.Hour))
	writeSidecar(t, audioPath, map[string]any{
		"recording_type": "call",
	})

	// The directory now contains call.m4a AND call.m4a.meta.json.
	// Only call.m4a should be processed.
	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 1 {
		t.Errorf("Collect() returned %d docs, want 1 (sidecar .meta.json must not be transcribed)", len(docs))
	}
}

// --- Integration tests: buildDocument's call-log merge decision (migration 033) ---

// TestWhisperCollector_Collect_CallSidecarMergesIntoCallLogID verifies that a
// recording with a Kind=="call" sidecar (carrying date_ms/number_hash/
// duration_seconds — the shape internal/api/ingest_recording.go writes) is
// emitted with the call-log-formula SourceID rather than its own
// transcript:{relPath} identity, so internal/scheduler routes it through
// store.AttachTranscript and merges it into the existing call document
// instead of creating a second one (model.SourceCall's doc comment).
func TestWhisperCollector_Collect_CallSidecarMergesIntoCallLogID(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const wantTranscript = "실제 통화 전사 내용."
	srv, _ := newWhisperTestServer(t, wantTranscript)

	mtime := time.Now().Add(-1 * time.Hour).UTC().Truncate(time.Second)
	audioName := "01012345678_20260101120000.m4a"
	audioPath := writeDummyAudio(t, dir, audioName, mtime)

	const dateMs = int64(1705311000000)
	const durationSec = 120
	const numHash = "abc123def456"
	writeSidecar(t, audioPath, map[string]any{
		"contact_name":     "Bob",
		"direction":        "incoming",
		"recording_type":   "call",
		"kind":             "call",
		"date_ms":          dateMs,
		"duration_seconds": durationSec,
		"number_hash":      numHash,
	})

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Collect() returned %d docs, want 1", len(docs))
	}
	doc := docs[0]

	if doc.SourceType != model.SourceCall {
		t.Errorf("SourceType = %q, want %q", doc.SourceType, model.SourceCall)
	}

	wantSourceID := fmt.Sprintf("call-log:%d:%s:%s", dateMs, numHash, smsmap.BodyShortHash(fmt.Sprintf("%d", durationSec)))
	if doc.SourceID != wantSourceID {
		t.Errorf("SourceID = %q, want %q (must merge into the call-log document, not its own transcript:{relPath} identity)", doc.SourceID, wantSourceID)
	}

	wantRawID := "transcript:" + audioName
	if doc.Metadata["transcript_source_id"] != wantRawID {
		t.Errorf("metadata[transcript_source_id] = %v, want %q (the raw audio identity, preserved for the ledger)", doc.Metadata["transcript_source_id"], wantRawID)
	}
	if doc.Metadata["transcription"] != "done" {
		t.Errorf("metadata[transcription] = %v, want \"done\"", doc.Metadata["transcription"])
	}
	if doc.Content != wantTranscript {
		t.Errorf("Content = %q, want the transcript text %q", doc.Content, wantTranscript)
	}
}

// TestWhisperCollector_Collect_VoiceMemoSidecarStaysStandalone verifies that a
// voice-memo sidecar (Kind=="voice-memo") — which carries no call identity to
// merge into — keeps the file's own transcript:{relPath} SourceID and does
// NOT set transcript_source_id/transcription, exactly like the no-sidecar
// case (TestWhisperCollector_Collect_NoSidecar_MetadataUnchanged).
func TestWhisperCollector_Collect_VoiceMemoSidecarStaysStandalone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, _ := newWhisperTestServer(t, "음성 메모 전사")

	mtime := time.Now().Add(-1 * time.Hour).UTC().Truncate(time.Second)
	audioName := "voice-memo_20260101120000.m4a"
	audioPath := writeDummyAudio(t, dir, audioName, mtime)

	writeSidecar(t, audioPath, map[string]any{
		"recording_type":   "voice-memo",
		"kind":             "voice-memo",
		"date_ms":          1705311000000,
		"duration_seconds": 45,
	})

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Collect() returned %d docs, want 1", len(docs))
	}
	doc := docs[0]

	if doc.SourceType != model.SourceCall {
		t.Errorf("SourceType = %q, want %q", doc.SourceType, model.SourceCall)
	}
	wantSourceID := "transcript:" + audioName
	if doc.SourceID != wantSourceID {
		t.Errorf("SourceID = %q, want %q (voice-memo has no call-log document to merge into)", doc.SourceID, wantSourceID)
	}
	if _, ok := doc.Metadata["transcript_source_id"]; ok {
		t.Errorf("metadata[transcript_source_id] should not be set for a standalone voice-memo document, got %v", doc.Metadata["transcript_source_id"])
	}
	if _, ok := doc.Metadata["transcription"]; ok {
		t.Errorf("metadata[transcription] should not be set for a standalone voice-memo document, got %v", doc.Metadata["transcription"])
	}
}
