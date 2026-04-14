package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// tokenSource resolves a Bearer token for each request.
// Implementations may cache tokens with TTL-based refresh.
type tokenSource interface {
	token() (string, error)
}

// staticToken always returns the same pre-configured token.
type staticToken struct{ t string }

func (s *staticToken) token() (string, error) { return s.t, nil }

// cliProxyToken reads an OAuth access_token from a CliProxyAPI JSON auth file.
// The file is re-read at most once per cacheTTL to pick up token refreshes
// performed by CliProxyAPI in the background.
type cliProxyToken struct {
	path     string
	cacheTTL time.Duration

	mu        sync.Mutex
	cached    string
	expiresAt time.Time
}

func newCliProxyToken(path string) *cliProxyToken {
	return &cliProxyToken{
		path:     path,
		cacheTTL: 5 * time.Minute,
	}
}

func (c *cliProxyToken) token() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cached != "" && time.Now().Before(c.expiresAt) {
		return c.cached, nil
	}

	data, err := os.ReadFile(c.path)
	if err != nil {
		return "", fmt.Errorf("cliproxy auth file: %w", err)
	}

	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", fmt.Errorf("cliproxy auth file parse: %w", err)
	}
	if payload.AccessToken == "" {
		return "", fmt.Errorf("cliproxy auth file: access_token is empty")
	}

	c.cached = payload.AccessToken
	c.expiresAt = time.Now().Add(c.cacheTTL)
	return c.cached, nil
}

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
	tokens tokenSource // nil when no auth configured
}

// NewEmbedClient returns an EmbedClient. When apiURL is empty the client is
// disabled — Embed and EmbedBatch return nil results without error.
//
// Token priority: apiKey > authFilePath > no auth.
func NewEmbedClient(apiURL, apiKey, authFilePath, model string) *EmbedClient {
	var ts tokenSource
	switch {
	case apiKey != "":
		ts = &staticToken{t: apiKey}
	case authFilePath != "":
		ts = newCliProxyToken(authFilePath)
	}

	return &EmbedClient{
		apiURL: apiURL,
		model:  model,
		client: &http.Client{Timeout: 30 * time.Second},
		tokens: ts,
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
	tok, err := c.tokens.token()
	if err != nil {
		return fmt.Errorf("embed auth token: %w", err)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return nil
}
