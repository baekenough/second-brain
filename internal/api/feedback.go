package api

import (
	"context"
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
	DocumentID *string        `json:"document_id,omitempty"`
	ChunkID    *int64         `json:"chunk_id,omitempty"`
	Source     string         `json:"source"`
	SessionID  *string        `json:"session_id,omitempty"`
	UserID     *string        `json:"user_id,omitempty"`
	Thumbs     int16          `json:"thumbs"`
	Comment    *string        `json:"comment,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// errFeedbackReferenceNotFound 는 document_id·chunk_id 가 가리키는 행이 없을 때
// (FK 위반 23503)의 응답 문구다. 어느 ID 인지는 제약 이름으로만 알 수 있어
// 둘을 묶어 말한다.
const errFeedbackReferenceNotFound = "document_id or chunk_id not found"

// FeedbackResponse is returned on successful feedback creation.
type FeedbackResponse struct {
	ID int64 `json:"id"`
}

// feedbackHandler handles POST /api/v1/feedback.
// Validates the request body and delegates persistence to FeedbackRecorder.
// Returns 201 Created with {"id": <id>} on success.
//
// 본문 상한·필드 검증(#286)은 DB 에 가기 전에 한다. 거부 응답과 로그에는
// 입력값을 넣지 않는다(필드 이름과 고정 사유만).
func (s *Server) feedbackHandler(w http.ResponseWriter, r *http.Request) {
	var req FeedbackRequest
	if err := decodeBoundedJSON(w, r, feedbackRequestMaxBytes, &req); err != nil {
		writeBoundedJSONError(w, err, feedbackRequestMaxBytes, "invalid request body")
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
	if err := validateFeedback(&f); err != nil {
		writeError(w, http.StatusBadRequest, searchInputMessage(err))
		return
	}

	id, err := s.feedback.Record(r.Context(), f)
	if err != nil {
		if isForeignKeyViolation(err) {
			writeError(w, http.StatusBadRequest, errFeedbackReferenceNotFound)
			return
		}
		slog.Error("feedback: record failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	writeJSON(w, http.StatusCreated, FeedbackResponse{ID: id})
}
