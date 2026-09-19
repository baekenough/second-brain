// Package jev provides a client for the TypeSafe "Jev" classification API
// (POST https://api.typesafe.ai/v1/systemone). It is used by
// internal/classify to classify documents (SMS, Gmail, call transcripts)
// into a segment (choice question) and a retention score (score question).
//
// The client never logs request or response bodies — both may contain
// personal message content — only shapes and counts (see
// guides/... personal-data logging policy referenced by
// internal/worker/classification_worker.go).
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// defaultAPIURL is the TypeSafe Jev classification endpoint.
const defaultAPIURL = "https://api.typesafe.ai/v1/systemone"

// defaultModel is the Jev model used for all classification calls.
const defaultModel = "jev-latest"

const (
	requestTimeout = 15 * time.Second
	maxRetries     = 3
)

// retryBackoffUnit is the linear backoff step (attempt * retryBackoffUnit)
// applied between retries on a 429/5xx response. A var, not a const, so
// client_test.go can shrink it for fast retry tests without changing
// production behaviour (2s).
var retryBackoffUnit = 2 * time.Second

// QuestionType identifies the shape of a Question's expected answer.
type QuestionType string

const (
	// QuestionChoice asks Jev to pick one option from Criteria (a
	// map[string]string of option -> description) and answers with
	// ChoiceAnswer.
	QuestionChoice QuestionType = "choice"
	// QuestionScore asks Jev to rate the state against an ordered list of
	// Criteria descriptions (index = score) and answers with ScoreAnswer.
	QuestionScore QuestionType = "score"
)

// Question is one entry of a classification Request's Questions map.
type Question struct {
	Type         QuestionType `json:"type"`
	Instructions string       `json:"instructions"`
	// Criteria is map[string]string for QuestionChoice (option -> description)
	// or []string for QuestionScore (ordered score descriptions).
	Criteria any `json:"criteria"`
}

// request is the wire shape of a POST /v1/systemone body.
type request struct {
	State     string              `json:"state"`
	Model     string              `json:"model"`
	Questions map[string]Question `json:"questions"`
}

// ChoiceAnswer is Jev's answer to a QuestionChoice question.
type ChoiceAnswer struct {
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
}

// Probability returns the probability Jev assigned to the chosen option,
// preferring Confidence (the top-choice probability field) and falling back
// to Probabilities[Choice] when Confidence is unset. Returns 0 when neither
// is available — callers must treat 0 as "no confidence", never as a
// disposable-eligible high-confidence answer.
func (a ChoiceAnswer) Probability() float64 {
	if a.Confidence > 0 {
		return a.Confidence
	}
	if a.Probabilities != nil {
		return a.Probabilities[a.Choice]
	}
	return 0
}

// ScoreAnswer is Jev's answer to a QuestionScore question.
type ScoreAnswer struct {
	Score      float64        `json:"score"`
	Legend     map[string]any `json:"legend"`
	Confidence float64        `json:"confidence"`
}

// Usage reports token accounting for a single Jev call.
type Usage struct {
	InputTokens int `json:"input_tokens"`
}

// Response is a decoded POST /v1/systemone response. Individual answers are
// kept as raw JSON until Choice/Score is called with the expected question
// name, because the wire shape of answers[name] depends on the
// corresponding request Question's Type, which the Response itself does not
// carry.
type Response struct {
	Answers map[string]json.RawMessage `json:"answers"`
	Usage   Usage                      `json:"usage"`
}

// Choice decodes the named answer as a ChoiceAnswer. Returns an error if the
// answer is missing or does not decode.
func (r *Response) Choice(name string) (ChoiceAnswer, error) {
	raw, ok := r.Answers[name]
	if !ok {
		return ChoiceAnswer{}, fmt.Errorf("jev: answer %q missing from response", name)
	}
	var a ChoiceAnswer
	if err := json.Unmarshal(raw, &a); err != nil {
		return ChoiceAnswer{}, fmt.Errorf("jev: decode choice answer %q: %w", name, err)
	}
	if a.Choice == "" {
		return ChoiceAnswer{}, fmt.Errorf("jev: answer %q has empty choice", name)
	}
	return a, nil
}

// Score decodes the named answer as a ScoreAnswer. Returns an error if the
// answer is missing or does not decode.
func (r *Response) Score(name string) (ScoreAnswer, error) {
	raw, ok := r.Answers[name]
	if !ok {
		return ScoreAnswer{}, fmt.Errorf("jev: answer %q missing from response", name)
	}
	var a ScoreAnswer
	if err := json.Unmarshal(raw, &a); err != nil {
		return ScoreAnswer{}, fmt.Errorf("jev: decode score answer %q: %w", name, err)
	}
	return a, nil
}

// Client is a TypeSafe Jev API client.
type Client struct {
	baseURL    string
	model      string
	apiKey     string
	httpClient *http.Client
}

// New constructs a Client. apiKey may be empty — Enabled() reports false in
// that case and callers must not invoke Classify. httpClient, when nil,
// defaults to a client with a 15s timeout.
func New(apiKey string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}
	return &Client{
		baseURL:    defaultAPIURL,
		model:      defaultModel,
		apiKey:     apiKey,
		httpClient: httpClient,
	}
}

// NewForTest constructs a Client pointed at baseURL instead of the real Jev
// endpoint. Exported (not test-file-scoped) so other packages' tests — e.g.
// internal/classify's — can point a Classifier at an httptest.Server without
// internal/jev having to export its baseURL field for production use.
func NewForTest(baseURL, apiKey string, httpClient *http.Client) *Client {
	c := New(apiKey, httpClient)
	c.baseURL = baseURL
	return c
}

// Enabled reports whether the client has an API key configured. Callers
// (internal/classify.Classifier, internal/worker.ClassificationWorker) must
// check this before calling Classify and degrade to leaving documents
// unclassified when it is false.
func (c *Client) Enabled() bool {
	return c != nil && c.apiKey != ""
}

// Classify sends state and questions to the Jev API and returns the decoded
// response. Retries up to maxRetries times, with a linear backoff
// (attempt * retryBackoffUnit), on HTTP 429 and 5xx responses and on
// network-level errors. state and questions are never logged — only the
// resulting error (which must never embed response bodies) may surface to
// callers' logs.
func (c *Client) Classify(ctx context.Context, state string, questions map[string]Question) (*Response, error) {
	if !c.Enabled() {
		return nil, errors.New("jev: client not configured (missing API key)")
	}

	body, err := json.Marshal(request{
		State:     state,
		Model:     c.model,
		Questions: questions,
	})
	if err != nil {
		return nil, fmt.Errorf("jev: marshal request: %w", err)
	}

	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		resp, retryable, err := c.doOnce(ctx, body)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !retryable || attempt == maxRetries {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt) * retryBackoffUnit):
		}
	}
	return nil, fmt.Errorf("jev: classify failed after %d attempt(s): %w", maxRetries, lastErr)
}

// doOnce performs a single HTTP round trip. The bool return indicates
// whether the error (if any) is retryable (429 or 5xx, or a network-level
// failure).
func (c *Client) doOnce(ctx context.Context, body []byte) (*Response, bool, error) {
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		// Network-level failure (timeout, connection reset, ...): retryable.
		return nil, true, fmt.Errorf("request failed: %w", err)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, true, fmt.Errorf("read response body: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		retryable := httpResp.StatusCode == http.StatusTooManyRequests || httpResp.StatusCode >= 500
		// Body length only — never the body itself, which may echo request
		// content back (error messages sometimes quote invalid fields).
		return nil, retryable, fmt.Errorf("unexpected status %d (body length %d)", httpResp.StatusCode, len(respBody))
	}

	var parsed Response
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, false, fmt.Errorf("decode response: %w", err)
	}
	return &parsed, false, nil
}
