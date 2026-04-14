package api

import (
	"log/slog"
	"net/http"
)

// baselineStatsHandler handles GET /api/v1/stats/baseline.
// Returns detailed baseline metrics: document counts with content-length
// percentiles per source type, chunk aggregates, extraction failure counts,
// and the most recent collection timestamp per source type.
func (s *Server) baselineStatsHandler(w http.ResponseWriter, r *http.Request) {
	stats, err := s.docs.QueryBaselineStats(r.Context())
	if err != nil {
		slog.Error("baseline stats: query failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, stats)
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
