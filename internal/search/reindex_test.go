package search

import (
	"context"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/store"
)

// --- mock implementations ---

type mockEvalMetrics struct {
	rec *store.EvalMetricsRecord
	err error
}

func (m *mockEvalMetrics) Latest(_ context.Context) (*store.EvalMetricsRecord, error) {
	return m.rec, m.err
}

type mockDocCounter struct {
	counts map[string]int
	err    error
}

func (m *mockDocCounter) CountBySource(_ context.Context) (map[string]int, error) {
	return m.counts, m.err
}

type mockReindexState struct {
	state *store.ReindexState
	err   error
}

func (m *mockReindexState) Latest(_ context.Context) (*store.ReindexState, error) {
	return m.state, m.err
}

// --- helpers ---

func baseConfig() ReindexConfig {
	return ReindexConfig{
		EvalRegressionThreshold: 0.05,
		NewDocThreshold:         1000,
		StalenessDays:           7,
	}
}

func recentState(docCount int) *store.ReindexState {
	return &store.ReindexState{
		ID:                1,
		LastReindexAt:     time.Now().Add(-24 * time.Hour), // 1 day ago — well within 7-day window
		DocCountAtReindex: docCount,
		TriggerReason:     "manual",
	}
}

// --- tests ---

func TestReindexChecker_NoState_RecommendsInitial(t *testing.T) {
	t.Parallel()

	checker := NewReindexChecker(
		baseConfig(),
		&mockEvalMetrics{},
		&mockDocCounter{counts: map[string]int{"notion": 500}},
		&mockReindexState{state: nil}, // no prior state
	)

	rec, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rec.ShouldReindex {
		t.Error("expected reindex recommendation when no prior state exists")
	}
	if len(rec.Reasons) == 0 {
		t.Error("expected at least one reason")
	}
}

func TestReindexChecker_BelowAllThresholds_NoRecommendation(t *testing.T) {
	t.Parallel()

	checker := NewReindexChecker(
		baseConfig(),
		&mockEvalMetrics{rec: &store.EvalMetricsRecord{NDCG5: 0.8}},
		&mockDocCounter{counts: map[string]int{"notion": 1100}}, // 100 new docs (below 1000)
		&mockReindexState{state: recentState(1000)},
	)

	rec, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.ShouldReindex {
		t.Errorf("expected no recommendation but got reasons: %v", rec.Reasons)
	}
	if len(rec.Reasons) != 0 {
		t.Errorf("expected empty reasons, got %v", rec.Reasons)
	}
}

func TestReindexChecker_Staleness_Recommends(t *testing.T) {
	t.Parallel()

	oldState := &store.ReindexState{
		ID:                1,
		LastReindexAt:     time.Now().Add(-8 * 24 * time.Hour), // 8 days ago — exceeds 7-day threshold
		DocCountAtReindex: 1000,
		TriggerReason:     "manual",
	}

	checker := NewReindexChecker(
		baseConfig(),
		&mockEvalMetrics{rec: &store.EvalMetricsRecord{NDCG5: 0.8}},
		&mockDocCounter{counts: map[string]int{"notion": 1050}}, // only 50 new docs
		&mockReindexState{state: oldState},
	)

	rec, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rec.ShouldReindex {
		t.Error("expected reindex recommendation due to staleness")
	}
	found := false
	for _, r := range rec.Reasons {
		if len(r) > 0 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected staleness reason, got %v", rec.Reasons)
	}
}

func TestReindexChecker_DocGrowth_Recommends(t *testing.T) {
	t.Parallel()

	checker := NewReindexChecker(
		baseConfig(),
		&mockEvalMetrics{rec: &store.EvalMetricsRecord{NDCG5: 0.8}},
		&mockDocCounter{counts: map[string]int{"notion": 2100}}, // 1100 new docs — exceeds 1000
		&mockReindexState{state: recentState(1000)},
	)

	rec, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rec.ShouldReindex {
		t.Error("expected reindex recommendation due to doc growth")
	}
}

func TestReindexChecker_EvalRegression_Recommends(t *testing.T) {
	t.Parallel()

	checker := NewReindexChecker(
		baseConfig(),
		&mockEvalMetrics{rec: &store.EvalMetricsRecord{NDCG5: 0.8}},
		&mockDocCounter{counts: map[string]int{"notion": 1050}},
		&mockReindexState{state: recentState(1000)},
	)

	// Simulate a 10% regression on ndcg5 (0.8 → 0.72, drop = 10% > 5% threshold)
	rec, err := checker.CheckWithBaseline(
		context.Background(),
		EvalSnapshot{NDCG5: 0.72, NDCG10: 0.80, MRR10: 0.75},
		EvalSnapshot{NDCG5: 0.80, NDCG10: 0.80, MRR10: 0.75},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rec.ShouldReindex {
		t.Error("expected reindex recommendation due to eval regression")
	}
}

func TestReindexChecker_NoRegressionBelowThreshold(t *testing.T) {
	t.Parallel()

	checker := NewReindexChecker(
		baseConfig(),
		&mockEvalMetrics{rec: &store.EvalMetricsRecord{NDCG5: 0.8}},
		&mockDocCounter{counts: map[string]int{"notion": 1050}},
		&mockReindexState{state: recentState(1000)},
	)

	// 3% drop — below the 5% regression threshold
	rec, err := checker.CheckWithBaseline(
		context.Background(),
		EvalSnapshot{NDCG5: 0.776, NDCG10: 0.80, MRR10: 0.75},
		EvalSnapshot{NDCG5: 0.80, NDCG10: 0.80, MRR10: 0.75},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.ShouldReindex {
		t.Errorf("expected no recommendation for sub-threshold regression, got reasons: %v", rec.Reasons)
	}
}

func TestReindexChecker_MultipleReasons(t *testing.T) {
	t.Parallel()

	oldState := &store.ReindexState{
		ID:                1,
		LastReindexAt:     time.Now().Add(-10 * 24 * time.Hour), // stale
		DocCountAtReindex: 500,
		TriggerReason:     "manual",
	}

	checker := NewReindexChecker(
		baseConfig(),
		&mockEvalMetrics{rec: &store.EvalMetricsRecord{NDCG5: 0.8}},
		&mockDocCounter{counts: map[string]int{"notion": 2000}}, // 1500 new docs
		&mockReindexState{state: oldState},
	)

	// Also add eval regression
	rec, err := checker.CheckWithBaseline(
		context.Background(),
		EvalSnapshot{NDCG5: 0.70, NDCG10: 0.80, MRR10: 0.75},
		EvalSnapshot{NDCG5: 0.80, NDCG10: 0.80, MRR10: 0.75},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rec.ShouldReindex {
		t.Error("expected reindex recommendation")
	}
	// Expect at least 3 reasons: staleness + doc growth + eval regression
	if len(rec.Reasons) < 3 {
		t.Errorf("expected at least 3 reasons, got %d: %v", len(rec.Reasons), rec.Reasons)
	}
}

func TestReindexChecker_ExactThresholdBoundary(t *testing.T) {
	t.Parallel()

	// Doc count exactly at threshold (1000 new docs) — should trigger.
	checker := NewReindexChecker(
		baseConfig(),
		&mockEvalMetrics{rec: &store.EvalMetricsRecord{NDCG5: 0.8}},
		&mockDocCounter{counts: map[string]int{"notion": 2000}}, // exactly 1000 new
		&mockReindexState{state: recentState(1000)},
	)

	rec, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rec.ShouldReindex {
		t.Error("expected recommendation when delta equals threshold")
	}
}

func TestSumCounts(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		counts   map[string]int
		expected int
	}{
		{"empty", map[string]int{}, 0},
		{"single", map[string]int{"notion": 42}, 42},
		{"multiple", map[string]int{"notion": 100, "telegram": 50, "discord": 25}, 175},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := sumCounts(tc.counts)
			if got != tc.expected {
				t.Errorf("sumCounts(%v) = %d, want %d", tc.counts, got, tc.expected)
			}
		})
	}
}

func TestEvalRegressionReason(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		threshold float64
		cur, base float64 // ndcg5 only for simplicity; ndcg10 and mrr10 are held constant
		wantEmpty bool
	}{
		{"no regression", 0.05, 0.80, 0.80, true},
		{"below threshold", 0.05, 0.776, 0.80, true},      // 3% drop
		{"at threshold exactly", 0.05, 0.76, 0.80, false},  // 5% drop — triggers
		{"above threshold", 0.05, 0.70, 0.80, false},       // 12.5% drop
		{"zero baseline skipped", 0.05, 0.0, 0.0, true},    // no valid denominator
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reason := evalRegressionReason(
				tc.threshold,
				EvalSnapshot{NDCG5: tc.cur, NDCG10: tc.base, MRR10: tc.base},
				EvalSnapshot{NDCG5: tc.base, NDCG10: tc.base, MRR10: tc.base},
			)
			if tc.wantEmpty && reason != "" {
				t.Errorf("expected empty reason, got %q", reason)
			}
			if !tc.wantEmpty && reason == "" {
				t.Error("expected non-empty reason")
			}
		})
	}
}
