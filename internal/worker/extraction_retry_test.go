package worker

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// ---------------------------------------------------------------------------
// Mock implementations
// ---------------------------------------------------------------------------

type mockExtractor struct {
	mu     sync.Mutex
	result string
	err    error
	calls  []string // paths passed to ExtractFromPath
}

func (m *mockExtractor) ExtractFromPath(_ context.Context, path string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, path)
	return m.result, m.err
}

func (m *mockExtractor) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

type mockFailureStore struct {
	mu       sync.Mutex
	due      []store.ExtractionFailure
	dueErr   error
	recorded []store.ExtractionFailure
	resolved [][2]string // [sourceType, sourceID] pairs
}

func (m *mockFailureStore) DueForRetry(_ context.Context, limit int) ([]store.ExtractionFailure, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dueErr != nil {
		return nil, m.dueErr
	}
	out := m.due
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *mockFailureStore) Record(_ context.Context, f store.ExtractionFailure) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recorded = append(m.recorded, f)
	return nil
}

func (m *mockFailureStore) Resolve(_ context.Context, sourceType, sourceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resolved = append(m.resolved, [2]string{sourceType, sourceID})
	return nil
}

type mockDocStore struct {
	mu      sync.Mutex
	upserts []*model.Document
	err     error
}

func (m *mockDocStore) Upsert(_ context.Context, doc *model.Document) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.upserts = append(m.upserts, doc)
	return nil
}

// ---------------------------------------------------------------------------
// looksLikeLocalPath tests
// ---------------------------------------------------------------------------

func TestLooksLikeLocalPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  bool
	}{
		{"/home/user/file.pdf", true},
		{"/tmp/attachment.docx", true},
		{"/", true},
		{"relative/path.pdf", false},
		{"file.pdf", false},
		{"//network/share/file.pdf", false},
		{"https://cdn.example.com/file.pdf", false},
		{"discord://attachment/123", false},
		{"", false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			got := looksLikeLocalPath(tc.input)
			if got != tc.want {
				t.Errorf("looksLikeLocalPath(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// processBatch: empty queue
// ---------------------------------------------------------------------------

func TestProcessBatch_EmptyQueue(t *testing.T) {
	t.Parallel()

	fStore := &mockFailureStore{due: nil}
	ext := &mockExtractor{}
	docSt := &mockDocStore{}

	w := New(Config{
		FailureStore: fStore,
		DocStore:     docSt,
		Extractor:    ext,
		Interval:     time.Minute,
		BatchSize:    20,
	})

	w.processBatch(context.Background())

	if ext.callCount() != 0 {
		t.Errorf("expected 0 extractor calls, got %d", ext.callCount())
	}
	if len(fStore.recorded) != 0 {
		t.Errorf("expected 0 recorded failures, got %d", len(fStore.recorded))
	}
	if len(fStore.resolved) != 0 {
		t.Errorf("expected 0 resolved, got %d", len(fStore.resolved))
	}
}

// ---------------------------------------------------------------------------
// processBatch: DueForRetry returns an error
// ---------------------------------------------------------------------------

func TestProcessBatch_DueForRetryError(t *testing.T) {
	t.Parallel()

	fStore := &mockFailureStore{dueErr: errors.New("db error")}
	ext := &mockExtractor{}
	docSt := &mockDocStore{}

	w := New(Config{
		FailureStore: fStore,
		DocStore:     docSt,
		Extractor:    ext,
	})

	// must not panic
	w.processBatch(context.Background())

	if ext.callCount() != 0 {
		t.Errorf("expected 0 extractor calls on db error, got %d", ext.callCount())
	}
}

// ---------------------------------------------------------------------------
// processBatch: local path — extraction succeeds
// ---------------------------------------------------------------------------

func TestProcessBatch_LocalPathSuccess(t *testing.T) {
	t.Parallel()

	failure := store.ExtractionFailure{
		ID:          1,
		SourceType:  "filesystem",
		SourceID:    "file-abc",
		FilePath:    "/tmp/test.pdf",
		ErrorMessage: "previous error",
		Attempts:    1,
	}

	fStore := &mockFailureStore{due: []store.ExtractionFailure{failure}}
	ext := &mockExtractor{result: "extracted content"}
	docSt := &mockDocStore{}

	w := New(Config{
		FailureStore: fStore,
		DocStore:     docSt,
		Extractor:    ext,
	})

	w.processBatch(context.Background())

	// Extractor must be called once with the correct path.
	if ext.callCount() != 1 {
		t.Fatalf("expected 1 extractor call, got %d", ext.callCount())
	}
	if ext.calls[0] != failure.FilePath {
		t.Errorf("extractor called with %q, want %q", ext.calls[0], failure.FilePath)
	}

	// Document must be upserted.
	docSt.mu.Lock()
	upserts := docSt.upserts
	docSt.mu.Unlock()
	if len(upserts) != 1 {
		t.Fatalf("expected 1 upsert, got %d", len(upserts))
	}
	if upserts[0].Content != "extracted content" {
		t.Errorf("upserted content = %q, want %q", upserts[0].Content, "extracted content")
	}
	if string(upserts[0].SourceType) != failure.SourceType {
		t.Errorf("upserted SourceType = %q, want %q", upserts[0].SourceType, failure.SourceType)
	}

	// Failure must be resolved.
	if len(fStore.resolved) != 1 {
		t.Fatalf("expected 1 resolved entry, got %d", len(fStore.resolved))
	}
	if fStore.resolved[0][0] != failure.SourceType || fStore.resolved[0][1] != failure.SourceID {
		t.Errorf("resolved [%s, %s], want [%s, %s]",
			fStore.resolved[0][0], fStore.resolved[0][1],
			failure.SourceType, failure.SourceID)
	}

	// No new failure records should be written on success.
	if len(fStore.recorded) != 0 {
		t.Errorf("expected 0 recorded failures, got %d", len(fStore.recorded))
	}
}

// ---------------------------------------------------------------------------
// processBatch: local path — extraction fails
// ---------------------------------------------------------------------------

func TestProcessBatch_LocalPathFailure(t *testing.T) {
	t.Parallel()

	failure := store.ExtractionFailure{
		ID:          2,
		SourceType:  "gdrive",
		SourceID:    "gdrive-xyz",
		FilePath:    "/tmp/corrupt.docx",
		ErrorMessage: "previous error",
		Attempts:    3,
	}

	extractErr := errors.New("extraction failed: corrupted file")
	fStore := &mockFailureStore{due: []store.ExtractionFailure{failure}}
	ext := &mockExtractor{err: extractErr}
	docSt := &mockDocStore{}

	w := New(Config{
		FailureStore: fStore,
		DocStore:     docSt,
		Extractor:    ext,
	})

	w.processBatch(context.Background())

	// Extractor called once.
	if ext.callCount() != 1 {
		t.Fatalf("expected 1 extractor call, got %d", ext.callCount())
	}

	// A new failure record must be written to increment the attempt counter.
	if len(fStore.recorded) != 1 {
		t.Fatalf("expected 1 recorded failure, got %d", len(fStore.recorded))
	}
	rec := fStore.recorded[0]
	if rec.SourceType != failure.SourceType {
		t.Errorf("recorded SourceType = %q, want %q", rec.SourceType, failure.SourceType)
	}
	if rec.SourceID != failure.SourceID {
		t.Errorf("recorded SourceID = %q, want %q", rec.SourceID, failure.SourceID)
	}
	if rec.ErrorMessage != extractErr.Error() {
		t.Errorf("recorded ErrorMessage = %q, want %q", rec.ErrorMessage, extractErr.Error())
	}

	// Nothing should be resolved.
	if len(fStore.resolved) != 0 {
		t.Errorf("expected 0 resolved entries, got %d", len(fStore.resolved))
	}

	// Nothing should be upserted.
	docSt.mu.Lock()
	n := len(docSt.upserts)
	docSt.mu.Unlock()
	if n != 0 {
		t.Errorf("expected 0 upserts, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// processBatch: remote path is skipped
// ---------------------------------------------------------------------------

func TestProcessBatch_SkipsRemotePath(t *testing.T) {
	t.Parallel()

	remoteFailures := []store.ExtractionFailure{
		{SourceType: "discord", SourceID: "msg-1", FilePath: "https://cdn.discordapp.com/att.pdf"},
		{SourceType: "slack", SourceID: "msg-2", FilePath: "slack://files/abc.docx"},
		{SourceType: "gdrive", SourceID: "gdrive-3", FilePath: "//network/share/file.xlsx"},
	}

	fStore := &mockFailureStore{due: remoteFailures}
	ext := &mockExtractor{result: "some content"}
	docSt := &mockDocStore{}

	w := New(Config{
		FailureStore: fStore,
		DocStore:     docSt,
		Extractor:    ext,
	})

	w.processBatch(context.Background())

	// Extractor must never be called for remote paths.
	if ext.callCount() != 0 {
		t.Errorf("expected 0 extractor calls for remote paths, got %d", ext.callCount())
	}

	// Nothing should be recorded or resolved.
	if len(fStore.recorded) != 0 {
		t.Errorf("expected 0 recorded failures, got %d", len(fStore.recorded))
	}
	if len(fStore.resolved) != 0 {
		t.Errorf("expected 0 resolved, got %d", len(fStore.resolved))
	}
}

// ---------------------------------------------------------------------------
// Run: goroutine stops on ctx cancel
// ---------------------------------------------------------------------------

func TestRun_StopsOnCtxCancel(t *testing.T) {
	t.Parallel()

	fStore := &mockFailureStore{due: nil}
	ext := &mockExtractor{}
	docSt := &mockDocStore{}

	// Use a long interval so the ticker never fires during the test.
	w := New(Config{
		FailureStore: fStore,
		DocStore:     docSt,
		Extractor:    ext,
		Interval:     10 * time.Minute,
		BatchSize:    5,
	})

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()

	// Cancel immediately and wait for the goroutine to exit.
	cancel()

	select {
	case <-done:
		// success: goroutine exited
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop within 2s after context cancel")
	}
}

// ---------------------------------------------------------------------------
// New: panics on missing required dependencies
// ---------------------------------------------------------------------------

func TestNew_PanicsOnNilDependencies(t *testing.T) {
	t.Parallel()

	fStore := &mockFailureStore{}
	docSt := &mockDocStore{}
	ext := &mockExtractor{}

	cases := []struct {
		name string
		cfg  Config
	}{
		{"nil FailureStore", Config{DocStore: docSt, Extractor: ext}},
		{"nil DocStore", Config{FailureStore: fStore, Extractor: ext}},
		{"nil Extractor", Config{FailureStore: fStore, DocStore: docSt}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("expected panic for %s, got none", tc.name)
				}
			}()
			New(tc.cfg)
		})
	}
}

// ---------------------------------------------------------------------------
// mockRefetcher
// ---------------------------------------------------------------------------

// mockRefetcher is a controllable Refetcher for testing.
type mockRefetcher struct {
	mu     sync.Mutex
	result *RefetchResult
	err    error
	calls  []store.ExtractionFailure
}

func (m *mockRefetcher) Refetch(_ context.Context, f store.ExtractionFailure) (*RefetchResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, f)
	return m.result, m.err
}

func (m *mockRefetcher) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// makeTempResult writes content to a real temp file and returns a RefetchResult
// with a cleanup function. Callers should defer result.Cleanup() or call it
// explicitly; the test framework will catch leaked temp files.
func makeTempResult(t *testing.T, content string) *RefetchResult {
	t.Helper()
	f, err := os.CreateTemp("", "test-refetch-*")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(f.Name())
		t.Fatalf("write temp file: %v", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		t.Fatalf("close temp file: %v", err)
	}
	path := f.Name()
	return &RefetchResult{
		LocalPath: path,
		Cleanup:   func() { os.Remove(path) },
	}
}

// ---------------------------------------------------------------------------
// processOne: remote path — nil Refetcher skips (backward-compat)
// ---------------------------------------------------------------------------

func TestProcessOne_RemotePath_NilRefetcher_Skips(t *testing.T) {
	t.Parallel()

	remoteFailures := []store.ExtractionFailure{
		{SourceType: "discord", SourceID: "att-1", FilePath: "https://cdn.discordapp.com/att.pdf"},
		{SourceType: "slack", SourceID: "file-2", FilePath: "https://files.slack.com/foo.docx"},
	}

	fStore := &mockFailureStore{due: remoteFailures}
	ext := &mockExtractor{result: "content"}
	docSt := &mockDocStore{}

	// No Refetcher — must behave identically to the original skip logic.
	w := New(Config{
		FailureStore: fStore,
		DocStore:     docSt,
		Extractor:    ext,
		// Refetcher intentionally absent
	})

	w.processBatch(context.Background())

	if ext.callCount() != 0 {
		t.Errorf("expected 0 extractor calls, got %d", ext.callCount())
	}
	if len(fStore.recorded) != 0 {
		t.Errorf("expected 0 recorded failures, got %d", len(fStore.recorded))
	}
	if len(fStore.resolved) != 0 {
		t.Errorf("expected 0 resolved, got %d", len(fStore.resolved))
	}
}

// ---------------------------------------------------------------------------
// processOne: remote path — Refetcher returns ErrRefetchNotSupported (skip)
// ---------------------------------------------------------------------------

func TestProcessOne_RemotePath_RefetchNotSupported_Skips(t *testing.T) {
	t.Parallel()

	failure := store.ExtractionFailure{
		SourceType: "telegram",
		SourceID:   "file-99",
		FilePath:   "https://api.telegram.org/file/bot/document.pdf",
	}

	fStore := &mockFailureStore{due: []store.ExtractionFailure{failure}}
	ext := &mockExtractor{result: "content"}
	docSt := &mockDocStore{}
	refetcher := &mockRefetcher{err: ErrRefetchNotSupported}

	w := New(Config{
		FailureStore: fStore,
		DocStore:     docSt,
		Extractor:    ext,
		Refetcher:    refetcher,
	})

	w.processBatch(context.Background())

	if refetcher.callCount() != 1 {
		t.Errorf("expected 1 refetcher call, got %d", refetcher.callCount())
	}
	if ext.callCount() != 0 {
		t.Errorf("expected 0 extractor calls when refetch not supported, got %d", ext.callCount())
	}
	if len(fStore.recorded) != 0 {
		t.Errorf("expected 0 recorded failures (skip should not increment attempts), got %d", len(fStore.recorded))
	}
	if len(fStore.resolved) != 0 {
		t.Errorf("expected 0 resolved, got %d", len(fStore.resolved))
	}
}

// ---------------------------------------------------------------------------
// processOne: remote path — Refetcher succeeds → extraction succeeds
// ---------------------------------------------------------------------------

func TestProcessOne_RemotePath_RefetchSuccess_ExtractionSuccess(t *testing.T) {
	t.Parallel()

	failure := store.ExtractionFailure{
		ID:         10,
		SourceType: "discord",
		SourceID:   "att-10",
		FilePath:   "https://cdn.discordapp.com/attachments/ch/msg/report.pdf",
		Attempts:   2,
	}

	fStore := &mockFailureStore{due: []store.ExtractionFailure{failure}}
	ext := &mockExtractor{result: "remote extracted content"}
	docSt := &mockDocStore{}

	result := makeTempResult(t, "remote extracted content")
	refetcher := &mockRefetcher{result: result}

	w := New(Config{
		FailureStore: fStore,
		DocStore:     docSt,
		Extractor:    ext,
		Refetcher:    refetcher,
	})

	w.processBatch(context.Background())

	// Refetcher called once.
	if refetcher.callCount() != 1 {
		t.Fatalf("expected 1 refetcher call, got %d", refetcher.callCount())
	}
	// Extractor called on the temp file path.
	if ext.callCount() != 1 {
		t.Fatalf("expected 1 extractor call, got %d", ext.callCount())
	}
	if ext.calls[0] != result.LocalPath {
		t.Errorf("extractor called with %q, want %q", ext.calls[0], result.LocalPath)
	}

	// Document upserted with correct content and source metadata.
	docSt.mu.Lock()
	upserts := docSt.upserts
	docSt.mu.Unlock()
	if len(upserts) != 1 {
		t.Fatalf("expected 1 upsert, got %d", len(upserts))
	}
	if upserts[0].Content != "remote extracted content" {
		t.Errorf("upserted content = %q, want %q", upserts[0].Content, "remote extracted content")
	}
	if string(upserts[0].SourceType) != failure.SourceType {
		t.Errorf("upserted SourceType = %q, want %q", upserts[0].SourceType, failure.SourceType)
	}
	if upserts[0].SourceID != failure.SourceID {
		t.Errorf("upserted SourceID = %q, want %q", upserts[0].SourceID, failure.SourceID)
	}

	// Failure resolved.
	if len(fStore.resolved) != 1 {
		t.Fatalf("expected 1 resolved entry, got %d", len(fStore.resolved))
	}
	if fStore.resolved[0][0] != failure.SourceType || fStore.resolved[0][1] != failure.SourceID {
		t.Errorf("resolved [%s, %s], want [%s, %s]",
			fStore.resolved[0][0], fStore.resolved[0][1],
			failure.SourceType, failure.SourceID)
	}

	// No new failure record written.
	if len(fStore.recorded) != 0 {
		t.Errorf("expected 0 recorded failures on success, got %d", len(fStore.recorded))
	}
}

// ---------------------------------------------------------------------------
// processOne: remote path — Refetcher succeeds → extraction fails (increment attempts)
// ---------------------------------------------------------------------------

func TestProcessOne_RemotePath_RefetchSuccess_ExtractionFails(t *testing.T) {
	t.Parallel()

	failure := store.ExtractionFailure{
		SourceType: "discord",
		SourceID:   "att-fail",
		FilePath:   "https://cdn.discordapp.com/attachments/ch/msg/corrupt.pdf",
		Attempts:   1,
	}

	extractErr := errors.New("corrupt PDF: unexpected EOF")
	fStore := &mockFailureStore{due: []store.ExtractionFailure{failure}}
	ext := &mockExtractor{err: extractErr}
	docSt := &mockDocStore{}

	result := makeTempResult(t, "")
	refetcher := &mockRefetcher{result: result}

	w := New(Config{
		FailureStore: fStore,
		DocStore:     docSt,
		Extractor:    ext,
		Refetcher:    refetcher,
	})

	w.processBatch(context.Background())

	// Extractor called.
	if ext.callCount() != 1 {
		t.Fatalf("expected 1 extractor call, got %d", ext.callCount())
	}

	// Attempt counter incremented.
	if len(fStore.recorded) != 1 {
		t.Fatalf("expected 1 recorded failure, got %d", len(fStore.recorded))
	}
	rec := fStore.recorded[0]
	if rec.SourceType != failure.SourceType {
		t.Errorf("recorded SourceType = %q, want %q", rec.SourceType, failure.SourceType)
	}
	if rec.SourceID != failure.SourceID {
		t.Errorf("recorded SourceID = %q, want %q", rec.SourceID, failure.SourceID)
	}
	if rec.ErrorMessage != extractErr.Error() {
		t.Errorf("recorded ErrorMessage = %q, want %q", rec.ErrorMessage, extractErr.Error())
	}

	// Nothing resolved.
	if len(fStore.resolved) != 0 {
		t.Errorf("expected 0 resolved entries, got %d", len(fStore.resolved))
	}

	// Nothing upserted.
	docSt.mu.Lock()
	n := len(docSt.upserts)
	docSt.mu.Unlock()
	if n != 0 {
		t.Errorf("expected 0 upserts, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// processOne: remote path — Refetcher itself errors (network/expired URL)
// ---------------------------------------------------------------------------

func TestProcessOne_RemotePath_RefetchError_IncrementsAttempts(t *testing.T) {
	t.Parallel()

	failure := store.ExtractionFailure{
		SourceType: "discord",
		SourceID:   "att-expired",
		FilePath:   "https://cdn.discordapp.com/expired.pdf",
		Attempts:   3,
	}

	refetchErr := errors.New("unexpected HTTP status 403")
	fStore := &mockFailureStore{due: []store.ExtractionFailure{failure}}
	ext := &mockExtractor{}
	docSt := &mockDocStore{}
	refetcher := &mockRefetcher{err: refetchErr}

	w := New(Config{
		FailureStore: fStore,
		DocStore:     docSt,
		Extractor:    ext,
		Refetcher:    refetcher,
	})

	w.processBatch(context.Background())

	// Extractor must not be called (download failed).
	if ext.callCount() != 0 {
		t.Errorf("expected 0 extractor calls after refetch error, got %d", ext.callCount())
	}

	// Attempt counter incremented.
	if len(fStore.recorded) != 1 {
		t.Fatalf("expected 1 recorded failure, got %d", len(fStore.recorded))
	}
	rec := fStore.recorded[0]
	if rec.SourceID != failure.SourceID {
		t.Errorf("recorded SourceID = %q, want %q", rec.SourceID, failure.SourceID)
	}

	// Nothing resolved.
	if len(fStore.resolved) != 0 {
		t.Errorf("expected 0 resolved entries, got %d", len(fStore.resolved))
	}
}

// ---------------------------------------------------------------------------
// processBatch: batch size is respected
// ---------------------------------------------------------------------------

func TestProcessBatch_BatchSizeLimit(t *testing.T) {
	t.Parallel()

	// Populate 10 failures, but set BatchSize to 3.
	var failures []store.ExtractionFailure
	for i := 0; i < 10; i++ {
		failures = append(failures, store.ExtractionFailure{
			SourceType: "filesystem",
			SourceID:   "id",
			FilePath:   "/tmp/file.pdf",
		})
	}

	fStore := &mockFailureStore{due: failures}
	ext := &mockExtractor{result: "ok"}
	docSt := &mockDocStore{}

	w := New(Config{
		FailureStore: fStore,
		DocStore:     docSt,
		Extractor:    ext,
		BatchSize:    3,
	})

	w.processBatch(context.Background())

	// mockFailureStore.DueForRetry honours the limit parameter.
	if ext.callCount() != 3 {
		t.Errorf("expected 3 extractor calls (batch limit), got %d", ext.callCount())
	}
}
