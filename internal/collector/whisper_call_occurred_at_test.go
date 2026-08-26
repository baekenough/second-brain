package collector

// whisper_call_occurred_at_test.go — Tests for callRecordingOccurredAt and its
// wiring into buildDocument's OccurredAt field.
//
// Production bug: OccurredAt was set from the audio file's mtime (when the
// server SAVED the file), not the actual call time. Normally the phone
// uploads within seconds of the call ending so the difference is invisible.
// When upload is delayed (server outage, etc.) mtime drifts hours away from
// the real call time. The TPhoneCallRecords filename embeds the true call
// time as a UTC timestamp (<number>_YYYYMMDDHHMMSS[-N].<ext>) — confirmed by
// cross-referencing filename timestamps against independently accurate
// call-log timestamps, which agreed to the second. This file locks in that
// fix and specifically guards against parsing the filename timestamp as
// time.Local (which is wrong on any non-UTC host, e.g. a KST dev machine).

import (
	"context"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/config"
)

// TestCallRecordingOccurredAt_NormalFilename verifies that a well-formed
// TPhoneCallRecords filename produces an occurred_at parsed from the
// filename, in UTC, matching the embedded digits exactly.
func TestCallRecordingOccurredAt_NormalFilename(t *testing.T) {
	t.Parallel()

	// mtime close to the filename time (normal case — upload right after the call).
	mtime := time.Date(2026, 8, 26, 5, 44, 40, 0, time.UTC)
	now := time.Date(2026, 8, 26, 6, 0, 0, 0, time.UTC)
	filename := "01025777190_20260826054434.m4a"

	got := callRecordingOccurredAt(filename, mtime, now)

	want := time.Date(2026, 8, 26, 5, 44, 34, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("callRecordingOccurredAt() = %v, want %v", got, want)
	}
	if got.Location() != time.UTC {
		t.Errorf("callRecordingOccurredAt() location = %v, want UTC", got.Location())
	}
}

// TestCallRecordingOccurredAt_MtimeDriftRegression is the key regression test
// for the production incident: a 16-hour permission-outage delay between the
// call (14:44:34 KST = 05:44:34 UTC) and the server saving the file
// (22:16 KST that night). Before the fix, OccurredAt used mtime and recorded
// the call ~16.5 hours late. After the fix, the filename timestamp must win.
func TestCallRecordingOccurredAt_MtimeDriftRegression(t *testing.T) {
	t.Parallel()

	filename := "01025777190_20260826054434.m4a" // call at 05:44:34 UTC (14:44:34 KST)
	// Server saved the file ~16.5 hours later (22:16 KST == 13:16 UTC next reckoning
	// within the same UTC day here since 05:44 + 16.5h stays before midnight UTC).
	mtime := time.Date(2026, 8, 26, 22, 16, 0, 0, time.UTC)
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)

	got := callRecordingOccurredAt(filename, mtime, now)

	wantCallTime := time.Date(2026, 8, 26, 5, 44, 34, 0, time.UTC)
	if !got.Equal(wantCallTime) {
		t.Errorf("callRecordingOccurredAt() = %v, want filename-derived call time %v (mtime %v must NOT be used)",
			got, wantCallTime, mtime)
	}
	if got.Equal(mtime) {
		t.Error("callRecordingOccurredAt() returned mtime — the exact bug this function fixes")
	}

	gap := mtime.Sub(got)
	if gap < 16*time.Hour {
		t.Fatalf("test setup error: mtime/filename gap is only %v, want >= 16h to exercise the regression", gap)
	}
}

// TestCallRecordingOccurredAt_UnparseableFilename_FallsBackToMtime verifies
// that filenames not matching the reTPhone pattern fall back to mtime
// unchanged (legacy files, voice memos, etc).
func TestCallRecordingOccurredAt_UnparseableFilename_FallsBackToMtime(t *testing.T) {
	t.Parallel()

	mtime := time.Date(2025, 12, 1, 8, 0, 0, 0, time.UTC)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	filenames := []string{
		"old_recording.m4a",
		"notes.txt",
		"20260101.m4a", // no underscore separator before the date
		"",
	}

	for _, filename := range filenames {
		filename := filename
		t.Run("fallback:"+filename, func(t *testing.T) {
			t.Parallel()
			got := callRecordingOccurredAt(filename, mtime, now)
			if !got.Equal(mtime) {
				t.Errorf("filename %q: got %v, want mtime fallback %v", filename, got, mtime)
			}
		})
	}
}

// TestCallRecordingOccurredAt_FutureTimestamp_FallsBackToMtime verifies the
// sanity check: a filename timestamp in the future relative to now is
// rejected as implausible (untrusted, phone-controlled input) and mtime is
// used instead.
func TestCallRecordingOccurredAt_FutureTimestamp_FallsBackToMtime(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	mtime := now.Add(-time.Minute)

	// Filename claims a call one day in the future relative to now.
	filename := "01025777190_20260827120000.m4a"

	got := callRecordingOccurredAt(filename, mtime, now)
	if !got.Equal(mtime) {
		t.Errorf("callRecordingOccurredAt() = %v, want mtime fallback %v (future timestamp must be rejected)", got, mtime)
	}
}

// TestCallRecordingOccurredAt_TooOldTimestamp_FallsBackToMtime verifies the
// other half of the sanity check: a year before 2000 is implausible.
func TestCallRecordingOccurredAt_TooOldTimestamp_FallsBackToMtime(t *testing.T) {
	t.Parallel()

	mtime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Year 1999 — before the isPlausibleRecordingTime floor of 2000.
	filename := "01025777190_19991231235959.m4a"

	got := callRecordingOccurredAt(filename, mtime, now)
	if !got.Equal(mtime) {
		t.Errorf("callRecordingOccurredAt() = %v, want mtime fallback %v (year < 2000 must be rejected)", got, mtime)
	}
}

// TestCallRecordingOccurredAt_RejectsKSTMisparse guards specifically against
// the regression this bug fix could introduce: parsing the filename
// timestamp with time.Local (or any explicit KST offset) instead of
// time.UTC. If the implementation ever regresses to that, this test fails
// because the result would be exactly 9 hours off from the UTC-correct value
// (Asia/Seoul is UTC+9).
func TestCallRecordingOccurredAt_RejectsKSTMisparse(t *testing.T) {
	t.Parallel()

	filename := "01025777190_20260826054434.m4a"
	mtime := time.Date(2026, 8, 26, 6, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)

	got := callRecordingOccurredAt(filename, mtime, now)

	correctUTC := time.Date(2026, 8, 26, 5, 44, 34, 0, time.UTC)
	if !got.Equal(correctUTC) {
		t.Fatalf("callRecordingOccurredAt() = %v, want %v (parsed as UTC)", got, correctUTC)
	}

	kst, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Skipf("Asia/Seoul tzdata unavailable in this environment: %v", err)
	}
	// What the buggy KST-parse would have produced: the same wall-clock
	// digits, but interpreted as KST instead of UTC. Converted to UTC for
	// comparison, this is 9 hours earlier than the correct UTC value.
	misparsedAsKST := time.Date(2026, 8, 26, 5, 44, 34, 0, kst)
	if got.Equal(misparsedAsKST.UTC()) {
		t.Fatalf("callRecordingOccurredAt() matched the KST-misparse value %v — timestamp must be parsed as UTC, not Local/KST",
			misparsedAsKST.UTC())
	}
	if diff := got.Sub(misparsedAsKST.UTC()); diff != 9*time.Hour {
		t.Fatalf("sanity check on test itself failed: expected exactly 9h difference from KST-misparse, got %v", diff)
	}
}

// TestWhisperCollector_BuildDocument_OccurredAt_UsesFilenameNotMtime is the
// end-to-end regression test: it drives the file through the real Collect()
// pipeline (walk -> transcribe -> buildDocument) and asserts the emitted
// model.Document.OccurredAt reflects the filename-derived call time, not the
// file's mtime, when the two diverge by many hours.
func TestWhisperCollector_BuildDocument_OccurredAt_UsesFilenameNotMtime(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, _ := newWhisperTestServer(t, "occurred_at regression transcript")

	// Call happened at 05:44:34 UTC; the file was only saved 16+ hours later
	// (simulating the permission-outage delayed upload).
	callTime := time.Date(2026, 8, 26, 5, 44, 34, 0, time.UTC)
	mtime := time.Date(2026, 8, 26, 22, 16, 0, 0, time.UTC)
	filename := "01025777190_20260826054434.m4a"
	writeDummyAudio(t, dir, filename, mtime)

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)
	c.WithIndexedIDs(map[string]struct{}{})

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d docs, want 1", len(docs))
	}

	doc := docs[0]
	if doc.OccurredAt == nil {
		t.Fatal("doc.OccurredAt is nil")
	}
	if !doc.OccurredAt.Equal(callTime) {
		t.Errorf("doc.OccurredAt = %v, want filename-derived call time %v (mtime %v must NOT be used)",
			doc.OccurredAt, callTime, mtime)
	}
	if doc.OccurredAt.Equal(mtime) {
		t.Error("doc.OccurredAt equals mtime — the exact production bug this test guards against")
	}
}

// TestWhisperCollector_BuildDocument_OccurredAt_UnparseableFallsBackToMtime
// covers the legacy-filename fallback path through the full pipeline.
func TestWhisperCollector_BuildDocument_OccurredAt_UnparseableFallsBackToMtime(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, _ := newWhisperTestServer(t, "legacy transcript")

	mtime := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	filename := "voice_memo.m4a" // no embedded timestamp
	writeDummyAudio(t, dir, filename, mtime)

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)
	c.WithIndexedIDs(map[string]struct{}{})

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d docs, want 1", len(docs))
	}

	doc := docs[0]
	if doc.OccurredAt == nil {
		t.Fatal("doc.OccurredAt is nil")
	}
	if !doc.OccurredAt.Equal(mtime) {
		t.Errorf("doc.OccurredAt = %v, want mtime fallback %v (no filename timestamp to parse)", doc.OccurredAt, mtime)
	}
}
