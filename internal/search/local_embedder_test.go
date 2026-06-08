package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ollamaEmbedResponse builds an Ollama /api/embeddings response.
func ollamaEmbedResponse(vec []float32) map[string]interface{} {
	return map[string]interface{}{"embedding": vec}
}

// ---------------------------------------------------------------------------
// LocalEmbedder unit tests
// ---------------------------------------------------------------------------

// TestLocalEmbedder_Disabled verifies that a LocalEmbedder with an empty
// endpoint is disabled and returns (nil, nil) without making any HTTP request.
func TestLocalEmbedder_Disabled(t *testing.T) {
	t.Parallel()

	e := NewLocalEmbedder("", "bge-m3", 768)
	if e.Enabled() {
		t.Fatal("expected Enabled()==false when endpoint is empty")
	}
	if e.Dimension() != 768 {
		t.Fatalf("Dimension() = %d, want 768", e.Dimension())
	}

	vec, err := e.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Embed on disabled embedder returned error: %v", err)
	}
	if vec != nil {
		t.Fatalf("Embed on disabled embedder returned non-nil vector: %v", vec)
	}

	batch, err := e.EmbedBatch(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("EmbedBatch on disabled embedder returned error: %v", err)
	}
	if len(batch) != 2 {
		t.Fatalf("EmbedBatch on disabled embedder returned %d results, want 2", len(batch))
	}
	for i, v := range batch {
		if v != nil {
			t.Errorf("EmbedBatch[%d] on disabled embedder returned non-nil vector", i)
		}
	}
}

// TestLocalEmbedder_Embed_OK verifies a successful Ollama /api/embeddings response.
func TestLocalEmbedder_Embed_OK(t *testing.T) {
	t.Parallel()

	const wantDim = 768
	want := fakeVec(wantDim)
	want[0] = 0.42

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embeddings" {
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		// Verify request body fields.
		var body struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if body.Model != "bge-m3" {
			http.Error(w, "unexpected model: "+body.Model, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ollamaEmbedResponse(want))
	}))
	defer srv.Close()

	e := NewLocalEmbedder(srv.URL, "bge-m3", wantDim)
	if !e.Enabled() {
		t.Fatal("expected Enabled()==true when endpoint is set")
	}
	if e.Dimension() != wantDim {
		t.Fatalf("Dimension() = %d, want %d", e.Dimension(), wantDim)
	}

	got, err := e.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("Embed returned unexpected error: %v", err)
	}
	if len(got) != wantDim {
		t.Fatalf("Embed returned vector of dimension %d, want %d", len(got), wantDim)
	}
	if got[0] != want[0] {
		t.Errorf("Embed vector[0] = %f, want %f", got[0], want[0])
	}
}

// TestLocalEmbedder_Embed_404 verifies that a non-200 status causes an error.
func TestLocalEmbedder_Embed_404(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not loaded", http.StatusNotFound)
	}))
	defer srv.Close()

	e := NewLocalEmbedder(srv.URL, "bge-m3", 768)
	_, err := e.Embed(context.Background(), "test")
	if err == nil {
		t.Fatal("expected error on 404 response, got nil")
	}
}

// TestLocalEmbedder_EmbedBatch_OrderPreserved verifies that EmbedBatch returns
// vectors in the same order as the input texts (sequential sequential execution).
func TestLocalEmbedder_EmbedBatch_OrderPreserved(t *testing.T) {
	t.Parallel()

	const dim = 4
	// Distinct fingerprint per input index.
	sentinels := []float32{1.0, 2.0, 3.0}
	callIdx := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		vec := fakeVec(dim)
		vec[0] = sentinels[callIdx]
		callIdx++

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ollamaEmbedResponse(vec))
	}))
	defer srv.Close()

	e := NewLocalEmbedder(srv.URL, "bge-m3", dim)
	texts := []string{"first", "second", "third"}
	got, err := e.EmbedBatch(context.Background(), texts)
	if err != nil {
		t.Fatalf("EmbedBatch returned error: %v", err)
	}
	if len(got) != len(texts) {
		t.Fatalf("EmbedBatch returned %d vectors, want %d", len(got), len(texts))
	}
	for i, sentinel := range sentinels {
		if got[i][0] != sentinel {
			t.Errorf("EmbedBatch[%d][0] = %f, want %f (order not preserved)", i, got[i][0], sentinel)
		}
	}
}

// TestLocalEmbedder_EmbedBatch_CancelledContext verifies that a cancelled context
// causes EmbedBatch to return an error rather than silently completing.
func TestLocalEmbedder_EmbedBatch_CancelledContext(t *testing.T) {
	t.Parallel()

	// Server that blocks until the request context is cancelled.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		http.Error(w, "cancelled", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	e := NewLocalEmbedder(srv.URL, "bge-m3", 768)
	_, err := e.EmbedBatch(ctx, []string{"a", "b", "c"})
	if err == nil {
		t.Fatal("expected error when context is cancelled before first request, got nil")
	}
}

// TestLocalEmbedder_EmbedBatch_Dimension verifies that EmbedBatch returns
// vectors with the dimension reported by the Ollama server (not the config dim).
func TestLocalEmbedder_EmbedBatch_Dimension(t *testing.T) {
	t.Parallel()

	const serverDim = 512 // Ollama returns 512-d vectors
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ollamaEmbedResponse(fakeVec(serverDim)))
	}))
	defer srv.Close()

	e := NewLocalEmbedder(srv.URL, "some-model", 768) // config says 768, server returns 512
	got, err := e.EmbedBatch(context.Background(), []string{"text"})
	if err != nil {
		t.Fatalf("EmbedBatch returned error: %v", err)
	}
	if len(got[0]) != serverDim {
		t.Errorf("EmbedBatch vector dimension = %d, want %d (server dimension)", len(got[0]), serverDim)
	}
}
