package collector

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/config"
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
