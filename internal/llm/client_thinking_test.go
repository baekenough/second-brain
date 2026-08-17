package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/llm"
)

// --- helpers ---

// captureBody returns a handler that records the decoded JSON request body
// into *dst and replies with a minimal successful completion.
func captureBody(t *testing.T, dst *map[string]any, reply []byte) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Errorf("unmarshal request body: %v", err)
			return
		}
		*dst = decoded
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(reply) //nolint:errcheck
	}
}

// chatResponseFinish builds an OpenAI-format chat completion response body
// with an explicit finish_reason and usage block.
func chatResponseFinish(content, finishReason string, completionTokens int) []byte {
	body := map[string]any{
		"choices": []map[string]any{
			{
				"message":       map[string]any{"content": content},
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     11,
			"completion_tokens": completionTokens,
			"total_tokens":      11 + completionTokens,
		},
	}
	data, _ := json.Marshal(body)
	return data
}

// sseChunkFinish builds an SSE data line carrying an optional content delta
// and a finish_reason.
func sseChunkFinish(content, finishReason string) string {
	choice := map[string]any{
		"delta":         map[string]any{"content": content},
		"finish_reason": finishReason,
	}
	data, _ := json.Marshal(map[string]any{"choices": []map[string]any{choice}})
	return "data: " + string(data) + "\n\n"
}

// sseServer replies with the given raw SSE payload.
func sseServer(t *testing.T, payload string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, payload) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- (a) thinking field presence/omission ---

func TestClient_Thinking_DisabledIsSentInRequestBody(t *testing.T) {
	t.Parallel()

	var got map[string]any
	srv := httptest.NewServer(captureBody(t, &got, chatResponseOK("ok")))
	t.Cleanup(srv.Close)

	c := llm.New(llm.Config{
		BaseURL:  srv.URL,
		Model:    "gpt-test",
		APIKey:   "key",
		Thinking: llm.ThinkingDisabled,
	}, nil)

	if _, err := c.Complete(context.Background(), "sys", "user"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	field, ok := got["thinking"]
	if !ok {
		t.Fatalf("request body must contain \"thinking\" when disabled; body=%v", keysOf(got))
	}
	obj, ok := field.(map[string]any)
	if !ok {
		t.Fatalf("thinking must be an object, got %T", field)
	}
	if obj["type"] != "disabled" {
		t.Fatalf("thinking.type = %v, want \"disabled\"", obj["type"])
	}
}

func TestClient_Thinking_EnabledOmitsField(t *testing.T) {
	t.Parallel()

	var got map[string]any
	srv := httptest.NewServer(captureBody(t, &got, chatResponseOK("ok")))
	t.Cleanup(srv.Close)

	c := llm.New(llm.Config{
		BaseURL:  srv.URL,
		Model:    "gpt-test",
		APIKey:   "key",
		Thinking: llm.ThinkingEnabled,
	}, nil)

	if _, err := c.Complete(context.Background(), "sys", "user"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, ok := got["thinking"]; ok {
		t.Fatalf("request body must NOT contain \"thinking\" when enabled (endpoints that do not know the parameter reject it); body=%v", keysOf(got))
	}
}

func TestClient_Thinking_EmptyConfigDefaultsToDisabled(t *testing.T) {
	t.Parallel()

	var got map[string]any
	srv := httptest.NewServer(captureBody(t, &got, chatResponseOK("ok")))
	t.Cleanup(srv.Close)

	// Zero-value Config.Thinking → disabled (the safe production default:
	// reasoning_content burns the max_tokens budget and yields empty content).
	c := llm.New(llm.Config{
		BaseURL: srv.URL,
		Model:   "gpt-test",
		APIKey:  "key",
	}, nil)

	if _, err := c.Complete(context.Background(), "sys", "user"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	obj, ok := got["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("zero-value Thinking must default to disabled; body=%v", keysOf(got))
	}
	if obj["type"] != "disabled" {
		t.Fatalf("thinking.type = %v, want \"disabled\"", obj["type"])
	}
}

// (e, part 1) the streaming request body carries the same thinking setting.
func TestClient_Thinking_AppliesToStreamingRequest(t *testing.T) {
	t.Parallel()

	var disabledBody, enabledBody map[string]any

	streamReply := func(dst *map[string]any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			var decoded map[string]any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Errorf("unmarshal streaming request body: %v", err)
				return
			}
			*dst = decoded
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, sseChunkFinish("hi", "")+"data: [DONE]\n\n") //nolint:errcheck
		}
	}

	srvDisabled := httptest.NewServer(streamReply(&disabledBody))
	t.Cleanup(srvDisabled.Close)
	srvEnabled := httptest.NewServer(streamReply(&enabledBody))
	t.Cleanup(srvEnabled.Close)

	cDisabled := llm.New(llm.Config{BaseURL: srvDisabled.URL, Model: "m", APIKey: "k", Thinking: llm.ThinkingDisabled}, nil)
	if err := cDisabled.StreamWithMessages(context.Background(), "sys", nil, func(string) {}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	obj, ok := disabledBody["thinking"].(map[string]any)
	if !ok || obj["type"] != "disabled" {
		t.Fatalf("streaming request must carry thinking.type=disabled; got %v", disabledBody["thinking"])
	}

	cEnabled := llm.New(llm.Config{BaseURL: srvEnabled.URL, Model: "m", APIKey: "k", Thinking: llm.ThinkingEnabled}, nil)
	if err := cEnabled.StreamWithMessages(context.Background(), "sys", nil, func(string) {}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := enabledBody["thinking"]; ok {
		t.Fatalf("streaming request must omit thinking when enabled; body=%v", keysOf(enabledBody))
	}
}

// --- (b) finish_reason=length + empty content → error ---

func TestClient_Complete_LengthWithEmptyContentIsError(t *testing.T) {
	t.Parallel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(chatResponseFinish("", "length", 8000)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, srv.URL, "key")
	got, err := c.Complete(context.Background(), "sys", "user")
	if err == nil {
		t.Fatalf("expected error for finish_reason=length with empty content, got content %q", got)
	}
	if got != "" {
		t.Fatalf("no content must be returned alongside a truncation error, got %q", got)
	}

	var te *llm.TruncatedError
	if !errors.As(err, &te) {
		t.Fatalf("error must be a *llm.TruncatedError, got %T: %v", err, err)
	}
	if !errors.Is(err, llm.ErrTruncated) {
		t.Fatalf("errors.Is(err, llm.ErrTruncated) must hold, got: %v", err)
	}
	if te.FinishReason != "length" {
		t.Fatalf("FinishReason = %q, want \"length\"", te.FinishReason)
	}
	if te.CompletionTokens != 8000 {
		t.Fatalf("CompletionTokens = %d, want 8000", te.CompletionTokens)
	}
	if te.ContentLength != 0 {
		t.Fatalf("ContentLength = %d, want 0", te.ContentLength)
	}
	msg := err.Error()
	if !strings.Contains(msg, "length") || !strings.Contains(msg, "8000") {
		t.Fatalf("error message must name finish_reason and completion tokens, got: %s", msg)
	}
	// Truncation is deterministic — retrying burns the same budget again.
	if calls != 1 {
		t.Fatalf("truncation must not be retried, got %d calls", calls)
	}
}

// --- (c) finish_reason=length + non-empty content → error (silent data loss) ---

func TestClient_Complete_LengthWithPartialContentIsError(t *testing.T) {
	t.Parallel()

	const partial = `{"entities":[{"name":"dummy-alpha","typ`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(chatResponseFinish(partial, "length", 8000)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, srv.URL, "key")
	got, err := c.Complete(context.Background(), "sys", "user")
	if err == nil {
		t.Fatal("truncated (non-empty) content must be an error, not a silent partial result")
	}
	if got != "" {
		t.Fatalf("truncated content must not be returned to the caller, got %q", got)
	}
	var te *llm.TruncatedError
	if !errors.As(err, &te) {
		t.Fatalf("error must be a *llm.TruncatedError, got %T: %v", err, err)
	}
	if te.ContentLength != len(partial) {
		t.Fatalf("ContentLength = %d, want %d", te.ContentLength, len(partial))
	}
}

// --- (d) finish_reason=stop → unchanged happy path ---

func TestClient_Complete_StopUnchanged(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(chatResponseFinish(`{"entities":[]}`, "stop", 12)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, srv.URL, "key")
	got, err := c.Complete(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("unexpected error on finish_reason=stop: %v", err)
	}
	if got != `{"entities":[]}` {
		t.Fatalf("got %q, want %q", got, `{"entities":[]}`)
	}
}

// A response with no finish_reason at all (proxies that omit it) must keep
// working exactly as before.
func TestClient_Complete_MissingFinishReasonUnchanged(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(chatResponseOK("plain answer")) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, srv.URL, "key")
	got, err := c.Complete(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "plain answer" {
		t.Fatalf("got %q, want %q", got, "plain answer")
	}
}

// --- (e) streaming applies the same verdict ---

func TestClient_Stream_LengthFinishReasonIsError(t *testing.T) {
	t.Parallel()

	payload := sseChunkFinish("partial ", "") +
		sseChunkFinish("output", "length") +
		"data: [DONE]\n\n"
	srv := sseServer(t, payload)

	c := newClient(t, srv.URL, "key")
	var seen strings.Builder
	err := c.StreamWithMessages(context.Background(), "sys", nil, func(d string) {
		seen.WriteString(d)
	})
	if err == nil {
		t.Fatal("streaming must report finish_reason=length as an error")
	}
	var te *llm.TruncatedError
	if !errors.As(err, &te) {
		t.Fatalf("error must be a *llm.TruncatedError, got %T: %v", err, err)
	}
	if te.ContentLength != len("partial output") {
		t.Fatalf("ContentLength = %d, want %d", te.ContentLength, len("partial output"))
	}
	if seen.String() != "partial output" {
		t.Fatalf("deltas already delivered before truncation must not be suppressed, got %q", seen.String())
	}
}

func TestClient_Stream_StopFinishReasonUnchanged(t *testing.T) {
	t.Parallel()

	payload := sseChunkFinish("hello ", "") +
		sseChunkFinish("world", "stop") +
		"data: [DONE]\n\n"
	srv := sseServer(t, payload)

	c := newClient(t, srv.URL, "key")
	var seen strings.Builder
	if err := c.StreamWithMessages(context.Background(), "sys", nil, func(d string) {
		seen.WriteString(d)
	}); err != nil {
		t.Fatalf("unexpected error on finish_reason=stop: %v", err)
	}
	if seen.String() != "hello world" {
		t.Fatalf("got %q, want %q", seen.String(), "hello world")
	}
}

// --- (f) the error must never carry the response body ---

func TestClient_TruncationError_DoesNotLeakResponseBody(t *testing.T) {
	t.Parallel()

	// Stand-in for personally identifying text a real completion may contain.
	const marker = "DUMMY-PII-MARKER-ZZZ"

	t.Run("non-streaming", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(chatResponseFinish(`{"name":"`+marker+`"`, "length", 8000)) //nolint:errcheck
		}))
		t.Cleanup(srv.Close)

		c := newClient(t, srv.URL, "key")
		_, err := c.Complete(context.Background(), "sys", "user")
		if err == nil {
			t.Fatal("expected truncation error")
		}
		if strings.Contains(err.Error(), marker) {
			t.Fatalf("error message leaks response content: %s", err.Error())
		}
	})

	t.Run("streaming", func(t *testing.T) {
		t.Parallel()
		srv := sseServer(t, sseChunkFinish(marker, "length")+"data: [DONE]\n\n")

		c := newClient(t, srv.URL, "key")
		err := c.StreamWithMessages(context.Background(), "sys", nil, func(string) {})
		if err == nil {
			t.Fatal("expected truncation error")
		}
		if strings.Contains(err.Error(), marker) {
			t.Fatalf("error message leaks streamed content: %s", err.Error())
		}
	})
}

// keysOf returns the sorted-ish key set of a decoded JSON body for failure
// messages (never the values — bodies may contain personal data).
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
