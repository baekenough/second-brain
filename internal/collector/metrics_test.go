package collector_test

import (
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/collector"
)

// TestMetrics_Record verifies that recording 100 responses is reflected in the
// snapshot's TotalResponses counter.
func TestMetrics_Record(t *testing.T) {
	t.Parallel()

	m := &collector.DiscordMetrics{}
	for i := 0; i < 100; i++ {
		m.Record(time.Duration(i+1)*time.Millisecond, false)
	}

	snap := m.Snapshot()
	if snap.TotalResponses != 100 {
		t.Fatalf("TotalResponses: want 100, got %d", snap.TotalResponses)
	}
	if snap.ZeroResultCount != 0 {
		t.Fatalf("ZeroResultCount: want 0, got %d", snap.ZeroResultCount)
	}
}

// TestMetrics_ZeroResultCount verifies that exactly 30% of zero-result records
// are accumulated when 30 out of 100 calls pass zeroResults=true.
func TestMetrics_ZeroResultCount(t *testing.T) {
	t.Parallel()

	m := &collector.DiscordMetrics{}
	for i := 0; i < 100; i++ {
		zeroResults := i < 30 // first 30 are zero-result
		m.Record(50*time.Millisecond, zeroResults)
	}

	snap := m.Snapshot()
	if snap.TotalResponses != 100 {
		t.Fatalf("TotalResponses: want 100, got %d", snap.TotalResponses)
	}
	if snap.ZeroResultCount != 30 {
		t.Fatalf("ZeroResultCount: want 30, got %d", snap.ZeroResultCount)
	}
}

// TestMetrics_LatencyPercentiles records 10 values (10ms, 20ms, …, 100ms) and
// verifies that p50 ≈ 50ms and p95 ≈ 100ms using the nearest-rank method.
func TestMetrics_LatencyPercentiles(t *testing.T) {
	t.Parallel()

	m := &collector.DiscordMetrics{}
	for i := 1; i <= 10; i++ {
		m.Record(time.Duration(i*10)*time.Millisecond, false)
	}

	snap := m.Snapshot()

	// With 10 sorted values [10, 20, 30, 40, 50, 60, 70, 80, 90, 100]:
	// percentileInt64(sorted, 0.50): rank = int(0.50*10) = 5 → sorted[5] = 60
	// percentileInt64(sorted, 0.95): rank = int(0.95*10) = 9 → sorted[9] = 100
	// percentileInt64(sorted, 0.99): rank = int(0.99*10) = 9 → sorted[9] = 100
	if snap.LatencyMS.P50 != 60 {
		t.Errorf("P50: want 60, got %d", snap.LatencyMS.P50)
	}
	if snap.LatencyMS.P95 != 100 {
		t.Errorf("P95: want 100, got %d", snap.LatencyMS.P95)
	}
	if snap.LatencyMS.P99 != 100 {
		t.Errorf("P99: want 100, got %d", snap.LatencyMS.P99)
	}
}

// TestMetrics_RollingWindow verifies that recording 150 values only retains the
// most recent 100 entries and that totals are still accurate (counters are
// unbounded; only the latency window is capped).
func TestMetrics_RollingWindow(t *testing.T) {
	t.Parallel()

	m := &collector.DiscordMetrics{}
	// Record 150 values. The first 50 (1ms–50ms) will be evicted.
	// The last 100 (51ms–150ms) must be retained.
	for i := 1; i <= 150; i++ {
		m.Record(time.Duration(i)*time.Millisecond, false)
	}

	snap := m.Snapshot()

	// TotalResponses counter is unbounded — must be 150.
	if snap.TotalResponses != 150 {
		t.Fatalf("TotalResponses: want 150, got %d", snap.TotalResponses)
	}

	// The rolling window contains only the last 100 values: [51ms … 150ms].
	// Minimum of the window = 51ms → p50 should reflect values in [51..150].
	// p50 using nearest-rank with n=100: rank=50 → sorted[50] = 101ms.
	if snap.LatencyMS.P50 != 101 {
		t.Errorf("P50 after rolling window eviction: want 101, got %d", snap.LatencyMS.P50)
	}
}

// TestMetrics_EmptySnapshot verifies that a zero-value DiscordMetrics returns
// safe zero values from Snapshot (no panic, no garbage).
func TestMetrics_EmptySnapshot(t *testing.T) {
	t.Parallel()

	m := &collector.DiscordMetrics{}
	snap := m.Snapshot()

	if snap.TotalResponses != 0 {
		t.Errorf("TotalResponses: want 0, got %d", snap.TotalResponses)
	}
	if snap.ZeroResultCount != 0 {
		t.Errorf("ZeroResultCount: want 0, got %d", snap.ZeroResultCount)
	}
	if snap.LatencyMS.P50 != 0 || snap.LatencyMS.P95 != 0 || snap.LatencyMS.P99 != 0 {
		t.Errorf("latency percentiles: want all 0, got p50=%d p95=%d p99=%d",
			snap.LatencyMS.P50, snap.LatencyMS.P95, snap.LatencyMS.P99)
	}
}
