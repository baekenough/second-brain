package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/baekenough/second-brain/internal/search"
)

// ReindexRecommender is the subset of search.ReindexChecker used by the
// reindex check handler. Defined as an interface so tests can inject a stub.
type ReindexRecommender interface {
	Check(ctx context.Context) (search.ReindexRecommendation, error)
}

// WithReindexCheck attaches a ReindexRecommender to the server so that the
// GET /api/v1/reindex/check route is registered. Returns s for method chaining.
//
// NOTE: WithReindexCheck must be called before the first call to Handler(),
// because the chi router is built exactly once via sync.Once.
func (s *Server) WithReindexCheck(rc ReindexRecommender) *Server {
	s.reindexCheck = rc
	return s
}

// reindexCheckHandler handles GET /api/v1/reindex/check
//
// Returns the current reindex recommendation based on threshold checks
// (staleness, document growth, eval regression). This endpoint is read-only;
// it never triggers a reindex operation.
//
// Response (200 OK):
//
//	{"should_reindex": bool, "reasons": ["..."]}
func (s *Server) reindexCheckHandler(w http.ResponseWriter, r *http.Request) {
	if s.reindexCheck == nil {
		writeError(w, http.StatusServiceUnavailable, "reindex check not configured")
		return
	}

	rec, err := s.reindexCheck.Check(r.Context())
	if err != nil {
		slog.Error("reindex check: failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	// Ensure reasons is always a JSON array, never null.
	if rec.Reasons == nil {
		rec.Reasons = []string{}
	}

	writeJSON(w, http.StatusOK, rec)
}
