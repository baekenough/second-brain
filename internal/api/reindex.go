package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/baekenough/second-brain/internal/store"
)

// ReindexStateRecorder is the subset of store.ReindexStateStore used by the
// reindex handler. Defined as an interface so tests can inject a stub.
type ReindexStateRecorder interface {
	Save(ctx context.Context, state store.ReindexState) error
}

// reindexRequest is the optional JSON request body for POST /api/v1/reindex.
type reindexRequest struct {
	Reason string `json:"reason"`
}

// reindexResponse is the JSON response body for POST /api/v1/reindex.
type reindexResponse struct {
	Status   string `json:"status"`
	DocCount int    `json:"doc_count"`
	Reason   string `json:"reason"`
}

// WithReindexState attaches a ReindexStateRecorder to the server so that the
// /api/v1/reindex route is registered. Returns s for method chaining.
//
// NOTE: WithReindexState must be called before the first call to Handler(),
// because the chi router is built exactly once via sync.Once.
func (s *Server) WithReindexState(rs ReindexStateRecorder) *Server {
	s.reindexState = rs
	return s
}

// reindexHandler handles POST /api/v1/reindex.
//
// It records a manual reindex trigger in the reindex_state table and returns
// a confirmation with the current document count and the reason.
//
// The handler does NOT perform any actual re-embedding. Re-embedding is a
// separate offline operation triggered by restarting the collector with the
// --reindex flag. This endpoint exists solely to record intent and provide
// the threshold check output to the caller.
//
// Request body (optional):
//
//	{"reason": "string"}   // defaults to "manual" when absent
//
// Response:
//
//	{"status": "reindex_recorded", "doc_count": N, "reason": "..."}
func (s *Server) reindexHandler(w http.ResponseWriter, r *http.Request) {
	if s.reindexState == nil {
		writeError(w, http.StatusServiceUnavailable, "reindex not configured")
		return
	}

	// Parse optional request body — ignore parse errors and use defaults.
	var req reindexRequest
	if r.ContentLength > 0 {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if req.Reason == "" {
		req.Reason = "manual"
	}

	// Get current document count.
	counts, err := s.docs.CountBySource(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	total := 0
	for _, c := range counts {
		total += c
	}

	// Record the reindex event.
	state := store.ReindexState{
		DocCountAtReindex: total,
		TriggerReason:     req.Reason,
	}
	if err := s.reindexState.Save(r.Context(), state); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, reindexResponse{
		Status:   "reindex_recorded",
		DocCount: total,
		Reason:   req.Reason,
	})
}
