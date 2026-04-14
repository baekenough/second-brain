package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// discordBotMetrics is the JSON shape for the discord_bot section of the
// baseline stats response. It is appended to the existing BaselineStats JSON
// as an additional top-level key so that existing callers are not affected.
type discordBotMetrics struct {
	TotalResponses  int64                  `json:"total_responses"`
	ZeroResultCount int64                  `json:"zero_result_count"`
	LatencyMS       discordLatencySnapshot `json:"latency_ms"`
}

type discordLatencySnapshot struct {
	P50 int64 `json:"p50"`
	P95 int64 `json:"p95"`
	P99 int64 `json:"p99"`
}

// baselineStatsHandler handles GET /api/v1/stats/baseline.
// Returns detailed baseline metrics: document counts with content-length
// percentiles per source type, chunk aggregates, extraction failure counts,
// the most recent collection timestamp per source type, and (when available)
// Discord bot response latency percentiles under the "discord_bot" key.
//
// The existing BaselineStats JSON schema is preserved: all original top-level
// keys are present as-is. The discord_bot key is added only when metrics are
// configured, so existing callers that do not use it remain unaffected.
func (s *Server) baselineStatsHandler(w http.ResponseWriter, r *http.Request) {
	stats, err := s.docs.QueryBaselineStats(r.Context())
	if err != nil {
		slog.Error("baseline stats: query failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	// When no Discord metrics are configured, write the store stats directly so
	// that the existing JSON schema is byte-for-byte compatible.
	if s.discordMetrics == nil {
		writeJSON(w, http.StatusOK, stats)
		return
	}

	// Merge the Discord metrics snapshot into the existing stats JSON by first
	// marshalling the store stats to a map, then adding the discord_bot key.
	// This preserves all existing top-level keys without a separate wrapper struct.
	raw, err := json.Marshal(stats)
	if err != nil {
		slog.Error("baseline stats: marshal failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	merged := make(map[string]json.RawMessage)
	if err := json.Unmarshal(raw, &merged); err != nil {
		// Fallback: write stats without Discord metrics.
		writeJSON(w, http.StatusOK, stats)
		return
	}

	snap := s.discordMetrics.Snapshot()
	botMetrics := discordBotMetrics{
		TotalResponses:  snap.TotalResponses,
		ZeroResultCount: snap.ZeroResultCount,
		LatencyMS: discordLatencySnapshot{
			P50: snap.LatencyMS.P50,
			P95: snap.LatencyMS.P95,
			P99: snap.LatencyMS.P99,
		},
	}
	botJSON, err := json.Marshal(botMetrics)
	if err == nil {
		merged["discord_bot"] = botJSON
	}

	writeJSON(w, http.StatusOK, merged)
}

// statsHandler handles GET /api/v1/stats.
// Returns document counts grouped by source_type (active only).
func (s *Server) statsHandler(w http.ResponseWriter, r *http.Request) {
	counts, err := s.docs.CountBySource(r.Context())
	if err != nil {
		slog.Error("stats: count failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	// Always include known sources even with zero count.
	known := []string{"filesystem", "slack", "github"}
	bySource := make(map[string]int, len(known))
	total := 0
	for _, k := range known {
		v := counts[k]
		bySource[k] = v
		total += v
	}
	// Include any unknown source types the store returned.
	for k, v := range counts {
		if _, ok := bySource[k]; !ok {
			bySource[k] = v
			total += v
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"by_source": bySource,
		"total":     total,
	})
}
