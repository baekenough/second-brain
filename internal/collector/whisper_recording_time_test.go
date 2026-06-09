package collector

// whisper_recording_time_test.go — Tests for recordingTime filename parser
// and the production bug fix: cutover floor must use the filename-parsed
// recording date, not the file mtime (which reflects staging time).

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/config"
)

// sentinel mtime used in fallback tests — chosen to be clearly distinct from
// any recording date parsed from a filename.
var sentinelMtime = time.Date(2025, 12, 1, 8, 0, 0, 0, time.UTC)

// TestRecordingTime_VoiceRecorder tests the Voice Recorder filename pattern:
// <label>_YYMMDD_HHMMSS.<ext>  (2-digit year → +2000)
func TestRecordingTime_VoiceRecorder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		filename string
		wantYear int
		wantMon  time.Month
		wantDay  int
		wantHour int
		wantMin  int
		wantSec  int
	}{
		{
			name:     "example from issue (Korean label)",
			filename: "메디웨일_260120_120138.m4a",
			wantYear: 2026, wantMon: 1, wantDay: 20,
			wantHour: 12, wantMin: 1, wantSec: 38,
		},
		{
			name:     "ASCII label",
			filename: "meeting_260530_093045.wav",
			wantYear: 2026, wantMon: 5, wantDay: 30,
			wantHour: 9, wantMin: 30, wantSec: 45,
		},
		{
			name:     "label with underscores",
			filename: "work_call_recording_251231_235959.m4a",
			wantYear: 2025, wantMon: 12, wantDay: 31,
			wantHour: 23, wantMin: 59, wantSec: 59,
		},
		{
			name:     "two-digit year at boundary 00 maps to 2000",
			filename: "label_000101_000000.mp3",
			wantYear: 2000, wantMon: 1, wantDay: 1,
			wantHour: 0, wantMin: 0, wantSec: 0,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := recordingTime(tc.filename, sentinelMtime)
			if got.Year() != tc.wantYear {
				t.Errorf("Year = %d, want %d", got.Year(), tc.wantYear)
			}
			if got.Month() != tc.wantMon {
				t.Errorf("Month = %v, want %v", got.Month(), tc.wantMon)
			}
			if got.Day() != tc.wantDay {
				t.Errorf("Day = %d, want %d", got.Day(), tc.wantDay)
			}
			if got.Hour() != tc.wantHour {
				t.Errorf("Hour = %d, want %d", got.Hour(), tc.wantHour)
			}
			if got.Minute() != tc.wantMin {
				t.Errorf("Minute = %d, want %d", got.Minute(), tc.wantMin)
			}
			if got.Second() != tc.wantSec {
				t.Errorf("Second = %d, want %d", got.Second(), tc.wantSec)
			}
			if got.Equal(sentinelMtime) {
				t.Error("got sentinel mtime — pattern should have matched")
			}
		})
	}
}

// TestRecordingTime_TPhone tests the TPhoneCallRecords filename pattern:
// <number>_YYYYMMDDHHMMSS[{-N}].<ext>
func TestRecordingTime_TPhone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		filename string
		wantYear int
		wantMon  time.Month
		wantDay  int
		wantHour int
		wantMin  int
		wantSec  int
	}{
		{
			name:     "example from issue (no suffix)",
			filename: "01025777190_20260327202518.m4a",
			wantYear: 2026, wantMon: 3, wantDay: 27,
			wantHour: 20, wantMin: 25, wantSec: 18,
		},
		{
			name:     "with -1 suffix variant",
			filename: "01025777190_20260327202518-1.m4a",
			wantYear: 2026, wantMon: 3, wantDay: 27,
			wantHour: 20, wantMin: 25, wantSec: 18,
		},
		{
			name:     "with -2 suffix variant",
			filename: "01012345678_20260120150000-2.mp3",
			wantYear: 2026, wantMon: 1, wantDay: 20,
			wantHour: 15, wantMin: 0, wantSec: 0,
		},
		{
			name:     "different number format",
			filename: "821025777190_20251215083000.m4a",
			wantYear: 2025, wantMon: 12, wantDay: 15,
			wantHour: 8, wantMin: 30, wantSec: 0,
		},
		{
			name:     "year 2000 boundary",
			filename: "test_20000101000000.m4a",
			wantYear: 2000, wantMon: 1, wantDay: 1,
			wantHour: 0, wantMin: 0, wantSec: 0,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := recordingTime(tc.filename, sentinelMtime)
			if got.Year() != tc.wantYear {
				t.Errorf("Year = %d, want %d", got.Year(), tc.wantYear)
			}
			if got.Month() != tc.wantMon {
				t.Errorf("Month = %v, want %v", got.Month(), tc.wantMon)
			}
			if got.Day() != tc.wantDay {
				t.Errorf("Day = %d, want %d", got.Day(), tc.wantDay)
			}
			if got.Hour() != tc.wantHour {
				t.Errorf("Hour = %d, want %d", got.Hour(), tc.wantHour)
			}
			if got.Minute() != tc.wantMin {
				t.Errorf("Minute = %d, want %d", got.Minute(), tc.wantMin)
			}
			if got.Second() != tc.wantSec {
				t.Errorf("Second = %d, want %d", got.Second(), tc.wantSec)
			}
			if got.Equal(sentinelMtime) {
				t.Error("got sentinel mtime — pattern should have matched")
			}
		})
	}
}

// TestRecordingTime_Fallback verifies that filenames matching neither pattern
// return the mtime unchanged.
func TestRecordingTime_Fallback(t *testing.T) {
	t.Parallel()

	fallbackCases := []string{
		"old.m4a",
		"new.m4a",
		"ancient1.wav",
		"call-2024.m4a", // has year but not in either pattern
		"notes.txt",
		"recording.mp3", // no timestamp
		"20260101.m4a",  // no underscore separator before date
		"",              // empty
	}

	for _, filename := range fallbackCases {
		filename := filename
		t.Run("fallback:"+filename, func(t *testing.T) {
			t.Parallel()
			got := recordingTime(filename, sentinelMtime)
			if !got.Equal(sentinelMtime) {
				t.Errorf("filename %q: got %v, want sentinel mtime %v (fallback expected)", filename, got, sentinelMtime)
			}
		})
	}
}

// --- Production bug regression tests ---
//
// TestWhisperCollector_Cutover_FilenameDate_SkipsHistorical is the key regression
// test for issue #110. It reproduces the exact production failure:
//
//   - Cutover floor is set to 2026-05-30.
//   - A historical recording from 2026-01-20 is staged onto the server AFTER
//     the cutover (mtime is recent).
//   - Old code: mtime is used → file passes cutover check → re-transcribed (wrong).
//   - New code: filename date 2026-01-20 is used → file fails cutover → skipped (correct).
func TestWhisperCollector_Cutover_FilenameDate_SkipsHistorical(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, _ := newWhisperTestServer(t, "should not appear")

	// Cutover: 2026-05-30 00:00:00 Local
	cutover := time.Date(2026, 5, 30, 0, 0, 0, 0, time.Local)

	// Historical file: filename encodes 2026-01-20 (before cutover),
	// but mtime is set to "now+1h" (after cutover) — simulating a staging copy.
	recentMtime := time.Now().Add(time.Hour)
	historicalName := "메디웨일_260120_120138.m4a"
	writeDummyAudio(t, dir, historicalName, recentMtime)

	// Genuinely new file: filename date 2026-06-01 (after cutover).
	newName := "메디웨일_260601_090000.m4a"
	writeDummyAudio(t, dir, newName, recentMtime)

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)
	c.WithCutover(cutover)
	// Both files are unindexed — without the fix, IndexAware would transcribe both.
	c.WithIndexedIDs(map[string]struct{}{})

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(docs) != 1 {
		sourceIDs := make([]string, len(docs))
		for i, d := range docs {
			sourceIDs[i] = d.SourceID
		}
		t.Fatalf("got %d docs (sourceIDs: %v), want 1 — historical file must be suppressed by filename date", len(docs), sourceIDs)
	}
	want := "transcript:" + newName
	if docs[0].SourceID != want {
		t.Errorf("SourceID = %q, want %q", docs[0].SourceID, want)
	}
}

// TestWhisperCollector_Cutover_FilenameDate_TPhone_SkipsHistorical is the same
// regression test for the TPhoneCallRecords pattern, including the -1 suffix variant.
func TestWhisperCollector_Cutover_FilenameDate_TPhone_SkipsHistorical(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, _ := newWhisperTestServer(t, "should not appear")

	cutover := time.Date(2026, 5, 30, 0, 0, 0, 0, time.Local)

	recentMtime := time.Now().Add(time.Hour)

	// Historical TPhone recording: 2026-03-27 (before cutover), staged recently.
	historicalName := "01025777190_20260327202518.m4a"
	writeDummyAudio(t, dir, historicalName, recentMtime)

	// Historical with -1 suffix: also before cutover, should be suppressed.
	historicalSuffixName := "01025777190_20260327202518-1.m4a"
	writeDummyAudio(t, dir, historicalSuffixName, recentMtime)

	// Recent TPhone recording: 2026-06-01 (after cutover).
	newName := "01025777190_20260601093000.m4a"
	writeDummyAudio(t, dir, newName, recentMtime)

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)
	c.WithCutover(cutover)
	c.WithIndexedIDs(map[string]struct{}{})

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(docs) != 1 {
		sourceIDs := make([]string, len(docs))
		for i, d := range docs {
			sourceIDs[i] = d.SourceID
		}
		t.Fatalf("got %d docs (sourceIDs: %v), want 1 — historical TPhone files must be suppressed", len(docs), sourceIDs)
	}
	want := "transcript:" + newName
	if docs[0].SourceID != want {
		t.Errorf("SourceID = %q, want %q", docs[0].SourceID, want)
	}
}

// TestWhisperCollector_Cutover_FilenameDate_Fallback_UsesMtime verifies that
// when a file has an unparseable name, the cutover check falls back to mtime —
// preserving existing behaviour.
func TestWhisperCollector_Cutover_FilenameDate_Fallback_UsesMtime(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, _ := newWhisperTestServer(t, "fallback transcription")

	now := time.Now()
	cutover := now.Add(-24 * time.Hour)

	// File with NO recognisable timestamp in name; mtime before cutover → suppressed.
	oldMtime := cutover.Add(-time.Hour)
	writeDummyAudio(t, dir, "recording.m4a", oldMtime)

	// File with NO recognisable timestamp in name; mtime after cutover → transcribed.
	newMtime := cutover.Add(time.Hour)
	writeDummyAudio(t, dir, "recording2.m4a", newMtime)

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)
	c.WithCutover(cutover)
	c.WithIndexedIDs(map[string]struct{}{})

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(docs) != 1 {
		t.Fatalf("got %d docs, want 1 (unparseable names fall back to mtime)", len(docs))
	}
	if docs[0].SourceID != "transcript:recording2.m4a" {
		t.Errorf("SourceID = %q, want transcript:recording2.m4a", docs[0].SourceID)
	}
}

// TestWhisperCollector_Cutover_FilenameDate_SubdirPath ensures that recordingTime
// is called with d.Name() (just the filename), not the full path, so that
// directory path components don't confuse the regex.
func TestWhisperCollector_Cutover_FilenameDate_SubdirPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Subdirectory name looks like a TPhone pattern with a historical date.
	// The FILE inside has a post-cutover date and should be processed.
	subDir := filepath.Join(dir, "01025777190_20260120120000")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	srv, _ := newWhisperTestServer(t, "subdir transcription")

	cutover := time.Date(2026, 5, 30, 0, 0, 0, 0, time.Local)
	recentMtime := time.Now().Add(time.Hour)

	// Filename date: 2026-06-01 (after cutover) — should pass.
	newName := "01025777190_20260601093000.m4a"
	writeDummyAudio(t, subDir, newName, recentMtime)

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)
	c.WithCutover(cutover)
	c.WithIndexedIDs(map[string]struct{}{})

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(docs) != 1 {
		t.Fatalf("got %d docs, want 1 (file in date-named subdir should be processed by filename date)", len(docs))
	}
}
