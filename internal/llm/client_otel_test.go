package llm_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

// These tests exercise the OTel span structure of internal/llm and are
// intentionally NOT run in parallel (no t.Parallel()): they mutate the
// process-global OTel TracerProvider via otel.SetTracerProvider, which the
// package's other (parallel) tests never touch. Go's test runner completes
// all non-parallel tests — including these — before resuming any test that
// called t.Parallel(), so there is no overlap with the rest of this
// package's test suite despite the shared global state.

// withInMemoryTracer installs an SDK TracerProvider backed by an in-memory
// span exporter (synchronous export via WithSyncer — no batching delay) as
// the global provider for the duration of the test, and restores a no-op
// provider on cleanup.
func withInMemoryTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(noop.NewTracerProvider())
	})
	return exp
}

// chatResponseOKWithUsage builds an OpenAI-format chat completion response
// body that additionally reports token usage, exercising the usage-parsing
// path added for gen_ai.usage.* span attributes.
func chatResponseOKWithUsage(content string, promptTokens, completionTokens int) []byte {
	type msg struct {
		Content string `json:"content"`
	}
	type choice struct {
		Message msg `json:"message"`
	}
	type usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	}
	type resp struct {
		Choices []choice `json:"choices"`
		Usage   usage    `json:"usage"`
	}
	data, _ := json.Marshal(resp{
		Choices: []choice{{Message: msg{Content: content}}},
		Usage:   usage{PromptTokens: promptTokens, CompletionTokens: completionTokens, TotalTokens: promptTokens + completionTokens},
	})
	return data
}

func TestClient_CompleteWithMessages_ParentAndAttemptSpans_OnRetry(t *testing.T) {
	exp := withInMemoryTracer(t)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(chatResponseOKWithUsage("recovered", 42, 7)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, srv.URL, "key")
	got, err := c.Complete(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "recovered" {
		t.Fatalf("want %q, got %q", "recovered", got)
	}
	if calls != 3 {
		t.Fatalf("expected 3 HTTP calls (2 failed attempts + 1 success), got %d", calls)
	}

	spans := exp.GetSpans()

	var parents, children int
	var parentSpan tracetest.SpanStub
	for _, s := range spans {
		switch s.Name {
		case "llm.complete":
			parents++
			parentSpan = s
		case "llm.complete.attempt":
			children++
		}
	}

	if parents != 1 {
		t.Fatalf("expected exactly 1 parent 'llm.complete' span, got %d (spans=%v)", parents, spanNames(spans))
	}
	if children != 3 {
		t.Fatalf("expected exactly 3 'llm.complete.attempt' child spans (one per HTTP call), got %d (spans=%v)", children, spanNames(spans))
	}

	// Every attempt span must be a child of the single parent span.
	for _, s := range spans {
		if s.Name != "llm.complete.attempt" {
			continue
		}
		if s.Parent.SpanID() != parentSpan.SpanContext.SpanID() {
			t.Errorf("attempt span %s has parent SpanID %s, want parent 'llm.complete' SpanID %s",
				s.SpanContext.SpanID(), s.Parent.SpanID(), parentSpan.SpanContext.SpanID())
		}
		if s.Parent.TraceID() != parentSpan.SpanContext.TraceID() {
			t.Errorf("attempt span has TraceID %s, want parent's TraceID %s", s.Parent.TraceID(), parentSpan.SpanContext.TraceID())
		}
	}

	// GenAI attributes on the parent span.
	attrs := attrMap(parentSpan)
	if got := attrs["gen_ai.system"]; got != "openai" {
		t.Errorf("gen_ai.system = %q, want %q", got, "openai")
	}
	if got := attrs["gen_ai.request.model"]; got != "gpt-test" {
		t.Errorf("gen_ai.request.model = %q, want %q", got, "gpt-test")
	}
	if got := attrs["gen_ai.usage.input_tokens"]; got != "42" {
		t.Errorf("gen_ai.usage.input_tokens = %q, want %q (from successful attempt's response)", got, "42")
	}
	if got := attrs["gen_ai.usage.output_tokens"]; got != "7" {
		t.Errorf("gen_ai.usage.output_tokens = %q, want %q", got, "7")
	}
}

func TestClient_CompleteWithMessages_SingleAttemptSpan_OnFirstTrySuccess(t *testing.T) {
	exp := withInMemoryTracer(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(chatResponseOK("hello")) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, srv.URL, "key")
	if _, err := c.Complete(context.Background(), "sys", "user"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	var parents, children int
	for _, s := range spans {
		switch s.Name {
		case "llm.complete":
			parents++
		case "llm.complete.attempt":
			children++
		}
	}
	if parents != 1 || children != 1 {
		t.Fatalf("expected 1 parent + 1 child span on first-try success, got parents=%d children=%d (spans=%v)", parents, children, spanNames(spans))
	}
}

func TestClient_StreamWithMessages_SingleSpanWithGenAIAttributes(t *testing.T) {
	exp := withInMemoryTracer(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n")) //nolint:errcheck
		w.Write([]byte("data: [DONE]\n\n"))                                                                  //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, srv.URL, "key")
	var got string
	err := c.StreamWithMessages(context.Background(), "sys", nil, func(delta string) { got += delta })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hi" {
		t.Fatalf("want %q, got %q", "hi", got)
	}

	spans := exp.GetSpans()
	var streamSpans int
	var streamSpan tracetest.SpanStub
	for _, s := range spans {
		if s.Name == "llm.stream" {
			streamSpans++
			streamSpan = s
		}
	}
	if streamSpans != 1 {
		t.Fatalf("expected exactly 1 'llm.stream' span, got %d (spans=%v)", streamSpans, spanNames(spans))
	}

	attrs := attrMap(streamSpan)
	if got := attrs["gen_ai.system"]; got != "openai" {
		t.Errorf("gen_ai.system = %q, want %q", got, "openai")
	}
	if got := attrs["gen_ai.request.model"]; got != "gpt-test" {
		t.Errorf("gen_ai.request.model = %q, want %q", got, "gpt-test")
	}
}

func TestClient_StreamWithMessages_ErrorRecordedOnSpan(t *testing.T) {
	exp := withInMemoryTracer(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, srv.URL, "key")
	err := c.StreamWithMessages(context.Background(), "sys", nil, func(string) {})
	if err == nil {
		t.Fatal("expected error from failing stream request")
	}

	spans := exp.GetSpans()
	var streamSpan tracetest.SpanStub
	found := false
	for _, s := range spans {
		if s.Name == "llm.stream" {
			streamSpan = s
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an 'llm.stream' span even on failure, got spans=%v", spanNames(spans))
	}
	if len(streamSpan.Status.Description) == 0 && streamSpan.Status.Code == 0 {
		t.Error("expected span status to reflect the error (RecordError/SetStatus), got zero-value status")
	}
	if len(streamSpan.Events) == 0 {
		t.Error("expected span.RecordError to add an exception event, got no events")
	}
}

// spanNames is a small debug helper for test failure messages.
func spanNames(spans tracetest.SpanStubs) []string {
	names := make([]string, len(spans))
	for i, s := range spans {
		names[i] = s.Name
	}
	return names
}

// attrMap flattens a span's attributes into a string-keyed map for
// convenient assertions (values stringified via their Emit()).
func attrMap(s tracetest.SpanStub) map[string]string {
	out := make(map[string]string, len(s.Attributes))
	for _, kv := range s.Attributes {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}
