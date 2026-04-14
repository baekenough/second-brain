package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/baekenough/second-brain/internal/store"
)

// FeedbackRecorder is the subset of store.FeedbackStore used by the feedback handler.
// Defined as an interface so tests can inject a stub without a real database.
type FeedbackRecorder interface {
	Record(ctx context.Context, f store.Feedback) (int64, error)
}

// FeedbackRequest is the JSON body accepted by POST /api/v1/feedback.
type FeedbackRequest struct {
	Query      *string        `json:"query,omitempty"`
	DocumentID *int64         `json:"document_id,omitempty"`
	ChunkID    *int64         `json:"chunk_id,omitempty"`
	Source     string         `json:"source"`
	SessionID  *string        `json:"session_id,omitempty"`
	UserID     *string        `json:"user_id,omitempty"`
	Thumbs     int16          `json:"thumbs"`
	Comment    *string        `json:"comment,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// FeedbackResponse is returned on successful feedback creation.
type FeedbackResponse struct {
	ID int64 `json:"id"`
}

// feedbackHandler handles POST /api/v1/feedback.
// Validates the request body and delegates persistence to FeedbackRecorder.
// Returns 201 Created with {"id": <id>} on success.
func (s *Server) feedbackHandler(w http.ResponseWriter, r *http.Request) {
	var req FeedbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Source == "" {
		writeError(w, http.StatusBadRequest, "source is required")
		return
	}
	if req.Thumbs < -1 || req.Thumbs > 1 {
		writeError(w, http.StatusBadRequest, "thumbs must be -1, 0, or 1")
		return
	}

	f := store.Feedback{
		Query:      req.Query,
		DocumentID: req.DocumentID,
		ChunkID:    req.ChunkID,
		Source:     req.Source,
		SessionID:  req.SessionID,
		UserID:     req.UserID,
		Thumbs:     req.Thumbs,
		Comment:    req.Comment,
		Metadata:   req.Metadata,
	}
	if f.Metadata == nil {
		f.Metadata = map[string]any{}
	}

	id, err := s.feedback.Record(r.Context(), f)
	if err != nil {
		slog.Error("feedback: record failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	writeJSON(w, http.StatusCreated, FeedbackResponse{ID: id})
}
