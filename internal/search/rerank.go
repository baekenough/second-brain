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
	client *http.Client
}

// NewHTTPReranker creates a reranker backed by the given endpoint.
// Pass empty apiURL to get a disabled (no-op) reranker. The last argument is
// retained for constructor compatibility; top_n now follows the bounded input
// candidate pool so a legacy fixed value cannot silently shrink result pages.
func NewHTTPReranker(apiURL, apiKey, model string, _ int) *HTTPReranker {
	return &HTTPReranker{
		apiURL: apiURL,
		apiKey: apiKey,
		model:  model,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// Enabled reports whether a rerank API endpoint is configured.
func (r *HTTPReranker) Enabled() bool { return r.apiURL != "" }

// rerankResponseBytesPerDoc 는 결과 항목 하나({"index":…,"relevance_score":…})
// 의 바이트 상한 추정치다. return_documents 를 쓰지 않으므로 문서 본문은
// 응답에 없다.
const rerankResponseBytesPerDoc = 256

// rerankResponseLimit 은 문서 n 개를 리랭크한 성공 응답 본문의 상한이다.
func rerankResponseLimit(n int) int64 {
	return 1<<20 + int64(n)*rerankResponseBytesPerDoc
}

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
		// ReturnDocuments 를 false 로 명시한다. 기본값이 true 인 공급자가 있어
		// 생략하면 응답에 문서 본문이 되돌아온다 — 응답 상한
		// (rerankResponseLimit)은 본문이 없다는 전제로 잡았다.
		ReturnDocuments bool `json:"return_documents"`
	}

	payload := rerankRequest{
		Model:     r.model,
		Query:     query,
		Documents: docs,
		TopN:      len(docs),
		// 문서 본문은 응답에 필요 없다(index·score 만 쓴다).
		ReturnDocuments: false,
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

	if resp.StatusCode != http.StatusOK {
		// 오류 문구는 상태 코드만 담는다("rerank API status %d" 유지). 리랭커
		// 오류 본문은 문서 조각을 되돌려 줄 수 있어 읽지 않고 상한 안에서
		// 버리기만 한다(연결 재사용).
		httperr.Drain(resp.Body)
		return nil, &httperr.StatusError{Prefix: "rerank API status", StatusCode: resp.StatusCode}
	}
	b, err := httperr.ReadBody(resp.Body, rerankResponseLimit(len(docs)))
	if err != nil {
		return nil, fmt.Errorf("rerank read response: %w", err)
	}

	var apiResp struct {
		Results []RerankResult `json:"results"`
	}
	if err := json.Unmarshal(b, &apiResp); err != nil {
		return nil, fmt.Errorf("rerank unmarshal: %w", err)
	}

	return apiResp.Results, nil
}
