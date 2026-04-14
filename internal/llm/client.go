// Package llm provides an OpenAI-compatible chat completion client.
// It is used by the Discord gateway to generate RAG-based answers
// from retrieved context documents.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Client is an OpenAI-compatible chat completion client.
// It communicates with any endpoint that speaks the /v1/chat/completions protocol
// (OpenAI, Azure OpenAI, local proxies such as cliproxy, etc.).
type Client struct {
	baseURL    string
	model      string
	apiKey     string
	maxTokens  int
	temperature float64
	httpClient *http.Client
}

// Config holds the parameters required to construct a Client.
type Config struct {
	BaseURL     string
	Model       string
	APIKey      string
	MaxTokens   int
	Temperature float64
}

// New returns a Client configured with the given Config.
// httpClient may be nil — the default http.Client with a 60-second timeout is used.
func New(cfg Config, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	// Normalise base URL: strip trailing slash.
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	return &Client{
		baseURL:     baseURL,
		model:       cfg.Model,
		apiKey:      cfg.APIKey,
		maxTokens:   cfg.MaxTokens,
		temperature: cfg.Temperature,
		httpClient:  httpClient,
	}
}

// Enabled reports whether the client has the minimum required configuration
// to make a request (non-empty base URL and model).
func (c *Client) Enabled() bool {
	return c.baseURL != "" && c.model != ""
}

// message is a single chat message in the OpenAI format.
type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatRequest is the request body for POST /v1/chat/completions.
type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []message `json:"messages"`
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens"`
}

// chatResponse is the relevant subset of the OpenAI chat completion response.
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Complete sends a single-turn chat completion request.
// system is the system prompt; user is the user turn content.
// 4xx responses are not retried. 5xx and network errors are retried up to 2 times.
func (c *Client) Complete(ctx context.Context, system, user string) (string, error) {
	if !c.Enabled() {
		return "", fmt.Errorf("llm: client is not configured (missing base URL or model)")
	}

	reqBody := chatRequest{
		Model: c.model,
		Messages: []message{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature: c.temperature,
		MaxTokens:   c.maxTokens,
	}

	const maxRetries = 2
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		result, err := c.doRequest(ctx, reqBody)
		if err == nil {
			return result, nil
		}
		if isClientError(err) {
			// 4xx — do not retry.
			return "", err
		}
		lastErr = err
		slog.Warn("llm: request failed, will retry",
			"attempt", attempt+1,
			"max_retries", maxRetries,
			"error", err,
		)
	}
	return "", fmt.Errorf("llm: all retries exhausted: %w", lastErr)
}

// clientError wraps an HTTP 4xx response so the caller can detect it without retrying.
type clientError struct {
	statusCode int
	body       string
}

func (e *clientError) Error() string {
	return fmt.Sprintf("llm: HTTP %d: %s", e.statusCode, e.body)
}

func isClientError(err error) bool {
	var ce *clientError
	return err != nil && (func() bool {
		var ok bool
		ce, ok = err.(*clientError)
		return ok && ce.statusCode >= 400 && ce.statusCode < 500
	})()
}

// doRequest performs a single HTTP round-trip to the chat completions endpoint.
func (c *Client) doRequest(ctx context.Context, reqBody chatRequest) (string, error) {
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("llm: marshal request: %w", err)
	}

	url := c.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm: HTTP request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("llm: read response body: %w", err)
	}

	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return "", &clientError{statusCode: resp.StatusCode, body: string(body)}
	}
	if resp.StatusCode >= 500 {
		return "", fmt.Errorf("llm: server error %d: %s", resp.StatusCode, string(body))
	}

	var chatResp chatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return "", fmt.Errorf("llm: unmarshal response: %w", err)
	}

	// Surface API-level errors embedded in a 200 response (some proxies do this).
	if chatResp.Error != nil {
		return "", &clientError{
			statusCode: http.StatusOK,
			body:       chatResp.Error.Message,
		}
	}

	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("llm: response contains no choices")
	}

	return chatResp.Choices[0].Message.Content, nil
}
