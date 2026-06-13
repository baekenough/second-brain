package scheduler

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/collector"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
)

// countingCollector records every Collect invocation.
type countingCollector struct {
	name    string
	source  model.SourceType
	enabled bool

	mu    sync.Mutex
	calls int
}

func newCountingCollector(name string, enabled bool) *countingCollector {
	return &countingCollector{
		name:    name,
		source:  model.SourceType("test-" + name),
		enabled: enabled,
	}
}

func (c *countingCollector) Name() string             { return c.name }
func (c *countingCollector) Source() model.SourceType { return c.source }
func (c *countingCollector) Enabled() bool            { return c.enabled }
func (c *countingCollector) Collect(_ context.Context, _ time.Time) ([]model.Document, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return nil, nil
}

func (c *countingCollector) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// --- test doubles ---

// mockStore implements DocumentUpserter for tests.
type mockStore struct {
	mu      sync.Mutex
	upserts int
}

func (m *mockStore) Upsert(_ context.Context, _ *model.Document) error {
	m.mu.Lock()
	m.upserts++
	m.mu.Unlock()
	return nil
}

func (m *mockStore) LastCollectedAt(_ context.Context, _ string, _ model.SourceType, fallback time.Time) time.Time {
	return fallback
}

func (m *mockStore) UpdateCollectorState(_ context.Context, _ string, _ model.SourceType, _ time.Time) error {
	return nil
}

func (m *mockStore) RecordCollectionLog(_ context.Context, _ model.SourceType, _ time.Time, _ int, _ error) error {
	return nil
}

func (m *mockStore) MarkDeleted(_ context.Context, _ model.SourceType, _ []string) (int, error) {
	return 0, nil
}

func (m *mockStore) ListUnembedded(_ context.Context, _ int) ([]*model.Document, error) {
	return nil, nil
}

func (m *mockStore) UpdateEmbedding(_ context.Context, _ *model.Document) error {
	return nil
}

func (m *mockStore) ActiveSourceIDSet(_ context.Context, _ model.SourceType) (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

// CountActiveDocuments satisfies DocumentUpserter (#148: promoted from optional
// ActiveDocumentCounter to required interface method).
// Returns 0 by default so the deletion-ratio guard never blocks in pure-mock tests.
func (m *mockStore) CountActiveDocuments(_ context.Context, _ model.SourceType) (int, error) {
	return 0, nil
}

// mockStoreErrorIDs is like mockStore but returns an error from ActiveSourceIDSet,
// simulating a transient store failure during the indexed-ID pre-load.
type mockStoreErrorIDs struct {
	mockStore
}

func (m *mockStoreErrorIDs) ActiveSourceIDSet(_ context.Context, _ model.SourceType) (map[string]struct{}, error) {
	return nil, errors.New("store unavailable")
}

// slowCollector blocks for the given duration then returns zero documents.
// Enabled() always returns true so the scheduler will run it.
type slowCollector struct {
	delay    time.Duration
	callsMu  sync.Mutex
	calls    int
}

func newSlowCollector(delay time.Duration) *slowCollector {
	return &slowCollector{delay: delay}
}

func (c *slowCollector) Name() string                 { return "slow" }
func (c *slowCollector) Source() model.SourceType     { return "test-slow" }
func (c *slowCollector) Enabled() bool                { return true }
func (c *slowCollector) Collect(_ context.Context, _ time.Time) ([]model.Document, error) {
	c.callsMu.Lock()
	c.calls++
	c.callsMu.Unlock()
	time.Sleep(c.delay)
	return nil, nil
}

func (c *slowCollector) callCount() int {
	c.callsMu.Lock()
	defer c.callsMu.Unlock()
	return c.calls
}

// panicCollector panics on the first Collect call, normal on subsequent calls.
type panicCollector struct {
	callsMu sync.Mutex
	calls   int
}

func (c *panicCollector) Name() string                 { return "panic-col" }
func (c *panicCollector) Source() model.SourceType     { return "test-panic" }
func (c *panicCollector) Enabled() bool                { return true }
func (c *panicCollector) Collect(_ context.Context, _ time.Time) ([]model.Document, error) {
	c.callsMu.Lock()
	n := c.calls
	c.calls++
	c.callsMu.Unlock()
	if n == 0 {
		panic(errors.New("intentional panic from test collector"))
	}
	return nil, nil
}

// instantCollector returns immediately with zero documents.
type instantCollector struct{}

func (c *instantCollector) Name() string             { return "instant" }
func (c *instantCollector) Source() model.SourceType { return "test-instant" }
func (c *instantCollector) Enabled() bool            { return true }
func (c *instantCollector) Collect(_ context.Context, _ time.Time) ([]model.Document, error) {
	return nil, nil
}

// blockingCollector blocks inside Collect until its release channel is closed.
// It records how many times Collect was entered and exited, allowing tests to
// observe concurrent execution without time.Sleep races.
type blockingCollector struct {
	name    string
	source  model.SourceType
	release chan struct{} // close to unblock all waiting Collects

	mu      sync.Mutex
	entered int // incremented as soon as Collect starts
	exited  int // incremented after Collect returns
}

func newBlockingCollector(name string) *blockingCollector {
	return &blockingCollector{
		name:    name,
		source:  model.SourceType("test-blocking-" + name),
		release: make(chan struct{}),
	}
}

func (c *blockingCollector) Name() string             { return c.name }
func (c *blockingCollector) Source() model.SourceType { return c.source }
func (c *blockingCollector) Enabled() bool            { return true }
func (c *blockingCollector) Collect(_ context.Context, _ time.Time) ([]model.Document, error) {
	c.mu.Lock()
	c.entered++
	c.mu.Unlock()

	<-c.release // block until released

	c.mu.Lock()
	c.exited++
	c.mu.Unlock()
	return nil, nil
}

func (c *blockingCollector) enteredCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entered
}

func (c *blockingCollector) exitedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exited
}

// disabledEmbed returns a no-op EmbeddingEngine (apiURL == "" → Enabled() false).
func disabledEmbed() search.EmbeddingEngine {
	return search.NewEmbedClient("", "", "", "", 0)
}

// --- tests ---

// TestScheduler_ConcurrentRun_Skipped verifies that when a slow collection is
// already in progress, a concurrent call to run() for the SAME collector is
// immediately skipped rather than executing a second collection cycle.
func TestScheduler_ConcurrentRun_Skipped(t *testing.T) {
	t.Parallel()

	col := newSlowCollector(200 * time.Millisecond)
	store := &mockStore{}
	sched := New(store, disabledEmbed(), col)

	ctx := context.Background()

	// Start the first run in the background — it will hold the per-collector
	// flag for ~200 ms.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sched.run(ctx, col)
	}()

	// Give the goroutine a moment to acquire the lock.
	time.Sleep(20 * time.Millisecond)

	// The second run should be skipped (per-collector flag is still held).
	sched.run(ctx, col)

	// Wait for the first run to finish before asserting.
	wg.Wait()

	// Only 1 actual Collect() invocation should have occurred.
	if got := col.callCount(); got != 1 {
		t.Errorf("expected 1 Collect call, got %d", got)
	}
}

// TestScheduler_SequentialRun_OK verifies that after the first run completes
// the per-collector running flag is cleared and a subsequent run executes normally.
func TestScheduler_SequentialRun_OK(t *testing.T) {
	t.Parallel()

	col := newSlowCollector(0) // instant
	store := &mockStore{}
	sched := New(store, disabledEmbed(), col)

	ctx := context.Background()

	sched.run(ctx, col)
	sched.run(ctx, col)

	if got := col.callCount(); got != 2 {
		t.Errorf("expected 2 Collect calls, got %d", got)
	}

	// Verify the per-collector running flag was released.
	if sched.runningFor(col.Name()) {
		t.Error("per-collector running flag should be false after both runs complete")
	}
}

// TestScheduler_PanicInRun_ReleasesLock verifies that if a collector panics
// the deferred flag.Store(false) still fires, allowing subsequent runs to
// succeed (i.e., the scheduler is not permanently locked).
func TestScheduler_PanicInRun_ReleasesLock(t *testing.T) {
	t.Parallel()

	col := &panicCollector{}
	store := &mockStore{}
	sched := New(store, disabledEmbed(), col)

	ctx := context.Background()

	// First run should panic; recover it so the test continues.
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected a panic from the first run, but none occurred")
			}
		}()
		sched.run(ctx, col)
	}()

	// The per-collector running flag MUST be false after the panic so the
	// next run can proceed.
	if sched.runningFor(col.Name()) {
		t.Fatal("per-collector running flag is still true after panic — defer did not fire")
	}

	// Second run should execute normally (no skip).
	sched.run(ctx, col)

	if sched.runningFor(col.Name()) {
		t.Error("per-collector running flag should be false after second run completes")
	}
}

// TestScheduler_RunAll_AllEnabledCollectorsRun verifies the starvation fix:
// a single runAll call runs every enabled collector exactly once and skips
// disabled collectors entirely.
//
// Before the fix, Register() added one cron job per collector; on the same
// tick they all competed for s.running and only one won — the rest were
// silently skipped. With the fix, Register() adds a single job that calls
// runAll(), which iterates through all collectors sequentially while holding
// the run-lock for the whole cycle.
func TestScheduler_RunAll_AllEnabledCollectorsRun(t *testing.T) {
	t.Parallel()

	col1 := newCountingCollector("alpha", true)
	col2 := newCountingCollector("bravo", true)
	col3 := newCountingCollector("charlie", true)
	disabledCol := newCountingCollector("delta", false)

	store := &mockStore{}
	sched := New(store, disabledEmbed(), col1, col2, col3, disabledCol)

	sched.runAll(context.Background())

	for _, col := range []*countingCollector{col1, col2, col3} {
		if got := col.callCount(); got != 1 {
			t.Errorf("collector %q: expected 1 Collect call, got %d", col.Name(), got)
		}
	}
	if got := disabledCol.callCount(); got != 0 {
		t.Errorf("disabled collector %q: expected 0 Collect calls, got %d", disabledCol.Name(), got)
	}

	// Running flag must be released after the cycle.
	if sched.running.Load() {
		t.Error("running flag should be false after runAll completes")
	}
}

// TestScheduler_RunAll_SkipsWhenRunning verifies that a concurrent runAll call
// is dropped (returns immediately) when a cycle is already in progress.
func TestScheduler_RunAll_SkipsWhenRunning(t *testing.T) {
	t.Parallel()

	// Use a slow collector so the first runAll holds the lock long enough for
	// the second call to arrive while it is still running.
	slow := newSlowCollector(200 * time.Millisecond)
	store := &mockStore{}
	sched := New(store, disabledEmbed(), slow)

	ctx := context.Background()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sched.runAll(ctx)
	}()

	// Let the first runAll acquire the lock.
	time.Sleep(20 * time.Millisecond)

	// Second runAll must be skipped immediately.
	sched.runAll(ctx)

	wg.Wait()

	if got := slow.callCount(); got != 1 {
		t.Errorf("expected 1 Collect call, got %d (second tick should have been skipped)", got)
	}
}

// TestScheduler_RunAll_SecondTickRunsAfterFirst verifies that after the first
// runAll cycle completes the running flag is cleared and a subsequent call
// executes all collectors again (normal sequential-tick behaviour).
func TestScheduler_RunAll_SecondTickRunsAfterFirst(t *testing.T) {
	t.Parallel()

	col := newCountingCollector("echo", true)
	store := &mockStore{}
	sched := New(store, disabledEmbed(), col)

	ctx := context.Background()
	sched.runAll(ctx)
	sched.runAll(ctx)

	if got := col.callCount(); got != 2 {
		t.Errorf("expected 2 Collect calls across two ticks, got %d", got)
	}
	if sched.running.Load() {
		t.Error("running flag should be false after both runAll calls complete")
	}
}

// TestScheduler_ActiveSourceIDSet_Error_ResetsCollector verifies that when
// ActiveSourceIDSet returns an error the scheduler calls WithIndexedIDs(nil) on
// the FilesystemCollector, clearing any stale indexedIDs map left over from a
// previous successful run. The observable consequence is that the run still
// completes (non-fatal path) and the per-collector running flag is released.
func TestScheduler_ActiveSourceIDSet_Error_ResetsCollector(t *testing.T) {
	t.Parallel()

	// Build a real temporary filesystem collector and prime it with a non-nil
	// indexedIDs set — simulating a successful prior run that populated the map.
	tmpDir := t.TempDir()
	fsc := collector.NewFilesystemCollector(tmpDir)
	fsc.WithIndexedIDs(map[string]struct{}{"stale-entry": {}})

	store := &mockStoreErrorIDs{}
	sched := New(store, disabledEmbed(), fsc)

	ctx := context.Background()
	// run() must complete without panic and release the per-collector flag.
	sched.run(ctx, fsc)

	if sched.runningFor(fsc.Name()) {
		t.Error("per-collector running flag should be false after run with ActiveSourceIDSet error")
	}
	// A second run must also succeed — confirming the scheduler is not broken
	// by the error path.
	sched.run(ctx, fsc)
	if sched.runningFor(fsc.Name()) {
		t.Error("per-collector running flag should be false after second run")
	}
}

// ---------------------------------------------------------------------------
// Per-collector concurrency tests (proving the head-of-line-blocking fix)
// ---------------------------------------------------------------------------

// TestScheduler_DistinctCollectors_RunConcurrently proves that two collectors
// with distinct names execute concurrently: a blocking collector does NOT
// prevent a second collector from entering its Collect method within the same
// scheduler interval.
//
// The test uses channels rather than time.Sleep to avoid flakiness: the fast
// collector must enter Collect before the blocking one is released, confirming
// true concurrency.
func TestScheduler_DistinctCollectors_RunConcurrently(t *testing.T) {
	t.Parallel()

	slow := newBlockingCollector("slow-col")
	fast := newCountingCollector("fast-col", true)
	st := &mockStore{}
	sched := New(st, disabledEmbed(), slow, fast)

	ctx := context.Background()

	// Launch the slow collector in the background; it blocks until released.
	var wgSlow sync.WaitGroup
	wgSlow.Add(1)
	go func() {
		defer wgSlow.Done()
		sched.run(ctx, slow)
	}()

	// Wait until the slow collector has entered Collect (holds its own flag).
	for slow.enteredCount() == 0 {
		runtime.Gosched()
	}

	// At this point the slow collector is inside Collect and has NOT released.
	// The fast collector MUST be able to run independently — it has its own flag.
	sched.run(ctx, fast)

	// fast should have executed exactly once.
	if got := fast.callCount(); got != 1 {
		t.Errorf("fast collector: expected 1 call while slow was blocked, got %d", got)
	}

	// Release the slow collector and wait for it to finish.
	close(slow.release)
	wgSlow.Wait()

	if got := slow.exitedCount(); got != 1 {
		t.Errorf("slow collector: expected 1 completed Collect, got %d", got)
	}
}

// TestScheduler_SameCollector_OverlappingTickSkipped proves that a single
// collector's overlapping tick is skipped while that collector is still running.
// This is the per-collector analogue of the old global-lock skip test.
func TestScheduler_SameCollector_OverlappingTickSkipped(t *testing.T) {
	t.Parallel()

	blocking := newBlockingCollector("overlap-col")
	st := &mockStore{}
	sched := New(st, disabledEmbed(), blocking)

	ctx := context.Background()

	// First invocation: blocks inside Collect.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sched.run(ctx, blocking)
	}()

	// Wait until Collect has been entered.
	for blocking.enteredCount() == 0 {
		runtime.Gosched()
	}

	// Overlapping tick for the SAME collector: must be skipped.
	sched.run(ctx, blocking)

	// Release the first goroutine.
	close(blocking.release)
	wg.Wait()

	// Only one invocation should have entered Collect.
	if got := blocking.enteredCount(); got != 1 {
		t.Errorf("expected 1 Collect entry (overlap skipped), got %d", got)
	}
}

// TestScheduler_DistinctCollectors_BothRun verifies that when two enabled
// collectors are registered, both are eventually executed (no starvation).
func TestScheduler_DistinctCollectors_BothRun(t *testing.T) {
	t.Parallel()

	col1 := newCountingCollector("col-one", true)
	col2 := newCountingCollector("col-two", true)
	st := &mockStore{}
	sched := New(st, disabledEmbed(), col1, col2)

	ctx := context.Background()

	// Run each collector once sequentially via separate run() calls — as cron
	// would do when each has its own goroutine.
	sched.run(ctx, col1)
	sched.run(ctx, col2)

	if got := col1.callCount(); got != 1 {
		t.Errorf("col-one: expected 1 call, got %d", got)
	}
	if got := col2.callCount(); got != 1 {
		t.Errorf("col-two: expected 1 call, got %d", got)
	}

	// Both per-collector flags must be released.
	if sched.runningFor(col1.Name()) {
		t.Error("col-one: per-collector flag not released")
	}
	if sched.runningFor(col2.Name()) {
		t.Error("col-two: per-collector flag not released")
	}
}
