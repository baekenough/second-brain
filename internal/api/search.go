package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/baekenough/second-brain/internal/curation"
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
	Sort               string             `json:"sort"`              // "relevance" (default) | "recent"
	UseHyDE            bool               `json:"use_hyde,omitempty"` // opt-in HyDE query expansion; default false
	Curated            bool               `json:"curated,omitempty"` // opt-in LLM curation and re-ranking; default false
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
		UseHyDE:            req.UseHyDE,
	}

	start := time.Now()
	results, err := s.search.Search(r.Context(), q)
	if err != nil {
		slog.Error("search: query failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	if req.Curated {
		curator := curation.New(s.llmClient)
		curatedResults, err := curator.Curate(r.Context(), req.Query, results)
		if err != nil {
			slog.Error("curation: failed", "error", err)
			writeError(w, http.StatusInternalServerError, "curation failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"results": curatedResults,
			"count":   len(curatedResults),
			"query":   req.Query,
			"curated": true,
			"took_ms": time.Since(start).Milliseconds(),
		})
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

// searchGetHandler handles GET /api/v1/search.
func (s *Server) searchGetHandler(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		writeError(w, http.StatusBadRequest, "q parameter is required")
		return
	}

	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}

	var srcType *model.SourceType
	if v := r.URL.Query().Get("source_type"); v != "" {
		st := model.SourceType(v)
		srcType = &st
	}

	curated := r.URL.Query().Get("curated") == "true"
	useHyDE := r.URL.Query().Get("use_hyde") == "true"

	q := model.SearchQuery{
		Query:      query,
		SourceType: srcType,
		Limit:      limit,
		UseHyDE:    useHyDE,
	}

	start := time.Now()
	results, err := s.search.Search(r.Context(), q)
	if err != nil {
		slog.Error("search: query failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	if curated {
		curator := curation.New(s.llmClient)
		curatedResults, err := curator.Curate(r.Context(), query, results)
		if err != nil {
			slog.Error("curation: failed", "error", err)
			writeError(w, http.StatusInternalServerError, "curation failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"results": curatedResults,
			"count":   len(curatedResults),
			"query":   query,
			"curated": true,
			"took_ms": time.Since(start).Milliseconds(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"results": results,
		"count":   len(results),
		"total":   len(results),
		"query":   query,
		"took_ms": time.Since(start).Milliseconds(),
	})
}
