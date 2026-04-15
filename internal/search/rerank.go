package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Reranker scores query-document pairs and returns results sorted by relevance.
type Reranker interface {
	Enabled() bool
	Rerank(ctx context.Context, query string, docs []string) ([]RerankResult, error)
}

// RerankResult holds the reranked position and score for a document.
type RerankResult struct {
	Index int     `json:"index"`
	Score float64 `json:"relevance_score"`
}

// HTTPReranker calls a Jina-compatible /rerank endpoint.
// When apiURL is empty, it acts as a no-op and returns the original order.
type HTTPReranker struct {
	apiURL string
	apiKey string
	model  string
	topN   int
	client *http.Client
}

// NewHTTPReranker creates a reranker backed by the given endpoint.
// Pass empty apiURL to get a disabled (no-op) reranker.
func NewHTTPReranker(apiURL, apiKey, model string, topN int) *HTTPReranker {
	if topN <= 0 {
		topN = 10
	}
	return &HTTPReranker{
		apiURL: apiURL,
		apiKey: apiKey,
		model:  model,
		topN:   topN,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// Enabled reports whether a rerank API endpoint is configured.
func (r *HTTPReranker) Enabled() bool { return r.apiURL != "" }

// Rerank scores each document against the query and returns results ordered by
// descending relevance score. When the reranker is disabled (empty apiURL) it
// returns the original order with synthetic scores so callers need not branch.
func (r *HTTPReranker) Rerank(ctx context.Context, query string, docs []string) ([]RerankResult, error) {
	if !r.Enabled() {
		// No-op: return original order with descending synthetic scores.
		out := make([]RerankResult, len(docs))
		for i := range docs {
			out[i] = RerankResult{
				Index: i,
				Score: float64(len(docs)-i) / float64(len(docs)),
			}
		}
		return out, nil
	}

	type rerankRequest struct {
		Model     string   `json:"model"`
		Query     string   `json:"query"`
		Documents []string `json:"documents"`
		TopN      int      `json:"top_n"`
	}

	payload := rerankRequest{
		Model:     r.model,
		Query:     query,
		Documents: docs,
		TopN:      r.topN,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("rerank marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		r.apiURL+"/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("rerank build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if r.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.apiKey)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rerank request: %w", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("rerank read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rerank API status %d: %s", resp.StatusCode, b)
	}

	var apiResp struct {
		Results []RerankResult `json:"results"`
	}
	if err := json.Unmarshal(b, &apiResp); err != nil {
		return nil, fmt.Errorf("rerank unmarshal: %w", err)
	}

	return apiResp.Results, nil
}
