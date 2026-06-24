package collector

// whisper_dedup_test.go — Regression + concurrency tests for the infinite
// re-transcription loop fix and the worker-pool load balancing.
//
// Core regression (TestWhisperCollector_SkipsAlreadyKnown_EvenWhenMtimeNew):
// the OLD emit logic was "mtimeNew || notIndexed", which re-transcribed any file
// absent from the active index every cycle. The NEW logic treats a non-nil index
// set as AUTHORITATIVE: a known source_id is NEVER transcribed again, regardless
// of mtime (audio is immutable). These tests fail on the old logic and pass on
// the new one.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/config"
)

// newRejectingWhisperServer returns a server that FAILS the test if it ever
// receives a transcription request, plus the captured request recorder.
func newRejectingWhisperServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("whisper server received a transcription request for an already-known file — must be skipped (path=%s)", r.URL.Path)
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestWhisperCollector_SkipsAlreadyKnown_EvenWhenMtimeNew is the core regression
// guard: a file whose mtime is AFTER `since` but whose source_id IS in the
// indexed set must NOT be transcribed (0 docs, no server hit).
//
// On the OLD "mtimeNew || notIndexed" logic mtimeNew==true → the file would be
// transcribed → this test fails. On the NEW authoritative logic the known
// source_id is skipped → 0 docs.
func TestWhisperCollector_SkipsAlreadyKnown_EvenWhenMtimeNew(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv := newRejectingWhisperServer(t)

	now := time.Now().UTC()
	since := now.Add(-2 * time.Hour)

	// File mtime is AFTER since (so mtimeNew would be true on the old logic).
	newMtime := now.Add(-1 * time.Hour).Truncate(time.Second)
	writeDummyAudio(t, dir, "known.m4a", newMtime)

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)
	// source_id IS already in the authoritative set (active index ∪ ledger).
	c.WithIndexedIDs(map[string]struct{}{
		"transcript:known.m4a": {},
	})

	docs, err := c.Collect(context.Background(), since)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("got %d docs, want 0 (already-known file must never be re-transcribed even with new mtime)", len(docs))
	}
}

// TestWhisperCollector_TranscribesUnknown verifies a file NOT in the indexed set
// is transcribed even when its mtime predates `since` (IndexAware recovery).
func TestWhisperCollector_TranscribesUnknown(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, _ := newWhisperTestServer(t, "새 파일 전사")

	now := time.Now().UTC()
	since := now.Add(-1 * time.Hour)

	// mtime BEFORE since — only the index-absence should drive transcription.
	oldMtime := now.Add(-5 * time.Hour).Truncate(time.Second)
	writeDummyAudio(t, dir, "unknown.m4a", oldMtime)

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)
	// Non-nil set that does NOT contain this source_id → must transcribe.
	c.WithIndexedIDs(map[string]struct{}{})

	docs, err := c.Collect(context.Background(), since)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d docs, want 1 (unknown file must be transcribed)", len(docs))
	}
	if docs[0].SourceID != "transcript:unknown.m4a" {
		t.Errorf("SourceID = %q, want transcript:unknown.m4a", docs[0].SourceID)
	}
}

// TestWhisperCollector_NilIndex_FallsBackToMtime verifies that when the index
// set is nil (scheduler store query failed) the collector falls back to the
// mtime watermark: new-mtime files are transcribed, old-mtime files are skipped.
func TestWhisperCollector_NilIndex_FallsBackToMtime(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, _ := newWhisperTestServer(t, "mtime fallback 전사")

	now := time.Now().UTC()
	since := now.Add(-2 * time.Hour)

	// New file: mtime after since → transcribed.
	writeDummyAudio(t, dir, "new.m4a", now.Add(-1*time.Hour).Truncate(time.Second))
	// Old file: mtime before since → skipped.
	writeDummyAudio(t, dir, "old.mp3", now.Add(-3*time.Hour).Truncate(time.Second))

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)
	// nil index → mtime-only fallback (this is the default, but set explicitly).
	c.WithIndexedIDs(nil)

	docs, err := c.Collect(context.Background(), since)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d docs, want 1 (nil index → mtime-only: new transcribed, old skipped)", len(docs))
	}
	if docs[0].SourceID != "transcript:new.m4a" {
		t.Errorf("SourceID = %q, want transcript:new.m4a", docs[0].SourceID)
	}
}

// TestWhisperCollector_Concurrency verifies that with WhisperConcurrency=4 and
// several unknown files, every file is transcribed exactly once — no duplicate
// submission, no loss — guarding the worker-pool against races. Each filename is
// recorded in a sync.Map keyed by filename with an atomic per-file counter; any
// filename hit more than once fails the test.
func TestWhisperCollector_Concurrency(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// hits maps uploaded filename → *int64 atomic counter.
	var hits sync.Map
	var totalHits int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&totalHits, 1)
		// Parse the multipart filename to key the counter.
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		var fname string
		if r.MultipartForm != nil {
			if fhs, ok := r.MultipartForm.File["file"]; ok && len(fhs) > 0 {
				fname = fhs[0].Filename
			}
		}
		ctr, _ := hits.LoadOrStore(fname, new(int64))
		atomic.AddInt64(ctr.(*int64), 1)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(whisperTranscribeResponse{Text: "병렬 전사 " + fname})
	}))
	defer srv.Close()

	const fileCount = 12
	now := time.Now().UTC()
	for i := 0; i < fileCount; i++ {
		name := filepath.Base(filepath.Join(dir, namef(i)))
		writeDummyAudio(t, dir, name, now.Add(-time.Duration(i)*time.Minute).Truncate(time.Second))
	}

	cfg := &config.Config{
		WhisperAudioDir:    dir,
		WhisperAPIURL:      srv.URL,
		WhisperModel:       "whisper-1",
		WhisperLanguage:    "ko",
		WhisperConcurrency: 4,
	}
	c := makeWhisperCollector(cfg, srv)
	c.WithIndexedIDs(map[string]struct{}{}) // all unknown → all transcribed

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != fileCount {
		t.Fatalf("got %d docs, want %d (each unknown file transcribed exactly once)", len(docs), fileCount)
	}
	if got := atomic.LoadInt64(&totalHits); got != fileCount {
		t.Errorf("server received %d total requests, want %d", got, fileCount)
	}

	// Every file must have been hit exactly once.
	seen := 0
	hits.Range(func(k, v any) bool {
		seen++
		if n := atomic.LoadInt64(v.(*int64)); n != 1 {
			t.Errorf("file %q transcribed %d times, want exactly 1", k, n)
		}
		return true
	})
	if seen != fileCount {
		t.Errorf("distinct files hit = %d, want %d", seen, fileCount)
	}

	// Verify worker count is honoured (clamped to >= 1).
	if c.concurrency() != 4 {
		t.Errorf("concurrency() = %d, want 4", c.concurrency())
	}
}

// namef returns a deterministic distinct audio filename for index i.
func namef(i int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	return "call-" + string(letters[i%len(letters)]) + string(letters[(i/len(letters))%len(letters)]) + ".m4a"
}

// TestWhisperCollector_Concurrency_DefaultIsSequential verifies the default
// (WhisperConcurrency unset → 0 → clamped to 1) reproduces sequential behaviour
// and still transcribes every file exactly once.
func TestWhisperCollector_Concurrency_DefaultIsSequential(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, _ := newWhisperTestServer(t, "순차 전사")

	now := time.Now().UTC()
	writeDummyAudio(t, dir, "x.m4a", now.Add(-1*time.Hour).Truncate(time.Second))
	writeDummyAudio(t, dir, "y.mp3", now.Add(-2*time.Hour).Truncate(time.Second))

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
		// WhisperConcurrency intentionally unset (0) → concurrency() clamps to 1.
	}
	c := makeWhisperCollector(cfg, srv)
	c.WithIndexedIDs(map[string]struct{}{})

	if c.concurrency() != 1 {
		t.Fatalf("concurrency() = %d, want 1 (default sequential)", c.concurrency())
	}

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 2 {
		t.Errorf("got %d docs, want 2", len(docs))
	}
}
