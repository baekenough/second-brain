package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/baekenough/second-brain/internal/model"
)

// searchRequest is the JSON body for POST /api/v1/search.
// It mirrors model.SearchQuery but uses snake_case JSON tags explicitly so that
// include_deleted is properly decoded from the request body.
type searchRequest struct {
	Query              string             `json:"query"`
	SourceType         *model.SourceType  `json:"source_type"`
	ExcludeSourceTypes []model.SourceType `json:"exclude_source_types"` // source types to exclude
	Limit              int                `json:"limit"`
	IncludeDeleted     bool               `json:"include_deleted"`
	Sort               string             `json:"sort"` // "relevance" (default) | "recent"
}

// searchHandler handles POST /api/v1/search.
func (s *Server) searchHandler(w http.ResponseWriter, r *http.Request) {
	var req searchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Query == "" {
		writeError(w, http.StatusBadRequest, "query field is required")
		return
	}

	q := model.SearchQuery{
		Query:              req.Query,
		SourceType:         req.SourceType,
		ExcludeSourceTypes: req.ExcludeSourceTypes,
		Limit:              req.Limit,
		IncludeDeleted:     req.IncludeDeleted,
		Sort:               req.Sort,
	}

	start := time.Now()
	results, err := s.search.Search(r.Context(), q)
	if err != nil {
		slog.Error("search: query failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"results": results,
		"count":   len(results),
		"total":   len(results),
		"query":   req.Query,
		"took_ms": time.Since(start).Milliseconds(),
	})
}
