package llm_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/baekenough/second-brain/internal/llm"
)

// chatResponseOK builds a minimal OpenAI-format chat completion response body.
func chatResponseOK(content string) []byte {
	type msg struct {
		Content string `json:"content"`
	}
	type choice struct {
		Message msg `json:"message"`
	}
	type resp struct {
		Choices []choice `json:"choices"`
	}
	data, _ := json.Marshal(resp{Choices: []choice{{Message: msg{Content: content}}}})
	return data
}

// chatResponseError builds a 200-status body that contains an API-level error.
func chatResponseError(msg string) []byte {
	type errField struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	type resp struct {
		Error *errField `json:"error"`
	}
	data, _ := json.Marshal(resp{Error: &errField{Message: msg, Type: "invalid_request_error"}})
	return data
}

// newClient builds an llm.Client pointed at the given test server URL.
func newClient(t *testing.T, serverURL, apiKey string) *llm.Client {
	t.Helper()
	return llm.New(llm.Config{
		BaseURL:     serverURL,
		Model:       "gpt-test",
		APIKey:      apiKey,
		MaxTokens:   128,
		Temperature: 0,
	}, nil)
}

// --- Tests ---

func TestClient_Complete_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(chatResponseOK("hello world")) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, srv.URL, "test-key")
	got, err := c.Complete(context.Background(), "sys", "user msg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hello world" {
		t.Fatalf("want %q, got %q", "hello world", got)
	}
}

func TestClient_Complete_401NoRetry(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, srv.URL, "bad-key")
	_, err := c.Complete(context.Background(), "sys", "user")
	if err == nil {
		t.Fatal("expected error for 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error should mention 401, got: %v", err)
	}
	// 4xx must NOT be retried — exactly one call expected.
	if n := calls.Load(); n != 1 {
		t.Fatalf("expected exactly 1 call (no retry for 401), got %d", n)
	}
}

func TestClient_Complete_500Retry(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			// First two calls return 500.
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// Third call succeeds.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(chatResponseOK("recovered")) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, srv.URL, "key")
	got, err := c.Complete(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("unexpected error after retry: %v", err)
	}
	if got != "recovered" {
		t.Fatalf("want %q, got %q", "recovered", got)
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("expected 3 calls (2 failures + 1 success), got %d", n)
	}
}

func TestClient_Complete_AuthHeaderInjected(t *testing.T) {
	t.Parallel()

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(chatResponseOK("ok")) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, srv.URL, "my-secret-key")
	if _, err := c.Complete(context.Background(), "sys", "user"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := "Bearer my-secret-key"
	if gotAuth != want {
		t.Fatalf("Authorization header: want %q, got %q", want, gotAuth)
	}
}

func TestClient_Enabled_WithoutToken(t *testing.T) {
	t.Parallel()

	// No APIKey and no AuthFile → token source is nil → Enabled() must be false.
	c := llm.New(llm.Config{
		BaseURL: "http://localhost:1234",
		Model:   "gpt-test",
	}, nil)
	if c.Enabled() {
		t.Fatal("Enabled() should be false when no token source is configured")
	}
}

func TestClient_CompleteWithMessages_MultiTurn(t *testing.T) {
	t.Parallel()

	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		capturedBody, err = func() ([]byte, error) {
			buf := make([]byte, r.ContentLength)
			_, e := r.Body.Read(buf)
			return buf, e
		}()
		if err != nil && err.Error() != "EOF" {
			t.Errorf("read body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(chatResponseOK("multi-turn answer")) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, srv.URL, "key")
	history := []llm.Message{
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "first answer"},
	}
	got, err := c.CompleteWithMessages(context.Background(), "system prompt", history)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "multi-turn answer" {
		t.Fatalf("want %q, got %q", "multi-turn answer", got)
	}

	// Verify the system message was prepended and history is preserved.
	var reqBody struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(capturedBody, &reqBody); err != nil {
		t.Fatalf("unmarshal captured body: %v", err)
	}

	if len(reqBody.Messages) < 3 {
		t.Fatalf("expected at least 3 messages (system + 2 history), got %d", len(reqBody.Messages))
	}
	if reqBody.Messages[0].Role != "system" {
		t.Fatalf("first message role: want %q, got %q", "system", reqBody.Messages[0].Role)
	}
	if reqBody.Messages[0].Content != "system prompt" {
		t.Fatalf("system content: want %q, got %q", "system prompt", reqBody.Messages[0].Content)
	}
	if reqBody.Messages[1].Role != "user" || reqBody.Messages[1].Content != "first question" {
		t.Fatalf("history[0] mismatch: %+v", reqBody.Messages[1])
	}
	if reqBody.Messages[2].Role != "assistant" || reqBody.Messages[2].Content != "first answer" {
		t.Fatalf("history[1] mismatch: %+v", reqBody.Messages[2])
	}
}

func TestClient_APIErrorIn200(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(chatResponseError("rate_limit_exceeded")) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, srv.URL, "key")
	_, err := c.Complete(context.Background(), "sys", "user")
	if err == nil {
		t.Fatal("expected error for API-level error in 200, got nil")
	}
	if !strings.Contains(err.Error(), "rate_limit_exceeded") {
		t.Fatalf("error should contain API error message, got: %v", err)
	}
}

// Compile-time assertion: *Client satisfies Completer.
var _ llm.Completer = (*llm.Client)(nil)
