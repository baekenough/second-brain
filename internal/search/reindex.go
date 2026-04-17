package search

import (
	"context"
	"fmt"
	"time"

	"github.com/baekenough/second-brain/internal/store"
)

// ReindexConfig holds the threshold configuration for reindex recommendations.
// All thresholds are evaluated independently; a recommendation is generated when
// ANY threshold is breached.
type ReindexConfig struct {
	// EvalRegressionThreshold is the minimum relative metric drop that triggers
	// a reindex recommendation (e.g. 0.05 = 5%).
	EvalRegressionThreshold float64

	// NewDocThreshold is the minimum number of new documents since the last
	// reindex that triggers a recommendation.
	NewDocThreshold int

	// StalenessDays is the minimum number of days since the last reindex
	// before a staleness recommendation is generated.
	StalenessDays int
}

// DefaultReindexConfig returns a ReindexConfig with conservative production defaults.
func DefaultReindexConfig() ReindexConfig {
	return ReindexConfig{
		EvalRegressionThreshold: 0.05,
		NewDocThreshold:         1000,
		StalenessDays:           7,
	}
}

// EvalMetricsProvider is the subset of store.EvalMetricsStore used by ReindexChecker.
// It is satisfied by *store.EvalMetricsStore.
type EvalMetricsProvider interface {
	Latest(ctx context.Context) (*store.EvalMetricsRecord, error)
}

// DocCounter is the subset of store.DocumentStore used by ReindexChecker.
// It is satisfied by *store.DocumentStore.
type DocCounter interface {
	CountBySource(ctx context.Context) (map[string]int, error)
}

// ReindexStateProvider is the subset of store.ReindexStateStore used by ReindexChecker.
// It is satisfied by *store.ReindexStateStore.
type ReindexStateProvider interface {
	Latest(ctx context.Context) (*store.ReindexState, error)
}

// ReindexRecommendation is the output of the threshold check.
// ShouldReindex is true when at least one threshold has been breached.
// Reasons lists a human-readable description for each breached threshold.
//
// IMPORTANT: This is a recommendation only. The system NEVER auto-executes
// reindexing; the operator must trigger it manually ("완전 자율 금지").
type ReindexRecommendation struct {
	ShouldReindex bool     `json:"should_reindex"`
	Reasons       []string `json:"reasons"`
}

// ReindexChecker evaluates reindex thresholds against live data and returns
// a recommendation. It never initiates any mutation.
type ReindexChecker struct {
	config       ReindexConfig
	evalMetrics  EvalMetricsProvider
	docCounter   DocCounter
	reindexState ReindexStateProvider
}

// NewReindexChecker returns a ReindexChecker wired to the provided data sources.
func NewReindexChecker(
	config ReindexConfig,
	metricsStore EvalMetricsProvider,
	docCounter DocCounter,
	stateStore ReindexStateProvider,
) *ReindexChecker {
	return &ReindexChecker{
		config:       config,
		evalMetrics:  metricsStore,
		docCounter:   docCounter,
		reindexState: stateStore,
	}
}

// Check evaluates all thresholds and returns a recommendation.
//
// Three independent checks are performed:
//  1. Eval regression: the latest eval metrics are compared against a
//     regression threshold to detect quality degradation.
//  2. New document growth: the total active document count is compared
//     against the doc count recorded at the last reindex.
//  3. Staleness: the last reindex timestamp is compared against the
//     configured staleness window.
//
// When no reindex event has been recorded (first run), the system recommends
// reindexing so that a baseline is established.
func (c *ReindexChecker) Check(ctx context.Context) (ReindexRecommendation, error) {
	var reasons []string

	// --- Check 1: First-run / no prior state ---
	lastState, err := c.reindexState.Latest(ctx)
	if err != nil {
		return ReindexRecommendation{}, err
	}
	if lastState == nil {
		// No reindex has ever been recorded; recommend to establish a baseline.
		return ReindexRecommendation{
			ShouldReindex: true,
			Reasons:       []string{"no reindex state found; recommend initial reindex to establish baseline"},
		}, nil
	}

	// --- Check 2: Staleness ---
	staleness := time.Since(lastState.LastReindexAt)
	if staleness >= time.Duration(c.config.StalenessDays)*24*time.Hour {
		reasons = append(reasons,
			formatStalenessReason(staleness, c.config.StalenessDays))
	}

	// --- Check 3: New document growth ---
	counts, err := c.docCounter.CountBySource(ctx)
	if err != nil {
		return ReindexRecommendation{}, err
	}
	currentTotal := sumCounts(counts)
	delta := currentTotal - lastState.DocCountAtReindex
	if delta >= c.config.NewDocThreshold {
		reasons = append(reasons,
			formatDocGrowthReason(delta, c.config.NewDocThreshold))
	}

	// --- Check 4: Eval regression ---
	latest, err := c.evalMetrics.Latest(ctx)
	if err != nil {
		return ReindexRecommendation{}, err
	}
	if regressionReason := c.checkEvalRegression(ctx, latest); regressionReason != "" {
		reasons = append(reasons, regressionReason)
	}

	return ReindexRecommendation{
		ShouldReindex: len(reasons) > 0,
		Reasons:       reasons,
	}, nil
}

// checkEvalRegression compares the latest eval metrics against the previous run.
// Returns a reason string when a regression is detected, or "" when no regression.
// When there are fewer than two eval records the check is skipped (no comparison possible).
func (c *ReindexChecker) checkEvalRegression(ctx context.Context, latest *store.EvalMetricsRecord) string {
	if latest == nil {
		return ""
	}
	// We only have access to the single Latest() here; regression detection
	// requires a second data point. The ReindexChecker defers the two-record
	// comparison to the caller (cmd/eval) which holds the baseline snapshot.
	// When integrated via cmd/eval the caller passes pre-computed regression
	// info. When used standalone (e.g. API handler) we conservatively skip.
	//
	// To enable standalone regression detection, extend EvalMetricsProvider
	// with a Previous(ctx) method returning the second-most-recent record.
	return ""
}

// CheckWithBaseline is a convenience method that adds eval regression detection
// when the caller already holds both the current and baseline metric snapshots.
// This is the typical path used by cmd/eval.
func (c *ReindexChecker) CheckWithBaseline(
	ctx context.Context,
	currentNDCG5, baselineNDCG5 float64,
	currentNDCG10, baselineNDCG10 float64,
	currentMRR10, baselineMRR10 float64,
) (ReindexRecommendation, error) {
	rec, err := c.Check(ctx)
	if err != nil {
		return ReindexRecommendation{}, err
	}

	// Append regression reason if any metric drops by more than the threshold.
	if reason := evalRegressionReason(
		c.config.EvalRegressionThreshold,
		currentNDCG5, baselineNDCG5,
		currentNDCG10, baselineNDCG10,
		currentMRR10, baselineMRR10,
	); reason != "" {
		rec.Reasons = append(rec.Reasons, reason)
		rec.ShouldReindex = true
	}

	return rec, nil
}

// --- helpers ---

func sumCounts(m map[string]int) int {
	total := 0
	for _, n := range m {
		total += n
	}
	return total
}

func formatStalenessReason(elapsed time.Duration, thresholdDays int) string {
	days := int(elapsed.Hours() / 24)
	return fmt.Sprintf("index is %d days old (threshold: %d days)", days, thresholdDays)
}

func formatDocGrowthReason(delta, threshold int) string {
	return fmt.Sprintf("%d new documents since last reindex (threshold: %d)", delta, threshold)
}

// evalRegressionReason checks whether any eval metric dropped by more than
// threshold relative to its baseline. Returns "" when no regression is detected.
func evalRegressionReason(
	threshold float64,
	curNDCG5, baseNDCG5 float64,
	curNDCG10, baseNDCG10 float64,
	curMRR10, baseMRR10 float64,
) string {
	type check struct {
		name          string
		cur, baseline float64
	}
	checks := []check{
		{"ndcg5", curNDCG5, baseNDCG5},
		{"ndcg10", curNDCG10, baseNDCG10},
		{"mrr10", curMRR10, baseMRR10},
	}
	for _, ch := range checks {
		if ch.baseline == 0 {
			continue
		}
		drop := (ch.baseline - ch.cur) / ch.baseline
		if drop >= threshold {
			return fmt.Sprintf("eval regression detected: %s dropped %.1f%% (threshold: %.1f%%)",
				ch.name, drop*100, threshold*100)
		}
	}
	return ""
}
