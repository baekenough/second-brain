package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
)

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

func (m *mockStore) LastCollectedAt(_ context.Context, _ model.SourceType, fallback time.Time) time.Time {
	return fallback
}

func (m *mockStore) RecordCollectionLog(_ context.Context, _ model.SourceType, _ time.Time, _ int, _ error) error {
	return nil
}

func (m *mockStore) MarkDeleted(_ context.Context, _ model.SourceType, _ []string) (int, error) {
	return 0, nil
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

// disabledEmbed returns a no-op EmbedClient (apiURL == "" → Enabled() false).
func disabledEmbed() *search.EmbedClient {
	return search.NewEmbedClient("", "", "", "")
}

// --- tests ---

// TestScheduler_ConcurrentRun_Skipped verifies that when a slow collection is
// already in progress, a concurrent call to run() is immediately skipped rather
// than executing a second collection cycle.
func TestScheduler_ConcurrentRun_Skipped(t *testing.T) {
	t.Parallel()

	col := newSlowCollector(200 * time.Millisecond)
	store := &mockStore{}
	sched := New(store, disabledEmbed(), col)

	ctx := context.Background()

	// Start the first run in the background — it will hold running=true for ~200 ms.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sched.run(ctx, col)
	}()

	// Give the goroutine a moment to acquire the lock.
	time.Sleep(20 * time.Millisecond)

	// The second run should be skipped (running is still true).
	sched.run(ctx, col)

	// Wait for the first run to finish before asserting.
	wg.Wait()

	// Only 1 actual Collect() invocation should have occurred.
	if got := col.callCount(); got != 1 {
		t.Errorf("expected 1 Collect call, got %d", got)
	}
}

// TestScheduler_SequentialRun_OK verifies that after the first run completes
// the running flag is cleared and a subsequent run executes normally.
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

	// Verify the running flag was released.
	if sched.running.Load() {
		t.Error("running flag should be false after both runs complete")
	}
}

// TestScheduler_PanicInRun_ReleasesLock verifies that if a collector panics
// the deferred running.Store(false) still fires, allowing subsequent runs to
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

	// The running flag MUST be false after the panic so the next run can proceed.
	if sched.running.Load() {
		t.Fatal("running flag is still true after panic — defer did not fire")
	}

	// Second run should execute normally (no skip).
	sched.run(ctx, col)

	if sched.running.Load() {
		t.Error("running flag should be false after second run completes")
	}
}
