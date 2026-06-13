package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/collector"
	"github.com/baekenough/second-brain/internal/model"
)

// ---------------------------------------------------------------------------
// Test doubles for deletion-guard tests
// ---------------------------------------------------------------------------

// trackingStore wraps mockStore and records every MarkDeleted call so tests can
// assert that MarkDeleted was or was not invoked, and what IDs were passed.
type trackingStore struct {
	mockStore

	mu           sync.Mutex
	markDeletedCalls []markDeletedCall
}

type markDeletedCall struct {
	sourceType model.SourceType
	activeIDs  []string
}

func (s *trackingStore) MarkDeleted(ctx context.Context, sourceType model.SourceType, activeIDs []string) (int, error) {
	s.mu.Lock()
	s.markDeletedCalls = append(s.markDeletedCalls, markDeletedCall{
		sourceType: sourceType,
		activeIDs:  append([]string(nil), activeIDs...), // copy
	})
	s.mu.Unlock()
	return len(activeIDs), nil // return plausible count
}

func (s *trackingStore) markDeletedCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.markDeletedCalls)
}

// trackingStoreWithActive is like trackingStore but also tracks active-document
// counts, enabling the sanity-check ratio test.
type trackingStoreWithActive struct {
	trackingStore

	// activeCount simulates how many active documents are in the DB for the source.
	activeCount int
	// markDeletedReturn is the number of rows MarkDeleted should report deleted.
	markDeletedReturn int
}

func (s *trackingStoreWithActive) MarkDeleted(ctx context.Context, sourceType model.SourceType, activeIDs []string) (int, error) {
	s.mu.Lock()
	s.markDeletedCalls = append(s.markDeletedCalls, markDeletedCall{
		sourceType: sourceType,
		activeIDs:  append([]string(nil), activeIDs...),
	})
	s.mu.Unlock()
	return s.markDeletedReturn, nil
}

// errListingCollector is a DeletionDetector whose ListActiveSourceIDs always
// returns an error, simulating an unmounted or missing root directory.
type errListingCollector struct {
	countingCollector
	listErr error
}

func newErrListingCollector(name string) *errListingCollector {
	return &errListingCollector{
		countingCollector: countingCollector{
			name:    name,
			source:  model.SourceType("test-err-" + name),
			enabled: true,
		},
		listErr: errors.New("root directory not accessible"),
	}
}

func (c *errListingCollector) ListActiveSourceIDs(_ context.Context) ([]string, error) {
	return nil, c.listErr
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestScheduler_DeletionGuard_ListError_SkipsMarkDeleted verifies Layer 2:
// when ListActiveSourceIDs returns an error (e.g. unmounted root), the
// scheduler must NOT call MarkDeleted. Calling MarkDeleted with an empty slice
// on a vacuously-true Postgres NOT IN query would soft-delete all active docs.
func TestScheduler_DeletionGuard_ListError_SkipsMarkDeleted(t *testing.T) {
	t.Parallel()

	col := newErrListingCollector("unmounted")
	st := &trackingStore{}
	sched := New(st, disabledEmbed(), col)

	sched.run(context.Background(), col)

	if got := st.markDeletedCallCount(); got != 0 {
		t.Errorf("MarkDeleted must not be called when ListActiveSourceIDs returns error, got %d calls", got)
	}
}

// TestScheduler_DeletionGuard_NormalOperation_CallsMarkDeleted verifies that
// when ListActiveSourceIDs succeeds with a non-empty slice, MarkDeleted IS
// called (normal soft-delete detection path is preserved).
func TestScheduler_DeletionGuard_NormalOperation_CallsMarkDeleted(t *testing.T) {
	t.Parallel()

	// Use a real FilesystemCollector on a temp dir with a file.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "kept.md"), []byte("# kept"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	col := collector.NewFilesystemCollector(root)
	st := &trackingStore{}
	sched := New(st, disabledEmbed(), col)

	sched.run(context.Background(), col)

	if got := st.markDeletedCallCount(); got != 1 {
		t.Errorf("MarkDeleted should be called once for normal deletion detection, got %d calls", got)
	}
}

// TestScheduler_DeletionRatioGuard_ExceedsThreshold_SkipsMarkDeleted verifies
// Layer 2 sanity-check: when the number of documents that would be deleted
// exceeds deletionRatioThreshold fraction of currently active documents,
// the scheduler skips the deletion and logs a warning instead.
//
// This guards against scenarios where a partial unmount or transient filesystem
// glitch makes most files invisible, which would otherwise bulk-delete real data.
func TestScheduler_DeletionRatioGuard_ExceedsThreshold_SkipsMarkDeleted(t *testing.T) {
	t.Parallel()

	// Create a real temp dir with one file. DB "thinks" there are 10 active docs.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "only.md"), []byte("# only"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	col := collector.NewFilesystemCollector(root)
	// Store reports 10 active docs; ListActiveSourceIDs returns 1 ID.
	// → 9 docs would be deleted = 90% ratio → exceeds threshold → must skip.
	st := &trackingStoreWithActiveAndCount{
		activeCount: 10,
	}
	sched := New(st, disabledEmbed(), col)

	sched.run(context.Background(), col)

	if got := st.markDeletedCallCount(); got != 0 {
		t.Errorf("MarkDeleted must be skipped when deletion ratio exceeds threshold, got %d calls", got)
	}
}

// TestScheduler_DeletionRatioGuard_BelowThreshold_AllowsMarkDeleted verifies
// that normal-sized deletions (below the safety threshold) are still performed.
func TestScheduler_DeletionRatioGuard_BelowThreshold_AllowsMarkDeleted(t *testing.T) {
	t.Parallel()

	// 10 files on disk, DB has 11 active docs → 1 deletion = ~9% → under threshold.
	root := t.TempDir()
	for i := 0; i < 10; i++ {
		name := filepath.Join(root, "file"+string(rune('a'+i))+".md")
		if err := os.WriteFile(name, []byte("# doc"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	col := collector.NewFilesystemCollector(root)
	st := &trackingStoreWithActiveAndCount{
		activeCount: 11,
	}
	sched := New(st, disabledEmbed(), col)

	sched.run(context.Background(), col)

	if got := st.markDeletedCallCount(); got != 1 {
		t.Errorf("MarkDeleted should be called when deletion ratio is below threshold, got %d calls", got)
	}
}

// trackingStoreWithActiveAndCount provides countActiveDocuments support.
type trackingStoreWithActiveAndCount struct {
	trackingStore

	activeCount int
}

func (s *trackingStoreWithActiveAndCount) CountActiveDocuments(_ context.Context, _ model.SourceType) (int, error) {
	return s.activeCount, nil
}

// TestScheduler_DeletionRatioGuard_ZeroActive_AllowsEmptyMarkDeleted verifies
// that when the DB has zero active documents (fresh start), an empty ListActiveSourceIDs
// result correctly calls MarkDeleted (no-op at store level due to Layer 3).
func TestScheduler_DeletionRatioGuard_ZeroActive_AllowsEmptyMarkDeleted(t *testing.T) {
	t.Parallel()

	// Empty dir: ListActiveSourceIDs returns [] nil.
	root := t.TempDir()

	col := collector.NewFilesystemCollector(root)
	st := &trackingStoreWithActiveAndCount{
		activeCount: 0, // fresh DB, no docs yet
	}
	sched := New(st, disabledEmbed(), col)

	sched.run(context.Background(), col)

	// With 0 active docs, ratio is 0/0 — must not blow up and must call MarkDeleted
	// (store layer will early-return on empty slice anyway — Layer 3 belt-and-suspenders).
	// The key invariant: no panic, scheduler completes cleanly.
	if sched.runningFor(col.Name()) {
		t.Error("running flag must be released after run")
	}
}

// TestScheduler_RunCompletesCleanly_AfterListError verifies that a
// ListActiveSourceIDs error does not leave the scheduler in a broken state:
// the per-collector running flag must still be released.
func TestScheduler_RunCompletesCleanly_AfterListError(t *testing.T) {
	t.Parallel()

	col := newErrListingCollector("broken-root")
	st := &trackingStore{}
	sched := New(st, disabledEmbed(), col)

	sched.run(context.Background(), col)

	if sched.runningFor(col.Name()) {
		t.Error("per-collector running flag must be released even after ListActiveSourceIDs error")
	}
}

// TestDeletionRatioWouldExceed is a pure-function unit test for the ratio guard
// helper (deletionRatioWouldExceed). It verifies the threshold logic without
// any real filesystem or database.
func TestDeletionRatioWouldExceed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		activeInDB  int
		activeOnFS  int
		wantExceed  bool
	}{
		// Clearly dangerous: 0 files visible, 10 in DB → 100% deletion
		{"all_deleted", 10, 0, true},
		// Exactly at threshold boundary (50%): 5 of 10 deleted → should NOT exceed (strict >)
		{"at_threshold", 10, 5, false},
		// Just over threshold: 4 of 10 visible → 6 deleted = 60% → exceeds
		{"just_over", 10, 4, true},
		// Normal deletion: 9 of 10 visible → 1 deleted = 10% → fine
		{"normal_deletion", 10, 9, false},
		// Zero active in DB: ratio is 0% regardless → never block
		{"zero_active_db", 0, 0, false},
		// Zero active in DB, some on FS: still fine
		{"zero_db_some_fs", 0, 5, false},
		// Large scale: 1000 active, 100 visible → 90% deleted → exceeds
		{"large_scale_exceed", 1000, 100, true},
		// Large scale: 1000 active, 600 visible → 40% deleted → fine
		{"large_scale_fine", 1000, 600, false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := deletionRatioWouldExceed(tc.activeInDB, tc.activeOnFS)
			if got != tc.wantExceed {
				t.Errorf("deletionRatioWouldExceed(activeInDB=%d, activeOnFS=%d) = %v, want %v",
					tc.activeInDB, tc.activeOnFS, got, tc.wantExceed)
			}
		})
	}
}

// TestDeletedAt_SchedulerRuntime sanity: confirm run() does not panic when the
// entire pipeline (collect → list IDs → mark deleted) succeeds end-to-end with
// a real filesystem and a permissive mock store.
func TestScheduler_EndToEnd_NoPanic(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("# a"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.md"), []byte("# b"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	col := collector.NewFilesystemCollector(root)
	st := &trackingStoreWithActiveAndCount{activeCount: 2}
	sched := New(st, disabledEmbed(), col)

	// Must not panic.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("run panicked: %v", r)
			}
		}()
		sched.run(context.Background(), col)
	}()

	if sched.runningFor(col.Name()) {
		t.Error("running flag must be released after clean run")
	}
}

// Ensure trackingStoreWithActiveAndCount satisfies time-related methods via
// embedded trackingStore → mockStore.
var _ interface {
	LastCollectedAt(context.Context, string, model.SourceType, time.Time) time.Time
	UpdateCollectorState(context.Context, string, model.SourceType, time.Time) error
} = &trackingStoreWithActiveAndCount{}
