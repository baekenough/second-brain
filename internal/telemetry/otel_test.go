package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace/noop"
)

// resetGlobalTracerProvider restores the process-global TracerProvider to a
// no-op implementation between tests. otel.SetTracerProvider has no "unset"
// API, so tests that call InitOTel (which mutates global state) must clean up
// after themselves to avoid leaking a configured provider — and its
// background export goroutines — into unrelated tests run later in the same
// process.
func resetGlobalTracerProvider() {
	otel.SetTracerProvider(noop.NewTracerProvider())
}

// TestInitOTel_NoopWhenEndpointEmpty verifies that an empty endpoint fully
// disables tracing: the global TracerProvider becomes a no-op, span creation
// through it is harmless (never panics, never blocks), and the returned
// shutdown function succeeds immediately. This is the "no OTLP/Langfuse
// configuration present" path exercised by local dev and CI.
func TestInitOTel_NoopWhenEndpointEmpty(t *testing.T) {
	t.Cleanup(resetGlobalTracerProvider)

	shutdown, err := InitOTel(context.Background(), "", "publicKey", "secretKey")
	if err != nil {
		t.Fatalf("InitOTel() with empty endpoint returned error: %v", err)
	}
	if shutdown == nil {
		t.Fatal("InitOTel() with empty endpoint returned a nil shutdown func")
	}

	_, span := otel.Tracer("test").Start(context.Background(), "noop-span")
	if span.IsRecording() {
		t.Error("span created after InitOTel(empty endpoint) unexpectedly reports IsRecording()==true; expected a no-op span")
	}
	span.End() // must not panic

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Errorf("shutdown() after empty-endpoint InitOTel returned error: %v", err)
	}
}

// TestInitOTel_ConfiguresRealProviderWhenEndpointSet verifies that a
// non-empty endpoint configures a real (non-no-op) SDK TracerProvider that
// produces recording spans and successfully exports them with the expected
// Basic Auth header. No real Langfuse instance or external network is
// contacted: the endpoint is a local httptest.Server stub that accepts the
// OTLP/HTTP POST and returns 200.
func TestInitOTel_ConfiguresRealProviderWhenEndpointSet(t *testing.T) {
	t.Cleanup(resetGlobalTracerProvider)

	var gotAuthHeader atomic.Value // string
	var exportCalled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exportCalled.Store(true)
		gotAuthHeader.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	shutdown, err := InitOTel(context.Background(), srv.URL+"/api/public/otel", "publicKey", "secretKey")
	if err != nil {
		t.Fatalf("InitOTel() with configured endpoint returned error: %v", err)
	}
	if shutdown == nil {
		t.Fatal("InitOTel() with configured endpoint returned a nil shutdown func")
	}

	_, span := otel.Tracer("test").Start(context.Background(), "recording-span")
	if !span.IsRecording() {
		t.Error("span created after InitOTel(configured endpoint) reports IsRecording()==false; expected a real recording span")
	}
	span.End()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Errorf("shutdown() after configured InitOTel returned error: %v", err)
	}

	if !exportCalled.Load() {
		t.Error("shutdown() did not flush the buffered span to the local stub OTLP server")
	}
	wantAuth := basicAuthHeader("publicKey", "secretKey")
	if got, _ := gotAuthHeader.Load().(string); got != wantAuth {
		t.Errorf("exported span Authorization header = %q, want %q", got, wantAuth)
	}
}

// TestInitOTel_BasicAuthHeaderEncoding verifies the Basic Auth header value
// construction contract (base64(publicKey:secretKey)) in isolation, since
// InitOTel itself does not expose the underlying exporter's headers for
// direct inspection.
func TestInitOTel_BasicAuthHeaderEncoding(t *testing.T) {
	got := basicAuthHeader("pk-lf-abc", "sk-lf-xyz")
	const want = "Basic cGstbGYtYWJjOnNrLWxmLXh5eg=="
	if got != want {
		t.Errorf("basicAuthHeader() = %q, want %q", got, want)
	}
}
