package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/baekenough/second-brain/internal/telemetry"
)

// These tests exercise the OTel span structure of EmbedClient and are
// intentionally NOT run in parallel (no t.Parallel()): they mutate the
// process-global OTel TracerProvider via otel.SetTracerProvider, which the
// rest of this package's tests never touch. Go's test runner completes all
// non-parallel tests — including these — before resuming any test that
// called t.Parallel(), so there is no overlap with this package's other
// (parallel) tests despite the shared global state. See the identical
// pattern and rationale in internal/llm/client_otel_test.go.
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

func spanNames(spans tracetest.SpanStubs) []string {
	names := make([]string, len(spans))
	for i, s := range spans {
		names[i] = s.Name
	}
	return names
}

func attrMap(s tracetest.SpanStub) map[string]string {
	out := make(map[string]string, len(s.Attributes))
	for _, kv := range s.Attributes {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

func TestEmbed_SingleSpanWithGenAIAttributes(t *testing.T) {
	exp := withInMemoryTracer(t)

	vec := fakeVec(1536)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(embeddingResponse([][]float32{vec}))
	}))
	t.Cleanup(srv.Close)

	c := NewEmbedClient(srv.URL, "sk-test", "", "text-embedding-3-small", 1536)
	if _, err := c.Embed(context.Background(), "hello"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := exp.GetSpans()
	var found int
	var span tracetest.SpanStub
	for _, s := range spans {
		if s.Name == "embedding.single" {
			found++
			span = s
		}
	}
	if found != 1 {
		t.Fatalf("expected exactly 1 'embedding.single' span, got %d (spans=%v)", found, spanNames(spans))
	}

	attrs := attrMap(span)
	if got := attrs[telemetry.AttrGenAISystem]; got != "openai" {
		t.Errorf("gen_ai.system = %q, want %q", got, "openai")
	}
	if got := attrs[telemetry.AttrGenAIRequestModel]; got != "text-embedding-3-small" {
		t.Errorf("gen_ai.request.model = %q, want %q", got, "text-embedding-3-small")
	}
}

func TestEmbed_DisabledClient_NoSpan(t *testing.T) {
	exp := withInMemoryTracer(t)

	c := NewEmbedClient("https://api.openai.com/v1", "", "", "text-embedding-3-small", 1536)
	if _, err := c.Embed(context.Background(), "hello"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spans := exp.GetSpans(); len(spans) != 0 {
		t.Fatalf("expected no spans for a disabled (no-op) client call, got %v", spanNames(spans))
	}
}

// TestEmbedBatch_ParentAndSubBatchChildSpans verifies EmbedBatch's tracing
// contract from the plan: one top-level "embedding.batch" parent span, and
// one "embedding.batch.subbatch" child span per sub-batch actually
// dispatched to the API (EmbedBatch splits large inputs into sub-batches
// bounded by maxBatchChars=500,000 chars — see embed.go). 17 texts of
// 30,000 characters each (510,000 chars total) are deliberately chosen to
// force a split into >=2 sub-batches: each text sits comfortably under the
// per-text truncateForEmbed ceiling (~32,000 chars for this repeated-phrase
// content at the 8,000-token cap — verified empirically against the
// embedded cl100k_base tokenizer), so truncation never alters these input
// lengths and the sub-batch boundary math stays exactly predictable.
func TestEmbedBatch_ParentAndSubBatchChildSpans(t *testing.T) {
	exp := withInMemoryTracer(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		vecs := make([][]float32, len(reqBody.Input))
		for i := range vecs {
			vecs[i] = fakeVec(4)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(embeddingResponse(vecs))
	}))
	t.Cleanup(srv.Close)

	c := NewEmbedClient(srv.URL, "sk-test", "", "text-embedding-3-small", 1536)

	unit := strings.Repeat("hello world foo bar baz qux ", 1072)[:30000]
	texts := make([]string, 17)
	for i := range texts {
		texts[i] = unit
	}

	vecs, err := c.EmbedBatch(context.Background(), texts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vecs) != len(texts) {
		t.Fatalf("EmbedBatch returned %d vectors, want %d", len(vecs), len(texts))
	}

	spans := exp.GetSpans()
	var parents, children int
	var parentSpan tracetest.SpanStub
	var subBatchSizeSum int
	for _, s := range spans {
		switch s.Name {
		case "embedding.batch":
			parents++
			parentSpan = s
		case "embedding.batch.subbatch":
			children++
			sz, _ := strconv.Atoi(attrMap(s)[telemetry.AttrEmbeddingBatchSize])
			subBatchSizeSum += sz
		}
	}

	if parents != 1 {
		t.Fatalf("expected exactly 1 parent 'embedding.batch' span, got %d (spans=%v)", parents, spanNames(spans))
	}
	if children < 2 {
		t.Fatalf("expected the 500,000-char sub-batch budget to split 17x30,000 chars into >=2 'embedding.batch.subbatch' spans, got %d (spans=%v)", children, spanNames(spans))
	}
	if subBatchSizeSum != len(texts) {
		t.Errorf("sum of embedding.batch_size across sub-batch spans = %d, want %d (total input texts)", subBatchSizeSum, len(texts))
	}

	for _, s := range spans {
		if s.Name != "embedding.batch.subbatch" {
			continue
		}
		if s.Parent.SpanID() != parentSpan.SpanContext.SpanID() {
			t.Errorf("sub-batch span has parent SpanID %s, want parent 'embedding.batch' SpanID %s",
				s.Parent.SpanID(), parentSpan.SpanContext.SpanID())
		}
		if s.Parent.TraceID() != parentSpan.SpanContext.TraceID() {
			t.Errorf("sub-batch span has TraceID %s, want parent's TraceID %s", s.Parent.TraceID(), parentSpan.SpanContext.TraceID())
		}
	}

	attrs := attrMap(parentSpan)
	if got := attrs[telemetry.AttrGenAISystem]; got != "openai" {
		t.Errorf("parent gen_ai.system = %q, want %q", got, "openai")
	}
	if got := attrs[telemetry.AttrGenAIRequestModel]; got != "text-embedding-3-small" {
		t.Errorf("parent gen_ai.request.model = %q, want %q", got, "text-embedding-3-small")
	}
}
