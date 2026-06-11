package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/baekenough/second-brain/internal/store"
)

const (
	recentDefaultLimit = 50
	recentMaxLimit     = 200
)

// RecentItemsQuerier is the store interface required by recentDocumentsHandler.
// It combines the paginated list query and the total-count query so the handler
// can report both the current page size and the true DB total in one response.
type RecentItemsQuerier interface {
	ListRecentByKind(ctx context.Context, kind store.RecentKind, limit int) ([]store.RecentItem, error)
	CountByKind(ctx context.Context, kind store.RecentKind) (int, error)
}

// recentDocumentsResponse is the JSON envelope returned by GET /api/v1/documents/recent.
//
// Fields:
//   - count — number of items in this response (≤ limit, ≤ 200).
//   - total — true DB count for this kind, independent of limit. Mobile clients
//     must use this field for "X items collected" display to avoid the cap bug.
type recentDocumentsResponse struct {
	Kind  string             `json:"kind"`
	Count int                `json:"count"`
	Total int                `json:"total"`
	Items []store.RecentItem `json:"items"`
}

// recentDocumentsHandler handles GET /api/v1/documents/recent.
//
// Query parameters:
//   - kind  — required; one of "sms", "call-recording", "voice-memo"
//   - limit — optional; default 50, max 200; invalid/negative values fall back to 50
func (s *Server) recentDocumentsHandler(w http.ResponseWriter, r *http.Request) {
	kindStr := r.URL.Query().Get("kind")

	var kind store.RecentKind
	switch kindStr {
	case string(store.RecentKindSMS):
		kind = store.RecentKindSMS
	case string(store.RecentKindCallRecording):
		kind = store.RecentKindCallRecording
	case string(store.RecentKindVoiceMemo):
		kind = store.RecentKindVoiceMemo
	default:
		writeError(w, http.StatusBadRequest,
			"kind must be one of: sms, call-recording, voice-memo")
		return
	}

	limit := queryInt(r, "limit", recentDefaultLimit)
	if limit > recentMaxLimit {
		limit = recentMaxLimit
	}

	querier, ok := s.docs.(RecentItemsQuerier)
	if !ok {
		slog.Error("recent documents: docs store does not implement RecentItemsQuerier")
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	items, err := querier.ListRecentByKind(r.Context(), kind, limit)
	if err != nil {
		slog.Error("recent documents: query failed", "kind", kindStr, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	total, err := querier.CountByKind(r.Context(), kind)
	if err != nil {
		slog.Error("recent documents: count failed", "kind", kindStr, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	writeJSON(w, http.StatusOK, recentDocumentsResponse{
		Kind:  kindStr,
		Count: len(items),
		Total: total,
		Items: items,
	})
}
