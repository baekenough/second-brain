package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPReranker_Disabled(t *testing.T) {
	r := NewHTTPReranker("", "", "", 0)

	if r.Enabled() {
		t.Fatal("expected Enabled() == false when apiURL is empty")
	}

	docs := []string{"doc a", "doc b", "doc c"}
	results, err := r.Rerank(context.Background(), "query", docs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != len(docs) {
		t.Fatalf("expected %d results, got %d", len(docs), len(results))
	}
	// Original order must be preserved: index 0, 1, 2.
	for i, res := range results {
		if res.Index != i {
			t.Errorf("result[%d].Index = %d, want %d", i, res.Index, i)
		}
	}
	// Scores must be descending.
	for i := 1; i < len(results); i++ {
		if results[i].Score >= results[i-1].Score {
			t.Errorf("scores not descending at position %d: %f >= %f",
				i, results[i].Score, results[i-1].Score)
		}
	}
}

func TestHTTPReranker_Rerank(t *testing.T) {
	// Mock Jina-compatible server that returns a fixed reranked response.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rerank" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		resp := map[string]interface{}{
			"results": []map[string]interface{}{
				{"index": 2, "relevance_score": 0.95},
				{"index": 0, "relevance_score": 0.72},
				{"index": 1, "relevance_score": 0.41},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	reranker := NewHTTPReranker(srv.URL, "test-key", "jina-reranker-v2-base-multilingual", 3)

	if !reranker.Enabled() {
		t.Fatal("expected Enabled() == true")
	}

	docs := []string{"first doc", "second doc", "third doc"}
	results, err := reranker.Rerank(context.Background(), "test query", docs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	// Verify index mapping matches mock response.
	wantIndexes := []int{2, 0, 1}
	wantScores := []float64{0.95, 0.72, 0.41}
	for i, res := range results {
		if res.Index != wantIndexes[i] {
			t.Errorf("result[%d].Index = %d, want %d", i, res.Index, wantIndexes[i])
		}
		if res.Score != wantScores[i] {
			t.Errorf("result[%d].Score = %f, want %f", i, res.Score, wantScores[i])
		}
	}
}

func TestHTTPReranker_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	reranker := NewHTTPReranker(srv.URL, "any-key", "model", 5)

	_, err := reranker.Rerank(context.Background(), "query", []string{"doc"})
	if err == nil {
		t.Fatal("expected error from 500 response, got nil")
	}
}
