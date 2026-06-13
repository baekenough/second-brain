package scheduler

// Tests for issue #149: deletion guard edge-case coverage.
//
// These tests complement the existing scheduler_deletion_guard_test.go with
// three additional scenarios mandated by the issue:
//
//  (a) CountActiveDocuments error → fail-closed (MarkDeleted skipped)
//  (b) Legitimate >50% deletion is blocked by the guard (documents the Q1
//      trade-off) AND that DELETION_RATIO_OVERRIDE lets it through (#147).
//  (c) #148: CountActiveDocuments is now required on DocumentUpserter, so the
//      guard ALWAYS runs — there is no unguarded path. This is proven by a
//      compile-time interface check and a runtime test.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/baekenough/second-brain/internal/collector"
	"github.com/baekenough/second-brain/internal/model"
)

// ---------------------------------------------------------------------------
// Additional test doubles
// ---------------------------------------------------------------------------

// errCountStore is a DocumentUpserter whose CountActiveDocuments always returns
// an error, simulating a transient database failure during the count query.
type errCountStore struct {
	trackingStore
}

func (s *errCountStore) CountActiveDocuments(_ context.Context, _ model.SourceType) (int, error) {
	return 0, errors.New("db: count query failed")
}

// overrideCountStore is a DocumentUpserter that reports a high active-doc count
// (triggering the ratio guard) but can be used with WithDeletionRatioOverride.
type overrideCountStore struct {
	trackingStore
	activeCount int
}

func (s *overrideCountStore) CountActiveDocuments(_ context.Context, _ model.SourceType) (int, error) {
	return s.activeCount, nil
}

// ---------------------------------------------------------------------------
// (a) fail-closed: CountActiveDocuments error skips MarkDeleted
// ---------------------------------------------------------------------------

// TestScheduler_DeletionGuard_CountError_FailClosed verifies that when
// CountActiveDocuments returns an error, the scheduler skips MarkDeleted
// entirely (fail-closed behaviour).
//
// This is the critical safety property: a transient DB failure during the count
// query must NOT allow an unchecked bulk soft-delete to proceed.
func TestScheduler_DeletionGuard_CountError_FailClosed(t *testing.T) {
	t.Parallel()

	// Create a real temp dir with one file so ListActiveSourceIDs succeeds.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "kept.md"), []byte("# kept"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	col := collector.NewFilesystemCollector(root)
	st := &errCountStore{}
	sched := New(st, disabledEmbed(), col)

	sched.run(context.Background(), col)

	// MarkDeleted must NOT have been called — fail-closed.
	if got := st.markDeletedCallCount(); got != 0 {
		t.Errorf("MarkDeleted must be skipped when CountActiveDocuments returns error (fail-closed), got %d calls", got)
	}
}

// ---------------------------------------------------------------------------
// (b) Legitimate >50% deletion: blocked by guard; override lets it through.
// ---------------------------------------------------------------------------

// TestScheduler_DeletionGuard_LargeDeleteBlocked documents the Q1 trade-off:
// when a user genuinely deletes >50% of files from a source, the guard blocks
// the deletion. This is intentional — it prevents accidental bulk-delete on
// partial-mount false-positives — but it means legitimate large-scale deletions
// are blocked until the operator uses DELETION_RATIO_OVERRIDE.
//
// This test makes the trade-off explicit and machine-verifiable.
func TestScheduler_DeletionGuard_LargeDeleteBlocked(t *testing.T) {
	t.Parallel()

	// 1 file on disk, 10 docs in DB → 90% deletion ratio → guard blocks.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "only.md"), []byte("# only"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	col := collector.NewFilesystemCollector(root)
	st := &overrideCountStore{activeCount: 10}
	sched := New(st, disabledEmbed(), col)
	// Override NOT set → guard is active.

	sched.run(context.Background(), col)

	// MarkDeleted must be blocked because the deletion ratio (90%) exceeds 50%.
	// Trade-off: a legitimate large deletion is permanently blocked until the
	// operator sets DELETION_RATIO_OVERRIDE=true for a single pass.
	if got := st.markDeletedCallCount(); got != 0 {
		t.Errorf("MarkDeleted must be blocked when ratio exceeds threshold (Q1 trade-off), got %d calls", got)
	}
}

// TestScheduler_DeletionGuard_LargeDeleteOverride verifies that setting
// WithDeletionRatioOverride(true) allows a >50% deletion to proceed (#147).
// This is the escape hatch for the scenario documented in TestScheduler_DeletionGuard_LargeDeleteBlocked.
func TestScheduler_DeletionGuard_LargeDeleteOverride(t *testing.T) {
	t.Parallel()

	// Same setup: 1 file on disk, 10 docs in DB → 90% deletion ratio.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "only.md"), []byte("# only"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	col := collector.NewFilesystemCollector(root)
	st := &overrideCountStore{activeCount: 10}
	sched := New(st, disabledEmbed(), col).
		WithDeletionRatioOverride(true) // operator explicitly bypasses guard

	sched.run(context.Background(), col)

	// MarkDeleted MUST be called when the override is active, even though the
	// deletion ratio (90%) exceeds the threshold.
	if got := st.markDeletedCallCount(); got != 1 {
		t.Errorf("MarkDeleted must be called when DELETION_RATIO_OVERRIDE is set, got %d calls", got)
	}
}

// ---------------------------------------------------------------------------
// (c) #148: guard ALWAYS runs — compile-time and runtime proof.
// ---------------------------------------------------------------------------

// TestScheduler_DeletionGuard_AlwaysRuns_CompileTimeProof is a compile-time
// proof that CountActiveDocuments is now required on DocumentUpserter (#148).
//
// If a type omits CountActiveDocuments it will no longer satisfy DocumentUpserter
// and the scheduler.New() call will fail to compile. This test cannot fail at
// runtime — its presence in the test binary proves the interface constraint.
//
// The complementary runtime test (TestScheduler_DeletionGuard_CountError_FailClosed)
// proves that the guard runs and the fail-closed path is exercised.
func TestScheduler_DeletionGuard_AlwaysRuns_CompileTimeProof(t *testing.T) {
	t.Parallel()

	// This type satisfies DocumentUpserter only because it embeds mockStore,
	// which now implements CountActiveDocuments. If the method were missing from
	// the interface the embed would not help and the type assertion below would
	// be a compile error.
	var _ DocumentUpserter = &mockStore{}

	// Runtime: verify that a store with CountActiveDocuments returning 0 allows
	// the normal deletion path to proceed (no silent bypass).
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("# a"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	col := collector.NewFilesystemCollector(root)
	st := &trackingStore{} // mockStore.CountActiveDocuments returns 0 → ratio guard passes
	sched := New(st, disabledEmbed(), col)

	sched.run(context.Background(), col)

	// MarkDeleted must be called — the guard ran (count=0, ratio=0%) and passed.
	if got := st.markDeletedCallCount(); got != 1 {
		t.Errorf("MarkDeleted must be called when guard passes (count=0, ratio=0%%), got %d calls", got)
	}
}
