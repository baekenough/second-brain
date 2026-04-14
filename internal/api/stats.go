package api

import (
	"log/slog"
	"net/http"
)

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
