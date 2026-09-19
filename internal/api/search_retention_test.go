package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
)

// ---------------------------------------------------------------------------
// include_retention wiring for POST/GET /api/v1/search.
//
// The actual exclusion default and the retention="low" score penalty live in
// search.Service.Search (internal/search/retention_test.go); these tests only
// pin the API-layer decode/wire-through — that a request body's
// "include_retention": true (POST) or ?include_retention=true (GET) reaches
// model.SearchQuery.IncludeRetention, and therefore reaches the service.
// ---------------------------------------------------------------------------

// searchQueryRecordingSearcher is a documentSearcher fake scoped to this file:
// it records every model.SearchQuery.IncludeRetention it received.
type searchQueryRecordingSearcher struct {
	gotQuery model.SearchQuery
}

func (s *searchQueryRecordingSearcher) Search(_ context.Context, q model.SearchQuery) ([]*model.SearchResult, error) {
	s.gotQuery = q
	return nil, nil
}

func newSearchRetentionTestServer(searcher search.DocumentSearcher) *Server {
	svc := search.NewService(searcher, askDisabledEmbedder{})
	return NewServer(nil, svc, nil, nil, nil, "", "")
}

// TestSearchHandler_IncludeRetention_Default verifies that a POST request
// with no include_retention field still gets the default exclusion — i.e.
// the wiring does not accidentally default to true.
func TestSearchHandler_IncludeRetention_Default(t *testing.T) {
	t.Parallel()

	fake := &searchQueryRecordingSearcher{}
	srv := newSearchRetentionTestServer(fake)

	body, _ := json.Marshal(map[string]any{"query": "뉴스레터"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/search", bytes.NewReader(body))
	w := httptest.NewRecorder()

	srv.searchHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if fake.gotQuery.IncludeRetention {
		t.Errorf("IncludeRetention = true, want false when the request omits include_retention")
	}
	if !containsRetentionValue(fake.gotQuery.ExcludeRetention, model.RetentionDisposable) {
		t.Errorf("ExcludeRetention = %v, want it to contain %q by default",
			fake.gotQuery.ExcludeRetention, model.RetentionDisposable)
	}
}

// TestSearchHandler_IncludeRetention_ExplicitTrue verifies the POST body field
// disables the default exclusion.
func TestSearchHandler_IncludeRetention_ExplicitTrue(t *testing.T) {
	t.Parallel()

	fake := &searchQueryRecordingSearcher{}
	srv := newSearchRetentionTestServer(fake)

	body, _ := json.Marshal(map[string]any{"query": "뉴스레터", "include_retention": true})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/search", bytes.NewReader(body))
	w := httptest.NewRecorder()

	srv.searchHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if !fake.gotQuery.IncludeRetention {
		t.Errorf("IncludeRetention = false, want true when the request sets include_retention=true")
	}
	if containsRetentionValue(fake.gotQuery.ExcludeRetention, model.RetentionDisposable) {
		t.Errorf("ExcludeRetention = %v, want empty when include_retention=true",
			fake.gotQuery.ExcludeRetention)
	}
}

// TestSearchGetHandler_IncludeRetention_QueryParam covers the GET variant's
// ?include_retention=true query-string parsing.
func TestSearchGetHandler_IncludeRetention_QueryParam(t *testing.T) {
	t.Parallel()

	fake := &searchQueryRecordingSearcher{}
	srv := newSearchRetentionTestServer(fake)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=뉴스레터&include_retention=true", nil)
	w := httptest.NewRecorder()

	srv.searchGetHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if !fake.gotQuery.IncludeRetention {
		t.Errorf("IncludeRetention = false, want true when ?include_retention=true is set")
	}
}

// TestSearchGetHandler_IncludeRetention_DefaultFalse pins the GET path's
// default the same way the POST test above pins the JSON body's default.
func TestSearchGetHandler_IncludeRetention_DefaultFalse(t *testing.T) {
	t.Parallel()

	fake := &searchQueryRecordingSearcher{}
	srv := newSearchRetentionTestServer(fake)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=뉴스레터", nil)
	w := httptest.NewRecorder()

	srv.searchGetHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if fake.gotQuery.IncludeRetention {
		t.Errorf("IncludeRetention = true, want false when include_retention is omitted from the query string")
	}
}

func containsRetentionValue(list []string, want string) bool {
	for _, r := range list {
		if r == want {
			return true
		}
	}
	return false
}
