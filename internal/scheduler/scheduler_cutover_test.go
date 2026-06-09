package scheduler

// scheduler_cutover_test.go — Tests for the WithCutover builder and cutover
// propagation to CutoverAwareCollectors.
//
// Requirements:
//  1. WithCutover sets s.cutover and is fluent (returns *Scheduler).
//  2. runCollector propagates the cutover to CutoverAwareCollectors.
//  3. runCollector advances the since watermark to the cutover when since < cutover
//     (handles date-watermark collectors: gmail/calendar/secretary/llm-memory).
//  4. Zero cutover = no change to since (existing behaviour preserved).

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
)

// cutoverCapturingCollector is a test double that:
//   - Implements CutoverAwareCollector (WithCutover) to capture the cutover
//     value the scheduler passes.
//   - Records the `since` argument passed to Collect.
type cutoverCapturingCollector struct {
	name    string
	source  model.SourceType
	enabled bool

	mu              sync.Mutex
	capturedCutover time.Time
	capturedSince   time.Time
}

func newCutoverCapturingCollector(name string) *cutoverCapturingCollector {
	return &cutoverCapturingCollector{
		name:    name,
		source:  model.SourceType("test-cutover-" + name),
		enabled: true,
	}
}

func (c *cutoverCapturingCollector) Name() string             { return c.name }
func (c *cutoverCapturingCollector) Source() model.SourceType { return c.source }
func (c *cutoverCapturingCollector) Enabled() bool            { return c.enabled }
func (c *cutoverCapturingCollector) Collect(_ context.Context, since time.Time) ([]model.Document, error) {
	c.mu.Lock()
	c.capturedSince = since
	c.mu.Unlock()
	return nil, nil
}

// WithCutover implements CutoverAwareCollector.
func (c *cutoverCapturingCollector) WithCutover(t time.Time) {
	c.mu.Lock()
	c.capturedCutover = t
	c.mu.Unlock()
}

func (c *cutoverCapturingCollector) getCutover() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capturedCutover
}

func (c *cutoverCapturingCollector) getSince() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capturedSince
}

// --- tests ---

// TestScheduler_WithCutover_FluencyAndField verifies that WithCutover is a
// fluent builder that sets s.cutover correctly.
func TestScheduler_WithCutover_FluencyAndField(t *testing.T) {
	t.Parallel()

	cutover := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	col := newCountingCollector("alpha", true)
	st := &mockStore{}

	sched := New(st, disabledEmbed(), col).WithCutover(cutover)
	if !sched.cutover.Equal(cutover) {
		t.Errorf("s.cutover = %v, want %v", sched.cutover, cutover)
	}
}

// TestScheduler_WithCutover_Zero_NoOp verifies that calling WithCutover with
// a zero time is a valid no-op (cutover remains disabled).
func TestScheduler_WithCutover_Zero_NoOp(t *testing.T) {
	t.Parallel()

	col := newCountingCollector("beta", true)
	st := &mockStore{}

	sched := New(st, disabledEmbed(), col).WithCutover(time.Time{})
	if !sched.cutover.IsZero() {
		t.Errorf("s.cutover should be zero, got %v", sched.cutover)
	}
}

// TestScheduler_Cutover_PropagatedToCutoverAwareCollector verifies that
// runCollector calls WithCutover on collectors that implement
// CutoverAwareCollector.
func TestScheduler_Cutover_PropagatedToCutoverAwareCollector(t *testing.T) {
	t.Parallel()

	cutover := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	col := newCutoverCapturingCollector("propagate")
	st := &mockStore{}

	sched := New(st, disabledEmbed(), col).WithCutover(cutover)
	sched.run(context.Background(), col)

	if got := col.getCutover(); !got.Equal(cutover) {
		t.Errorf("collector.cutover = %v, want %v", got, cutover)
	}
}

// TestScheduler_Cutover_ZeroNotPropagated verifies that when s.cutover is zero,
// WithCutover(time.Time{}) is still called on the collector (zero is passed
// through, which the collector treats as disabled).
func TestScheduler_Cutover_ZeroNotPropagated(t *testing.T) {
	t.Parallel()

	col := newCutoverCapturingCollector("zero-propagate")
	st := &mockStore{}

	sched := New(st, disabledEmbed(), col) // no WithCutover call
	sched.run(context.Background(), col)

	if got := col.getCutover(); !got.IsZero() {
		t.Errorf("collector.cutover should be zero when scheduler has no cutover, got %v", got)
	}
}

// sinceCapturingStore is a mockStore variant that records the LastCollectedAt
// return value (simulating a watermark before the cutover).
type sinceCapturingStore struct {
	mockStore
	since time.Time // value returned by LastCollectedAt
}

func (s *sinceCapturingStore) LastCollectedAt(_ context.Context, _ string, _ model.SourceType, _ time.Time) time.Time {
	return s.since
}

// TestScheduler_Cutover_AdvancesSinceWhenBeforeCutover verifies that when the
// stored since watermark is before the cutover, runCollector advances since to
// the cutover value before calling Collect.
//
// This is the "date-watermark collector" path: gmail, calendar, secretary,
// llm-memory all receive since as their start time, so advancing since is
// sufficient to prevent them from fetching pre-cutover data.
func TestScheduler_Cutover_AdvancesSinceWhenBeforeCutover(t *testing.T) {
	t.Parallel()

	cutover := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	stored := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC) // well before cutover

	col := newCutoverCapturingCollector("advance-since")
	st := &sinceCapturingStore{since: stored}

	sched := New(st, disabledEmbed(), col).WithCutover(cutover)
	sched.run(context.Background(), col)

	got := col.getSince()
	if !got.Equal(cutover) {
		t.Errorf("since passed to Collect = %v, want %v (cutover)", got, cutover)
	}
}

// TestScheduler_Cutover_DoesNotRetractSinceWhenAfterCutover verifies that when
// the stored since watermark is already after the cutover, runCollector does NOT
// retract since backwards. The watermark must never go backwards.
func TestScheduler_Cutover_DoesNotRetractSinceWhenAfterCutover(t *testing.T) {
	t.Parallel()

	cutover := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	stored := time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC) // after cutover

	col := newCutoverCapturingCollector("no-retract")
	st := &sinceCapturingStore{since: stored}

	sched := New(st, disabledEmbed(), col).WithCutover(cutover)
	sched.run(context.Background(), col)

	got := col.getSince()
	if !got.Equal(stored) {
		t.Errorf("since passed to Collect = %v, want %v (stored watermark, not retracted to cutover)", got, stored)
	}
}

// TestScheduler_Cutover_ZeroCutoverPreservesSince verifies that when s.cutover
// is zero (disabled), the since watermark is passed through unchanged.
func TestScheduler_Cutover_ZeroCutoverPreservesSince(t *testing.T) {
	t.Parallel()

	stored := time.Date(2024, 3, 15, 12, 0, 0, 0, time.UTC)

	col := newCutoverCapturingCollector("zero-preserve")
	st := &sinceCapturingStore{since: stored}

	sched := New(st, disabledEmbed(), col) // zero cutover
	sched.run(context.Background(), col)

	got := col.getSince()
	if !got.Equal(stored) {
		t.Errorf("since passed to Collect = %v, want %v (zero cutover should not modify since)", got, stored)
	}
}
