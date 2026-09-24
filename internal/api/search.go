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

// The insight-exclusion default (spec §3.2 echo-chamber guard 4, §6.5) used to
// be applied here, in these two handlers. It now lives in
// search.Service.Search, because three other callers of the same service —
// the Discord RAG gateway, the GraphQL search resolver and the MCP search tool
// — went through no handler at all and so got no exclusion. The call sites
// here are removed rather than left as no-ops: keeping them would imply the
// handler is where the policy lives, and the next endpoint added would be
// written to match.

// searchRequest is the JSON body for POST /api/v1/search.
// It mirrors model.SearchQuery but uses snake_case JSON tags explicitly so that
// include_deleted is properly decoded from the request body.
type searchRequest struct {
	Query              string             `json:"query"`
	SourceType         *model.SourceType  `json:"source_type"`
	ExcludeSourceTypes []model.SourceType `json:"exclude_source_types"` // source types to exclude
	Limit              int                `json:"limit"`
	IncludeDeleted     bool               `json:"include_deleted"`
	Sort               string             `json:"sort"`                 // "relevance" (default) | "recent"
	UseHyDE            bool               `json:"use_hyde,omitempty"`   // opt-in HyDE query expansion; default false
	UseRerank          bool               `json:"use_rerank,omitempty"` // opt-in cross-encoder reranking; default false
	Curated            bool               `json:"curated,omitempty"`    // opt-in LLM curation and re-ranking; default false
	// IncludeRetention, when true, disables the default retention=disposable
	// exclusion that search.Service.Search applies to every request (see
	// search.applyRetentionExclusionDefault). Default false.
	IncludeRetention bool `json:"include_retention,omitempty"`
}

// stripEvidenceForREST returns a shallow copy of results with Evidence set
// to nil on each copy. model.SearchResult.Evidence carries chunk-lane
// provenance (chunk ID/index/lane/score, #267) added for /ask's excerpt
// selection (internal/api/ask_context.go) only — it was never meant to be a
// public field, but its "evidence,omitempty" JSON tag means it silently
// started appearing in REST /api/v1/search responses once chunk-lane fusion
// began populating it. A shallow copy is used (rather than clearing
// Evidence on the original *model.SearchResult) so this never mutates the
// slice the search service returned, which the caller may still hold a
// reference to (e.g. a curated-results branch derives from the same slice).
func stripEvidenceForREST(results []*model.SearchResult) []*model.SearchResult {
	out := make([]*model.SearchResult, len(results))
	for i, r := range results {
		if r == nil {
			continue
		}
		cp := *r
		cp.Evidence = nil
		out[i] = &cp
	}
	return out
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
		UseRerank:          req.UseRerank,
		IncludeRetention:   req.IncludeRetention,
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
		"results": stripEvidenceForREST(results),
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
	useRerank := r.URL.Query().Get("use_rerank") == "true"
	includeRetention := r.URL.Query().Get("include_retention") == "true"

	q := model.SearchQuery{
		Query:            query,
		SourceType:       srcType,
		Limit:            limit,
		UseHyDE:          useHyDE,
		UseRerank:        useRerank,
		IncludeRetention: includeRetention,
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
		"results": stripEvidenceForREST(results),
		"count":   len(results),
		"total":   len(results),
		"query":   query,
		"took_ms": time.Since(start).Milliseconds(),
	})
}
