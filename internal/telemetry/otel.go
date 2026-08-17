// Package telemetry configures OpenTelemetry tracing for the second-brain
// backend and exports spans to a self-hosted Langfuse instance over
// OTLP/HTTP.
//
// Langfuse does not publish a Go SDK — only Python/JS/TS. The documented
// integration path for other languages is the standard OpenTelemetry SDK
// talking OTLP/HTTP directly to Langfuse's OTLP receiver
// (POST {endpoint}/api/public/otel), authenticated with HTTP Basic Auth
// (base64(publicKey:secretKey)). This package implements exactly that path;
// it intentionally does not depend on any Langfuse-specific client library.
//
// Non-blocking guarantee: this package NEVER calls
// sdktrace.WithBlocking(). Spans are exported via a BatchSpanProcessor with
// a bounded export timeout, so a dead or unreachable Langfuse collector can
// only ever cause buffered spans to be dropped once the queue fills — it can
// never stall or fail application request handling.
package telemetry

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// ServiceName identifies this process in the OTel resource `service.name`
// attribute and is the value Langfuse groups traces by when multiple
// services (server, collector, eval runner) export to the same project.
const ServiceName = "second-brain"

// batchTimeout and exportTimeout are fixed per the plan's non-blocking
// requirement (see package doc): a short export timeout bounds how long a
// stalled collector can hold buffered spans before they are dropped, and is
// intentionally NOT configurable — making it operator-tunable would invite
// accidentally reintroducing blocking behavior via a very large value.
const (
	batchTimeout  = 5 * time.Second
	exportTimeout = 3 * time.Second
)

// InitOTel configures the process-global OpenTelemetry TracerProvider and
// returns a shutdown function the caller must defer.
//
// When endpoint is empty, tracing is fully disabled: the global
// TracerProvider is set to a no-op implementation (noop.NewTracerProvider)
// so every otel.Tracer(...).Start(...) call anywhere in the codebase becomes
// a safe, side-effect-free no-op. This is the default (unconfigured)
// behavior in local dev and CI, where LANGFUSE_OTLP_ENDPOINT is unset.
//
// When endpoint is non-empty, spans are batched (sdktrace.BatchSpanProcessor
// with WithBatchTimeout(5s)/WithExportTimeout(3s) — see package doc for why
// WithBlocking() is never used) and exported over OTLP/HTTP to endpoint,
// authenticated via HTTP Basic Auth using publicKey/secretKey (Langfuse's
// documented OTLP ingestion contract). endpoint is used verbatim as the
// full OTLP traces URL — it is NOT treated as a bare host that gets
// "/v1/traces" appended, which is otlptracehttp's default behavior for
// WithEndpoint (as opposed to WithEndpointURL, which this function uses).
//
// Example endpoint for this deployment: "http://100.77.20.12:3300/api/public/otel"
// (the langfuse-web Tailscale-interface binding, port 3300 — see the ops
// repo's Langfuse compose). Do NOT use the public hostname
// (https://langfuse.<domain>) here: that hostname sits behind Cloudflare
// Access, so a server-to-server OTLP POST with no browser session gets a
// silent 302-to-login instead of a real response, and the span is dropped
// with no error surfaced anywhere — the failure is invisible unless someone
// checks the Langfuse UI and wonders why it's empty.
//
// The returned shutdown function flushes any buffered spans and releases the
// exporter. Callers MUST wrap invocation of the returned func with a short
// timeout (e.g. 5s) — see package doc: a dead collector must never block
// process shutdown.
func InitOTel(ctx context.Context, endpoint, publicKey, secretKey string) (func(context.Context) error, error) {
	if endpoint == "" {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return func(context.Context) error { return nil }, nil
	}

	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(endpoint),
		otlptracehttp.WithHeaders(map[string]string{
			"Authorization": basicAuthHeader(publicKey, secretKey),
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: create OTLP HTTP exporter: %w", err)
	}

	res := resource.NewSchemaless(
		// Plain "service.name" key rather than a versioned semconv import —
		// this single attribute is stable across every semconv revision, so
		// pinning a semconv package version purely for this one key is not
		// worth the added dependency surface (see attrs.go doc comment for
		// the fuller rationale, which applies to the GenAI attribute keys).
		serviceNameAttr(ServiceName),
	)

	bsp := sdktrace.NewBatchSpanProcessor(exp,
		sdktrace.WithBatchTimeout(batchTimeout),
		sdktrace.WithExportTimeout(exportTimeout),
		// WithBlocking() is deliberately never called — see package doc.
	)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(bsp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	return tp.Shutdown, nil
}

// basicAuthHeader returns the value of an HTTP Authorization header
// implementing Basic Auth for (publicKey, secretKey), matching Langfuse's
// documented OTLP ingestion contract: "Basic " + base64("publicKey:secretKey").
func basicAuthHeader(publicKey, secretKey string) string {
	raw := publicKey + ":" + secretKey
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(raw))
}
