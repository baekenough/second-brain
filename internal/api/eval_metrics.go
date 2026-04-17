package api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/baekenough/second-brain/internal/store"
)

// EvalMetricsLister is the subset of store.EvalMetricsStore used by the eval
// metrics handler. Defined as an interface so tests can inject a stub.
type EvalMetricsLister interface {
	List(ctx context.Context, limit int) ([]store.EvalMetricsRecord, error)
}

// WithEvalMetrics attaches an EvalMetricsLister to the server so that the
// GET /api/v1/eval/metrics route is registered. Returns s for method chaining.
//
// NOTE: WithEvalMetrics must be called before the first call to Handler(),
// because the chi router is built exactly once via sync.Once.
func (s *Server) WithEvalMetrics(em EvalMetricsLister) *Server {
	s.evalMetrics = em
	return s
}

// evalMetricsHandler handles GET /api/v1/eval/metrics?limit=N
//
// Returns a JSON array of eval run records ordered by run_at DESC.
// The optional `limit` query parameter controls how many records are returned
// (default: 20, max: 100). Values outside the allowed range are silently clamped.
func (s *Server) evalMetricsHandler(w http.ResponseWriter, r *http.Request) {
	if s.evalMetrics == nil {
		writeError(w, http.StatusServiceUnavailable, "eval metrics not configured")
		return
	}

	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}

	records, err := s.evalMetrics.List(r.Context(), limit)
	if err != nil {
		slog.Error("eval metrics: list failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	// Return an empty array instead of null when there are no records.
	if records == nil {
		records = []store.EvalMetricsRecord{}
	}

	writeJSON(w, http.StatusOK, records)
}
