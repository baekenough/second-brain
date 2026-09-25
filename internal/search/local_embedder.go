package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/baekenough/second-brain/internal/httperr"
)

// localEmbedTimeout is the HTTP timeout for a single Ollama embedding request.
// Ollama may need time to load the model on the first call, so we use 60 s.
const localEmbedTimeout = 60 * time.Second

// LocalEmbedder sends embedding requests to an Ollama-compatible local server.
// The expected API contract is:
//
//	POST {endpoint}/api/embeddings
//	Body: {"model": "<model>", "prompt": "<text>"}
//	Response: {"embedding": [<float32>, ...]}
//
// When endpoint is empty the embedder is in the disabled state and all methods
// return nil results without error.
//
// LocalEmbedder satisfies the EmbeddingEngine interface.
type LocalEmbedder struct {
	endpoint string
	model    string
	dim      int // advisory dimension from config
	client   *http.Client
}

// NewLocalEmbedder returns a LocalEmbedder targeting the given Ollama endpoint.
// endpoint should be the base URL, e.g. "http://localhost:11434".
// model is the Ollama model name, e.g. "bge-m3".
// dim is the advisory vector dimension from config (used by Dimension()).
func NewLocalEmbedder(endpoint, model string, dim int) *LocalEmbedder {
	return &LocalEmbedder{
		endpoint: endpoint,
		model:    model,
		dim:      dim,
		client:   &http.Client{Timeout: localEmbedTimeout},
	}
}

// Enabled reports whether the local embedding endpoint is configured.
func (e *LocalEmbedder) Enabled() bool { return e.endpoint != "" }

// Dimension returns the advisory vector dimension for this embedder.
func (e *LocalEmbedder) Dimension() int { return e.dim }

// Embed returns the embedding vector for a single text.
// Returns nil, nil when the embedder is disabled.
func (e *LocalEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if !e.Enabled() {
		return nil, nil
	}
	return e.embed(ctx, text)
}

// EmbedBatch returns embedding vectors for multiple texts in input order.
// Requests are issued sequentially; the loop honours ctx cancellation between
// iterations so that callers can cancel large batches promptly.
//
// Returns a slice of nil vectors (not an error) when the embedder is disabled.
func (e *LocalEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if !e.Enabled() {
		return make([][]float32, len(texts)), nil
	}

	out := make([][]float32, len(texts))
	for i, t := range texts {
		// Respect context cancellation between requests.
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("local embed batch cancelled at index %d: %w", i, err)
		}
		vec, err := e.embed(ctx, t)
		if err != nil {
			return nil, fmt.Errorf("local embed batch [%d]: %w", i, err)
		}
		out[i] = vec
	}
	return out, nil
}

// localEmbedMaxResponseBytes 는 Ollama /api/embeddings 성공 응답 본문의
// 상한이다. 벡터 하나(8192차원 × 약 22B ≈ 180KB)를 넉넉히 덮는다.
const localEmbedMaxResponseBytes = 1 << 20

// embed sends a single POST /api/embeddings request to the Ollama server.
func (e *LocalEmbedder) embed(ctx context.Context, text string) ([]float32, error) {
	payload := struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}{
		Model:  e.model,
		Prompt: text,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("local embed marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		e.endpoint+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("local embed build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("local embed request: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		// 본문(Ollama 는 {"error":"…"} 에 요청 조각을 담을 수 있다)은 오류에
		// 싣지 않는다(#288 3항).
		return nil, httperr.ReadStatusError("local embed API status", res)
	}
	b, err := httperr.ReadBody(res.Body, localEmbedMaxResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("local embed read response: %w", err)
	}

	var resp struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, fmt.Errorf("local embed unmarshal: %w", err)
	}
	if len(resp.Embedding) == 0 {
		return nil, fmt.Errorf("local embed: empty embedding in response")
	}
	return resp.Embedding, nil
}
