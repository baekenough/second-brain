package collector

// whisper_stream_test.go — Tests for the StreamingCollector implementation of
// WhisperCollector (CollectStream). These verify that:
//
//   - CollectStream emits transcripts in multiple bounded batches rather than a
//     single terminal slice.
//   - Every audio file appears in EXACTLY ONE batch (no duplicates, no omissions)
//     even under the concurrent worker pool — verified with a sync.Map.
//   - Collect (the thin accumulator wrapper) still returns the full result set.
//   - Context cancellation aborts the stream early.
//
// Run with -race to exercise the worker-pool / drain-goroutine synchronisation.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/model"
)

// newCountingWhisperServer returns a server that responds to every
// transcription request with a fixed transcript and counts the requests.
func newCountingWhisperServer(t *testing.T, transcript string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(whisperTranscribeResponse{Text: transcript})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestWhisperCollector_CollectStream_MultipleBatches verifies that 12 files with
// a batch size of whisperStreamBatchSize (5) produce 3 batches (5 + 5 + 2) and
// that each file is emitted in exactly one batch.
func TestWhisperCollector_CollectStream_MultipleBatches(t *testing.T) {
	t.Parallel()

	if whisperStreamBatchSize != 5 {
		t.Fatalf("test assumes whisperStreamBatchSize == 5, got %d", whisperStreamBatchSize)
	}

	const fileCount = 12
	dir := t.TempDir()
	srv, _ := newCountingWhisperServer(t, "전사 결과")

	now := time.Now().UTC()
	for i := 0; i < fileCount; i++ {
		writeDummyAudio(t, dir, fmt.Sprintf("rec-%02d.m4a", i), now)
	}

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)
	c.workerCount = 4 // exercise the concurrent path
	c.WithIndexedIDs(map[string]struct{}{})

	// Collect every emitted batch and verify each source_id appears exactly once.
	var (
		mu          sync.Mutex
		batchSizes  []int
		seen        = map[string]int{} // source_id -> times seen
		totalEmitted int
	)

	err := c.CollectStream(context.Background(), time.Time{}, func(batch []model.Document) error {
		// Defensive copy: the collector reuses the batch backing array between
		// flushes, so the test must not retain the slice across calls.
		mu.Lock()
		batchSizes = append(batchSizes, len(batch))
		for _, d := range batch {
			seen[d.SourceID]++
			totalEmitted++
		}
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatalf("CollectStream: %v", err)
	}

	if totalEmitted != fileCount {
		t.Fatalf("total emitted = %d, want %d", totalEmitted, fileCount)
	}

	// Exactly 3 batches expected: 5 + 5 + 2 (order of the partial batch may vary,
	// but with 12 files and N=5 there must be exactly 3 batches).
	if len(batchSizes) != 3 {
		t.Fatalf("got %d batches %v, want 3 (12 files / N=5)", len(batchSizes), batchSizes)
	}
	// No batch may exceed the configured size.
	for i, sz := range batchSizes {
		if sz == 0 || sz > whisperStreamBatchSize {
			t.Errorf("batch[%d] size = %d, must be in (0, %d]", i, sz, whisperStreamBatchSize)
		}
	}

	// Every file emitted exactly once — no duplicates, no omissions.
	if len(seen) != fileCount {
		t.Fatalf("distinct source_ids seen = %d, want %d", len(seen), fileCount)
	}
	for i := 0; i < fileCount; i++ {
		id := fmt.Sprintf("transcript:rec-%02d.m4a", i)
		if seen[id] != 1 {
			t.Errorf("source_id %q seen %d times, want exactly 1", id, seen[id])
		}
	}
}

// TestWhisperCollector_CollectStream_ExactlyOnce_HighConcurrency stresses the
// exactly-once guarantee under high concurrency using an atomic counter and a
// sync.Map of observed source_ids. With concurrency > batch size and many files,
// any duplicate send or dropped document surfaces as a mismatch.
func TestWhisperCollector_CollectStream_ExactlyOnce_HighConcurrency(t *testing.T) {
	t.Parallel()

	const fileCount = 37 // not a multiple of N (5) → final partial batch
	dir := t.TempDir()
	srv, hits := newCountingWhisperServer(t, "동시성 전사")

	now := time.Now().UTC()
	for i := 0; i < fileCount; i++ {
		writeDummyAudio(t, dir, fmt.Sprintf("c-%03d.wav", i), now)
	}

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
	}
	c := makeWhisperCollector(cfg, srv)
	c.workerCount = 8
	c.WithIndexedIDs(map[string]struct{}{})

	var (
		emittedCount atomic.Int64
		seen         sync.Map // source_id -> struct{}
		dupes        atomic.Int64
	)

	err := c.CollectStream(context.Background(), time.Time{}, func(batch []model.Document) error {
		for _, d := range batch {
			emittedCount.Add(1)
			if _, loaded := seen.LoadOrStore(d.SourceID, struct{}{}); loaded {
				dupes.Add(1)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("CollectStream: %v", err)
	}

	if got := emittedCount.Load(); got != fileCount {
		t.Fatalf("emitted = %d, want %d", got, fileCount)
	}
	if d := dupes.Load(); d != 0 {
		t.Fatalf("observed %d duplicate source_ids, want 0", d)
	}
	// Each file is transcribed exactly once (server hit count matches).
	if got := hits.Load(); got != fileCount {
		t.Errorf("server transcription requests = %d, want %d", got, fileCount)
	}

	// Verify the sync.Map holds exactly fileCount distinct ids.
	distinct := 0
	seen.Range(func(_, _ any) bool { distinct++; return true })
	if distinct != fileCount {
		t.Errorf("distinct source_ids = %d, want %d", distinct, fileCount)
	}
}

// TestWhisperCollector_Collect_WrapsCollectStream verifies the backward-compatible
// Collect wrapper still returns the full document set (it accumulates every batch
// CollectStream emits).
func TestWhisperCollector_Collect_WrapsCollectStream(t *testing.T) {
	t.Parallel()

	const fileCount = 7
	dir := t.TempDir()
	srv, _ := newCountingWhisperServer(t, "래퍼 전사")

	now := time.Now().UTC()
	for i := 0; i < fileCount; i++ {
		writeDummyAudio(t, dir, fmt.Sprintf("w-%02d.m4a", i), now)
	}

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
	}
	c := makeWhisperCollector(cfg, srv)
	c.workerCount = 3
	c.WithIndexedIDs(map[string]struct{}{})

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != fileCount {
		t.Fatalf("Collect returned %d docs, want %d", len(docs), fileCount)
	}

	// All ids distinct.
	ids := map[string]struct{}{}
	for _, d := range docs {
		ids[d.SourceID] = struct{}{}
	}
	if len(ids) != fileCount {
		t.Errorf("distinct source_ids = %d, want %d", len(ids), fileCount)
	}
}

// TestWhisperCollector_CollectStream_ContextCancellation verifies the stream
// aborts early when ctx is cancelled and returns the context error.
func TestWhisperCollector_CollectStream_ContextCancellation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// stop is closed during cleanup so any handler still blocked unblocks before
	// srv.Close, avoiding httptest's 5s "blocked in Close" wait. The handler
	// otherwise blocks (simulating a slow transcription) until EITHER the request
	// context is cancelled (by the collector) or the test tears down.
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	t.Cleanup(func() {
		close(stop)
		srv.Close()
	})

	now := time.Now().UTC()
	for i := 0; i < 6; i++ {
		writeDummyAudio(t, dir, fmt.Sprintf("slow-%02d.m4a", i), now)
	}

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
	}
	c := makeWhisperCollector(cfg, srv)
	c.workerCount = 2
	c.WithIndexedIDs(map[string]struct{}{})

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel shortly after the stream starts so workers are mid-transcription.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := c.CollectStream(ctx, time.Time{}, func(batch []model.Document) error {
		return nil
	})
	if err == nil {
		t.Fatalf("CollectStream returned nil error, want context cancellation error")
	}
}

// TestWhisperCollector_CollectStream_OnBatchErrorPropagates verifies that an
// error returned by onBatch aborts the stream and is propagated to the caller.
func TestWhisperCollector_CollectStream_OnBatchErrorPropagates(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, _ := newCountingWhisperServer(t, "전사")

	now := time.Now().UTC()
	for i := 0; i < 12; i++ {
		writeDummyAudio(t, dir, fmt.Sprintf("e-%02d.m4a", i), now)
	}

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
	}
	c := makeWhisperCollector(cfg, srv)
	c.workerCount = 4
	c.WithIndexedIDs(map[string]struct{}{})

	wantErr := fmt.Errorf("emit boom")
	err := c.CollectStream(context.Background(), time.Time{}, func(batch []model.Document) error {
		return wantErr // fail on the first batch
	})
	if err == nil {
		t.Fatalf("CollectStream returned nil error, want %v", wantErr)
	}
	// The caller's original error must be returned, not an internal wrapper.
	if err.Error() != wantErr.Error() {
		t.Errorf("CollectStream error = %v, want %v", err, wantErr)
	}
}
