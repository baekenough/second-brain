// Package collector — metrics.go tracks Discord bot response latency and
// result-quality signals in a lightweight in-memory store.
//
// All operations are safe for concurrent use. No external dependencies: only
// stdlib sync/atomic, sync, and sort are used.
package collector

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const discordMetricsWindowSize = 100

// DiscordMetrics accumulates Discord bot response metrics in memory.
// It maintains a rolling window of the last 100 total-latency samples and
// atomic counters for total responses and zero-search-result events.
//
// Zero value is usable — all fields are initialised lazily or default to zero.
type DiscordMetrics struct {
	// TotalResponses is the number of buildReply completions (success or error).
	TotalResponses atomic.Int64

	// ZeroResultCount is the number of completions where the search returned
	// no results (an important quality signal).
	ZeroResultCount atomic.Int64

	mu              sync.RWMutex
	recentLatencies []int64 // rolling window, capped at discordMetricsWindowSize
}

// DiscordMetricsSnapshot is a point-in-time read of DiscordMetrics.
// It is safe to serialise to JSON directly.
type DiscordMetricsSnapshot struct {
	TotalResponses  int64            `json:"total_responses"`
	ZeroResultCount int64            `json:"zero_result_count"`
	LatencyMS       latencyPercentiles `json:"latency_ms"`
}

type latencyPercentiles struct {
	P50 int64 `json:"p50"`
	P95 int64 `json:"p95"`
	P99 int64 `json:"p99"`
}

// Record adds a single observation to the metrics store.
// total is the end-to-end latency of the buildReply call.
// zeroResults should be true when the knowledge-base search returned no hits.
func (m *DiscordMetrics) Record(total time.Duration, zeroResults bool) {
	m.TotalResponses.Add(1)
	if zeroResults {
		m.ZeroResultCount.Add(1)
	}

	ms := total.Milliseconds()
	m.mu.Lock()
	m.recentLatencies = append(m.recentLatencies, ms)
	if len(m.recentLatencies) > discordMetricsWindowSize {
		// Drop the oldest entry — O(n) copy but n ≤ 100, negligible cost.
		m.recentLatencies = m.recentLatencies[1:]
	}
	m.mu.Unlock()
}

// Snapshot returns a consistent point-in-time view of the current metrics.
// The latency percentiles are computed from the rolling window.
func (m *DiscordMetrics) Snapshot() DiscordMetricsSnapshot {
	m.mu.RLock()
	latencies := make([]int64, len(m.recentLatencies))
	copy(latencies, m.recentLatencies)
	m.mu.RUnlock()

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	return DiscordMetricsSnapshot{
		TotalResponses:  m.TotalResponses.Load(),
		ZeroResultCount: m.ZeroResultCount.Load(),
		LatencyMS: latencyPercentiles{
			P50: percentileInt64(latencies, 0.50),
			P95: percentileInt64(latencies, 0.95),
			P99: percentileInt64(latencies, 0.99),
		},
	}
}

// percentileInt64 returns the p-th percentile of a sorted slice of int64 values.
// p must be in [0, 1]. Returns 0 for an empty slice.
func percentileInt64(sorted []int64, p float64) int64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	// Nearest-rank method: rank = ceil(p * n), 1-indexed.
	rank := int(p * float64(n))
	if rank >= n {
		rank = n - 1
	}
	return sorted[rank]
}
