package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
)

// embeddingResponse builds a minimal OpenAI-compatible /v1/embeddings response
// with the provided float32 vectors.
func embeddingResponse(vecs [][]float32) map[string]interface{} {
	data := make([]map[string]interface{}, len(vecs))
	for i, v := range vecs {
		data[i] = map[string]interface{}{"index": i, "embedding": v}
	}
	return map[string]interface{}{"object": "list", "data": data}
}

// fakeVec returns a float32 slice of the given length filled with 0.1.
func fakeVec(dim int) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = 0.1
	}
	return v
}

// ---------------------------------------------------------------------------
// EmbedClient unit tests
// ---------------------------------------------------------------------------

// TestEmbedClient_Disabled verifies that a client constructed with no key is
// in the disabled state and returns (nil, nil) without issuing any HTTP request.
func TestEmbedClient_Disabled(t *testing.T) {
	t.Parallel()

	// No apiKey, no authFilePath → disabled regardless of URL.
	c := NewEmbedClient("https://api.openai.com/v1", "", "", "text-embedding-3-small")

	if c.Enabled() {
		t.Fatal("expected Enabled()==false when both apiKey and authFilePath are empty")
	}

	vec, err := c.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Embed on disabled client returned error: %v", err)
	}
	if vec != nil {
		t.Fatalf("Embed on disabled client returned non-nil vector: %v", vec)
	}
}

// TestEmbedClient_200_SingleVector verifies that a 200 response with a valid
// embedding body is parsed correctly and the dimension matches expectations.
func TestEmbedClient_200_SingleVector(t *testing.T) {
	t.Parallel()

	const wantDim = 1536
	vec := fakeVec(wantDim)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(embeddingResponse([][]float32{vec}))
	}))
	defer srv.Close()

	c := NewEmbedClient(srv.URL, "sk-test", "", "text-embedding-3-small")

	if !c.Enabled() {
		t.Fatal("expected Enabled()==true when apiKey is set")
	}

	got, err := c.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("Embed returned unexpected error: %v", err)
	}
	if len(got) != wantDim {
		t.Fatalf("Embed returned vector of dimension %d, want %d", len(got), wantDim)
	}
	for i, v := range got {
		if v != vec[i] {
			t.Fatalf("Embed vector[%d] = %f, want %f", i, v, vec[i])
		}
	}
}

// TestEmbedClient_404_ReturnsError verifies that a 404 response (e.g. cliproxy
// endpoint that does not support /v1/embeddings) causes Embed to return an
// error with a descriptive message.
func TestEmbedClient_404_ReturnsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Not Found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewEmbedClient(srv.URL, "sk-test", "", "text-embedding-3-small")

	_, err := c.Embed(context.Background(), "test")
	if err == nil {
		t.Fatal("expected error on 404 response, got nil")
	}
}

// TestEmbedBatch_200_OrderPreserved verifies that EmbedBatch returns vectors in
// the same order as the input texts, even though the API may return them in
// any order via the index field.
func TestEmbedBatch_200_OrderPreserved(t *testing.T) {
	t.Parallel()

	const dim = 1536
	// Deliberately return vectors in reverse order via the index field to
	// exercise the reordering logic inside embedBatchOnce.
	texts := []string{"first", "second", "third"}
	vecs := [][]float32{fakeVec(dim), fakeVec(dim), fakeVec(dim)}
	// Give each vec a distinct fingerprint so we can identify them.
	vecs[0][0] = 1.0
	vecs[1][0] = 2.0
	vecs[2][0] = 3.0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return in reverse order: index 2, 1, 0.
		data := []map[string]interface{}{
			{"index": 2, "embedding": vecs[2]},
			{"index": 1, "embedding": vecs[1]},
			{"index": 0, "embedding": vecs[0]},
		}
		resp := map[string]interface{}{"object": "list", "data": data}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewEmbedClient(srv.URL, "sk-test", "", "text-embedding-3-small")

	got, err := c.EmbedBatch(context.Background(), texts)
	if err != nil {
		t.Fatalf("EmbedBatch returned error: %v", err)
	}
	if len(got) != len(texts) {
		t.Fatalf("EmbedBatch returned %d vectors, want %d", len(got), len(texts))
	}
	for i, want := range vecs {
		if got[i][0] != want[0] {
			t.Errorf("EmbedBatch[%d][0] = %f, want %f", i, got[i][0], want[0])
		}
	}
}

// ---------------------------------------------------------------------------
// Search.Service graceful-degradation test
//
// Verifies that when the embedding endpoint returns a non-200 (e.g. 404),
// Search.Service.Search falls back to FTS and returns results without panicking.
// ---------------------------------------------------------------------------

// fakeDocSearcher is a minimal DocumentSearcher stub that always returns a
// fixed result set, simulating successful FTS retrieval.
type fakeDocSearcher struct {
	results []*model.SearchResult
}

func (f *fakeDocSearcher) Search(_ context.Context, _ model.SearchQuery) ([]*model.SearchResult, error) {
	return f.results, nil
}

// TestSearchService_EmbedFails_FallbackFTS confirms that when the embed
// endpoint returns a 404 (e.g. cliproxy), the search service:
//   - does NOT panic
//   - returns FTS results as a non-fatal degradation
func TestSearchService_EmbedFails_FallbackFTS(t *testing.T) {
	t.Parallel()

	// Embedding endpoint always returns 404 (simulates cliproxy behaviour).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Not Found", http.StatusNotFound)
	}))
	defer srv.Close()

	embedClient := NewEmbedClient(srv.URL, "sk-test", "", "text-embedding-3-small")

	wantResults := []*model.SearchResult{
		{
			Document: model.Document{Title: "FTS result"},
			Score:    1.0,
			MatchType: "fulltext",
		},
	}
	docSearcher := &fakeDocSearcher{results: wantResults}

	svc := NewService(docSearcher, embedClient)

	got, err := svc.Search(context.Background(), model.SearchQuery{Query: "test query", Limit: 10})
	if err != nil {
		t.Fatalf("Search returned unexpected error: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected FTS fallback results, got empty slice")
	}
	if got[0].Title != wantResults[0].Title {
		t.Errorf("got result title %q, want %q", got[0].Title, wantResults[0].Title)
	}
}
