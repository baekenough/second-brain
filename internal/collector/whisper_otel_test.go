package collector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/telemetry"
)

// These tests exercise the OTel span structure of WhisperCollector's
// transcription calls and are intentionally NOT run in parallel (no
// t.Parallel()): they mutate the process-global OTel TracerProvider via
// otel.SetTracerProvider, which the rest of this package's tests never
// touch. Go's test runner completes all non-parallel tests — including
// these — before resuming any test that called t.Parallel(), so there is no
// overlap with this package's other (parallel) tests despite the shared
// global state. See the identical pattern in
// internal/llm/client_otel_test.go and internal/search/embed_otel_test.go.
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

func attrMap(s tracetest.SpanStub) map[string]string {
	out := make(map[string]string, len(s.Attributes))
	for _, kv := range s.Attributes {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

// TestWhisperGenAISystem_LocalVsCloud verifies the pure gen_ai.system value
// selection in isolation (no network involved): local whisper.cpp-style
// endpoints and OpenAI's cloud API are tagged differently so a Langfuse
// dashboard can distinguish them, per Task 4(a) of the observability plan
// ("which provider is failing").
func TestWhisperGenAISystem_LocalVsCloud(t *testing.T) {
	if got, want := whisperGenAISystem(true), "whisper-self-hosted"; got != want {
		t.Errorf("whisperGenAISystem(true) = %q, want %q", got, want)
	}
	if got, want := whisperGenAISystem(false), "openai"; got != want {
		t.Errorf("whisperGenAISystem(false) = %q, want %q", got, want)
	}
}

// TestWhisperCollector_Collect_TranscriptionSpanCreated verifies that each
// transcribeFile call (invoked per-worker inside the CollectStream worker
// pool — see buildDocument) produces exactly one "transcription" span
// carrying the GenAI attributes plus audio.size_bytes. httptest.Server binds
// to 127.0.0.1, so isLocalWhisperEndpoint classifies it as local — the
// "whisper-self-hosted" branch of whisperGenAISystem is what this
// integration path naturally exercises (the "openai"/cloud branch is
// covered directly by TestWhisperGenAISystem_LocalVsCloud above, since
// reaching a genuinely non-local host from a test would require real
// network access, which these tests must not do).
func TestWhisperCollector_Collect_TranscriptionSpanCreated(t *testing.T) {
	exp := withInMemoryTracer(t)

	dir := t.TempDir()
	srv, _ := newWhisperTestServer(t, "안녕하세요")

	mtime := time.Now().Add(-1 * time.Hour).UTC().Truncate(time.Second)
	audioPath := writeDummyAudio(t, dir, "call.m4a", mtime)
	audioInfo, err := os.Stat(audioPath)
	if err != nil {
		t.Fatalf("stat audio file: %v", err)
	}
	audioSize := audioInfo.Size()

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "gpt-4o-transcribe-diarize",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Collect() returned %d docs, want 1", len(docs))
	}

	spans := exp.GetSpans()
	var found int
	var span tracetest.SpanStub
	for _, s := range spans {
		if s.Name == "transcription" {
			found++
			span = s
		}
	}
	if found != 1 {
		t.Fatalf("expected exactly 1 'transcription' span, got %d", found)
	}

	attrs := attrMap(span)
	if got, want := attrs[telemetry.AttrGenAISystem], "whisper-self-hosted"; got != want {
		t.Errorf("gen_ai.system = %q, want %q (httptest server is loopback → local)", got, want)
	}
	if got, want := attrs[telemetry.AttrGenAIRequestModel], "gpt-4o-transcribe-diarize"; got != want {
		t.Errorf("gen_ai.request.model = %q, want %q", got, want)
	}
	if got, want := attrs[telemetry.AttrAudioSizeBytes], strconv.FormatInt(audioSize, 10); got != want {
		t.Errorf("audio.size_bytes = %q, want %q", got, want)
	}
}

// TestWhisperCollector_Collect_TranscriptionSpanRecordsErrorOnFailure
// verifies that a failed transcription (partial-success skip at the
// buildDocument level — see its doc comment) still leaves behind a
// "transcription" span with the error recorded, so failed calls remain
// visible in Langfuse even though they never produce a Document.
func TestWhisperCollector_Collect_TranscriptionSpanRecordsErrorOnFailure(t *testing.T) {
	exp := withInMemoryTracer(t)

	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	mtime := time.Now().Add(-1 * time.Hour).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "call.m4a", mtime)

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "gpt-4o-transcribe-diarize",
	}
	c := makeWhisperCollector(cfg, srv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("Collect() returned %d docs, want 0 (transcription failure is partial-success skip)", len(docs))
	}

	spans := exp.GetSpans()
	var span tracetest.SpanStub
	found := false
	for _, s := range spans {
		if s.Name == "transcription" {
			span = s
			found = true
		}
	}
	if !found {
		t.Fatal("expected a 'transcription' span even on failure")
	}
	if len(span.Events) == 0 {
		t.Error("expected span.RecordError to add an exception event, got no events")
	}
}
