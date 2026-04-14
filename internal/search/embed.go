package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/baekenough/second-brain/internal/auth"
)

// EmbedClient calls an OpenAI-compatible /v1/embeddings endpoint to produce
// vector representations of text. When apiURL is empty all methods are no-ops
// and return nil, enabling full-text-only operation.
//
// Token resolution at construction time:
//  1. apiKey non-empty  → static Bearer token
//  2. authFilePath non-empty → CliProxyAPI OAuth token (auto-refreshed with 5-min TTL)
//  3. both empty → no Authorization header sent
type EmbedClient struct {
	apiURL string
	model  string
	client *http.Client
	tokens auth.TokenSource // nil when no auth configured
}

// NewEmbedClient returns an EmbedClient. When apiURL is empty the client is
// disabled — Embed and EmbedBatch return nil results without error.
//
// Token priority: apiKey > authFilePath > no auth.
func NewEmbedClient(apiURL, apiKey, authFilePath, model string) *EmbedClient {
	return &EmbedClient{
		apiURL: apiURL,
		model:  model,
		client: &http.Client{Timeout: 30 * time.Second},
		tokens: auth.Resolve(apiKey, authFilePath),
	}
}

// Enabled reports whether an embedding API is configured.
func (c *EmbedClient) Enabled() bool { return c.apiURL != "" }

// Embed returns the embedding vector for text, or nil if the client is disabled.
func (c *EmbedClient) Embed(ctx context.Context, text string) ([]float32, error) {
	if !c.Enabled() {
		return nil, nil
	}

	payload := map[string]interface{}{
		"input": text,
		"model": c.model,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("embed marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.apiURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embed build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.setAuth(req); err != nil {
		return nil, err
	}

	res, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed request: %w", err)
	}
	defer res.Body.Close()

	b, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("embed read response: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed API status %d: %s", res.StatusCode, b)
	}

	var resp struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, fmt.Errorf("embed unmarshal: %w", err)
	}
	if len(resp.Data) == 0 || len(resp.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embed: empty embedding in response")
	}
	return resp.Data[0].Embedding, nil
}

// EmbedBatch generates embeddings for multiple texts in a single API call.
func (c *EmbedClient) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if !c.Enabled() {
		return make([][]float32, len(texts)), nil
	}

	payload := map[string]interface{}{
		"input": texts,
		"model": c.model,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("embed batch marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.apiURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.setAuth(req); err != nil {
		return nil, err
	}

	res, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed batch request: %w", err)
	}
	defer res.Body.Close()

	b, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed batch API status %d: %s", res.StatusCode, b)
	}

	var resp struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, err
	}

	out := make([][]float32, len(texts))
	for _, d := range resp.Data {
		if d.Index < len(out) {
			out[d.Index] = d.Embedding
		}
	}
	return out, nil
}

// setAuth attaches the Authorization header when a token source is configured.
func (c *EmbedClient) setAuth(req *http.Request) error {
	if c.tokens == nil {
		return nil
	}
	tok, err := c.tokens.Token()
	if err != nil {
		return fmt.Errorf("embed auth token: %w", err)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return nil
}
