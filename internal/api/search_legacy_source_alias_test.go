package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
)

// ---------------------------------------------------------------------------
// Legacy call-log/call-transcript source_type alias normalization.
//
// Migration 033 (2026-09-19) rewrote every call-log/call-transcript document
// to source_type='call' (model.SourceCall's doc comment). A caller — e.g.
// hermes, which predates the migration — that still filters
// POST/GET /api/v1/search by one of the old values must still match those
// (now-renamed) documents rather than silently receiving zero results with no
// error. See model.NormalizeSourceType's doc comment for the full bug.
//
// These tests only pin the API-layer wiring: that a request naming the legacy
// value reaches the searcher with an effective include set of {call}, exactly
// like search_retention_test.go pins include_retention's wiring. The actual
// normalization is model.SearchQuery.IncludeSourceTypes — see
// internal/model/search_query_source_types_test.go for that unit test.
// ---------------------------------------------------------------------------

func TestSearchHandler_LegacyCallSourceType_NormalizedToCall(t *testing.T) {
	t.Parallel()

	legacy := model.SourceCallTranscript
	fake := &searchQueryRecordingSearcher{}
	srv := newSearchRetentionTestServer(fake)

	body, _ := json.Marshal(map[string]any{"query": "통화", "source_type": string(legacy)})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/search", bytes.NewReader(body))
	w := httptest.NewRecorder()

	srv.searchHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	got := fake.gotQuery.IncludeSourceTypes()
	if len(got) != 1 || got[0] != model.SourceCall {
		t.Errorf("IncludeSourceTypes() = %v, want [call]; source_type=%q must normalize to call, not silently match nothing",
			got, legacy)
	}
}

func TestSearchGetHandler_LegacyCallSourceType_NormalizedToCall(t *testing.T) {
	t.Parallel()

	fake := &searchQueryRecordingSearcher{}
	srv := newSearchRetentionTestServer(fake)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=통화&source_type=call-log", nil)
	w := httptest.NewRecorder()

	srv.searchGetHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	got := fake.gotQuery.IncludeSourceTypes()
	if len(got) != 1 || got[0] != model.SourceCall {
		t.Errorf("IncludeSourceTypes() = %v, want [call]; ?source_type=call-log must normalize to call", got)
	}
}
