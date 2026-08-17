// Package llm provides an OpenAI-compatible chat completion client.
// It is used by the Discord gateway to generate RAG-based answers
// from retrieved context documents.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/baekenough/second-brain/internal/auth"
	"github.com/baekenough/second-brain/internal/telemetry"
)

// tracerName is the OTel instrumentation scope name for every span created
// in this package (span names below distinguish operations within it).
// otel.Tracer(tracerName) is looked up fresh on every call rather than
// cached in a package-level var: the global TracerProvider it resolves
// against is only guaranteed to reflect internal/telemetry.InitOTel's
// configuration for lookups performed AFTER InitOTel runs (see
// go.opentelemetry.io/otel's global-provider delegation semantics), and
// tests reconfigure the global provider per-test.
const tracerName = "github.com/baekenough/second-brain/internal/llm"

// genAISystem is the value used for the gen_ai.system span attribute on
// every span in this package. This client speaks the OpenAI-compatible
// /v1/chat/completions protocol (OpenAI, Azure OpenAI, or a local proxy such
// as cliproxy — see the Client doc comment) and Client itself has no signal
// distinguishing which backend a given baseURL actually points at, so
// "openai" is used uniformly as the protocol-family identifier. This keeps
// the attribute populated (Langfuse groups/prices by it) rather than
// omitting it; it does not claim the traffic is literally served by OpenAI.
const genAISystem = "openai"

func tracer() oteltrace.Tracer { return otel.Tracer(tracerName) }

// Client is an OpenAI-compatible chat completion client.
// It communicates with any endpoint that speaks the /v1/chat/completions protocol
// (OpenAI, Azure OpenAI, local proxies such as cliproxy, etc.).
type Client struct {
	baseURL     string
	model       string
	tokens      auth.TokenSource // nil when no auth configured
	maxTokens   int
	temperature float64
	// thinking is the value serialised into the request's "thinking" field.
	// nil means the field is omitted entirely from the request body (see
	// Config.Thinking).
	thinking   *thinkingParam
	httpClient *http.Client
}

// Thinking modes accepted by Config.Thinking.
//
// Reasoning models (e.g. deepseek-v4-flash) charge reasoning_content against
// the same max_tokens budget as the visible completion. On inputs whose
// reasoning runs long the budget is consumed entirely by reasoning and the
// response comes back with zero-length content and finish_reason="length" —
// measured on a 2,768-character document: max_tokens=8000 produced 25,388
// reasoning characters and 0 content characters. Disabling reasoning on the
// same document produced a complete answer in 750 completion tokens.
//
// Both users of this client (the summarizer and the extraction worker) do
// mechanical structured-output work, so ThinkingDisabled is the default.
const (
	// ThinkingDisabled sends "thinking": {"type": "disabled"}.
	ThinkingDisabled = "disabled"
	// ThinkingEnabled omits the "thinking" field entirely, leaving the
	// endpoint's own default in force. The field is omitted rather than sent
	// as {"type":"enabled"} because OpenAI-compatible endpoints that do not
	// implement this vendor parameter reject unknown fields with HTTP 400.
	ThinkingEnabled = "enabled"
)

// Config holds the parameters required to construct a Client.
//
// Token resolution order:
//  1. APIKey non-empty → static Bearer token
//  2. AuthFile non-empty → CliProxyAPI OAuth token (auto-refreshed from disk, 5-min TTL)
//  3. both empty → no Authorization header sent
type Config struct {
	BaseURL     string
	Model       string
	APIKey      string // static Bearer token (overrides AuthFile)
	AuthFile    string // path to CliProxyAPI OAuth JSON (e.g. ~/.cli-proxy-api/user.json)
	MaxTokens   int
	Temperature float64
	// Timeout is the per-request HTTP timeout for LLM calls.
	// When zero or negative, the default of 60 s is used.
	// Increase for slow local CPU inference (e.g. gemma3:4b) by setting
	// LLM_TIMEOUT_SECONDS in the environment.
	Timeout time.Duration
	// Thinking controls the vendor "thinking" request parameter:
	// ThinkingDisabled (or "", the zero value) sends
	// "thinking": {"type": "disabled"}; ThinkingEnabled omits the field.
	// Any other value is treated as ThinkingDisabled. Sourced from the
	// LLM_THINKING environment variable via config.Config.LLMThinking.
	Thinking string
}

// New returns a Client configured with the given Config.
// httpClient may be nil — an http.Client with cfg.Timeout (default 60 s) is used.
// When cfg.Timeout is positive it overrides the default. Pass a pre-built
// httpClient to take full control of transport settings.
func New(cfg Config, httpClient *http.Client) *Client {
	if httpClient == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 60 * time.Second
		}
		httpClient = &http.Client{Timeout: timeout}
	}
	// Normalise base URL: strip trailing slash.
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	return &Client{
		baseURL:     baseURL,
		model:       cfg.Model,
		tokens:      auth.Resolve(cfg.APIKey, cfg.AuthFile),
		maxTokens:   cfg.MaxTokens,
		temperature: cfg.Temperature,
		thinking:    thinkingFor(cfg.Thinking),
		httpClient:  httpClient,
	}
}

// thinkingFor maps a Config.Thinking value to the request field value.
// Returning nil means "omit the field" — see the ThinkingEnabled doc comment
// for why enabling is expressed as an omission.
func thinkingFor(mode string) *thinkingParam {
	if strings.EqualFold(strings.TrimSpace(mode), ThinkingEnabled) {
		return nil
	}
	return &thinkingParam{Type: ThinkingDisabled}
}

// Enabled reports whether the client has the minimum required configuration
// to make a request (non-empty base URL, model, and a token source configured).
func (c *Client) Enabled() bool {
	return c.baseURL != "" && c.model != "" && c.tokens != nil
}

// Completer is the minimal interface for LLM chat completion.
// It is satisfied by *Client and can be used by callers that need to
// inject a mock or alternative implementation for testing.
type Completer interface {
	Enabled() bool
	CompleteWithMessages(ctx context.Context, system string, messages []Message) (string, error)
}

// Message is a single chat message in the OpenAI format.
// It is exported so callers can build multi-turn conversation histories
// for CompleteWithMessages.
type Message struct {
	Role    string `json:"role"` // "system", "user", or "assistant"
	Content string `json:"content"`
}

// StreamCompleter is implemented by LLM clients that support token-by-token
// streaming. It is intentionally NOT merged into Completer — existing fake
// Completer implementations across internal/curation, internal/collector,
// internal/search/hyde.go, internal/worker/{summarizer,note_enrichment_worker,
// entity_extractor}.go would all break if Completer grew a new required
// method. Callers that need streaming type-assert:
//
//	if sc, ok := completer.(llm.StreamCompleter); ok { ... } else { /* fallback */ }
type StreamCompleter interface {
	// StreamWithMessages sends a multi-turn chat completion request with
	// stream: true. onDelta is called once per received content delta
	// (never called with an empty string). Returns when the stream ends
	// (a literal "data: [DONE]" line) or ctx is cancelled/times out.
	StreamWithMessages(ctx context.Context, system string, messages []Message, onDelta func(string)) error
}

// chatRequest is the request body for POST /v1/chat/completions.
type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens"`
	Stream      bool      `json:"stream,omitempty"`
	// Thinking is a POINTER with omitempty on purpose: encoding/json's
	// omitempty does not treat an empty struct as empty, so a value type
	// would serialise "thinking":{"type":""} on every request and any
	// OpenAI-compatible endpoint that does not implement this vendor
	// parameter would answer HTTP 400. nil omits the key entirely.
	Thinking *thinkingParam `json:"thinking,omitempty"`
}

// thinkingParam is the vendor "thinking" request object, e.g.
// {"type": "disabled"}.
type thinkingParam struct {
	Type string `json:"type"`
}

// finishReasonLength is the finish_reason a provider returns when generation
// stopped because the max_tokens budget ran out.
const finishReasonLength = "length"

// ErrTruncated is the sentinel behind every *TruncatedError, so callers can
// branch with errors.Is without depending on the struct type.
var ErrTruncated = errors.New("llm: response truncated by token budget")

// TruncatedError reports that generation stopped on the token budget
// (finish_reason="length") rather than at a natural stop. Both variants are
// errors:
//
//   - empty content: the whole budget was spent on reasoning_content, and the
//     caller would otherwise see only a confusing "cannot parse empty JSON".
//   - non-empty content: a partial completion. Passing it on as success
//     silently loses data (half-parsed entities, half-written summaries).
//
// The response body is deliberately NOT a field of this error and never
// appears in Error(): completions contain personal data (issue #194).
type TruncatedError struct {
	// FinishReason is the raw finish_reason reported by the provider.
	FinishReason string
	// CompletionTokens is the provider-reported completion token count, or 0
	// when the response carried no usage block (streaming responses usually
	// do not).
	CompletionTokens int
	// ContentLength is the byte length of the content that was produced —
	// a length only, never the content itself.
	ContentLength int
}

func (e *TruncatedError) Error() string {
	return fmt.Sprintf(
		"llm: generation stopped on the token budget (finish_reason=%q, completion_tokens=%d, content_length=%d); raise LLM_MAX_TOKENS or set LLM_THINKING=disabled",
		e.FinishReason, e.CompletionTokens, e.ContentLength,
	)
}

// Unwrap makes errors.Is(err, ErrTruncated) hold.
func (e *TruncatedError) Unwrap() error { return ErrTruncated }

// streamChunk is a single OpenAI-compatible streaming SSE data payload:
// {"choices":[{"delta":{"content":"..."},"finish_reason":null}]}
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// chatResponse is the relevant subset of the OpenAI chat completion response.
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		// FinishReason is absent on some OpenAI-compatible proxies; an empty
		// value is treated as "no signal" and never fails the request.
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	// Usage is optional: some OpenAI-compatible proxies omit it. A nil Usage
	// (or a struct that never unmarshals because the field is entirely
	// absent) simply means no gen_ai.usage.* span attributes are recorded
	// for that call — see doRequest.
	Usage *tokenUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// tokenUsage is the OpenAI chat completions "usage" object, parsed so its
// fields can be attached to spans as gen_ai.usage.input_tokens /
// gen_ai.usage.output_tokens (OpenTelemetry GenAI semantic conventions).
type tokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Complete sends a single-turn chat completion request.
// system is the system prompt; user is the user turn content.
// 4xx responses are not retried. 5xx and network errors are retried up to 2 times.
func (c *Client) Complete(ctx context.Context, system, user string) (string, error) {
	return c.CompleteWithMessages(ctx, system, []Message{
		{Role: "user", Content: user},
	})
}

// CompleteWithMessages sends a multi-turn chat completion request.
// system is the system prompt; messages is the ordered conversation history
// including the final user turn. The system message is always prepended.
// 4xx responses are not retried. 5xx and network errors are retried up to 2 times.
//
// Tracing: the entire retry loop is wrapped in a single parent span
// ("llm.complete") so that retry-induced latency and eventual failure are
// visible as one logical operation, while each individual HTTP attempt gets
// its own child span ("llm.complete.attempt", attribute llm.retry.attempt).
// This mirrors the plan's explicit requirement — a lone "one span per call"
// design would hide how much of the total latency/failure came from
// retries.
func (c *Client) CompleteWithMessages(ctx context.Context, system string, messages []Message) (string, error) {
	if !c.Enabled() {
		return "", fmt.Errorf("llm: client is not configured (missing base URL or model)")
	}

	ctx, span := tracer().Start(ctx, "llm.complete", oteltrace.WithAttributes(
		attribute.String(telemetry.AttrGenAISystem, genAISystem),
		attribute.String(telemetry.AttrGenAIRequestModel, c.model),
		attribute.Int(telemetry.AttrGenAIRequestMaxTokens, c.maxTokens),
	))
	defer span.End()

	allMessages := make([]Message, 0, len(messages)+1)
	allMessages = append(allMessages, Message{Role: "system", Content: system})
	allMessages = append(allMessages, messages...)

	reqBody := chatRequest{
		Model:       c.model,
		Messages:    allMessages,
		Temperature: c.temperature,
		MaxTokens:   c.maxTokens,
		Thinking:    c.thinking,
	}

	const maxRetries = 2
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		result, usage, err := c.doRequestTraced(ctx, reqBody, attempt)
		if err == nil {
			if usage != nil {
				span.SetAttributes(
					attribute.Int(telemetry.AttrGenAIUsageInputTokens, usage.PromptTokens),
					attribute.Int(telemetry.AttrGenAIUsageOutputTokens, usage.CompletionTokens),
				)
			}
			return result, nil
		}
		if isClientError(err) || errors.Is(err, ErrTruncated) {
			// 4xx — do not retry.
			//
			// Truncation is not retried either: the same prompt against the
			// same budget deterministically exhausts it again, so retrying
			// only bills three full max_tokens generations for one failure.
			// The error tells the operator which knob to turn instead.
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return "", err
		}
		lastErr = err
		slog.Warn("llm: request failed, will retry",
			"attempt", attempt+1,
			"max_retries", maxRetries,
			"error", err,
		)
	}
	finalErr := fmt.Errorf("llm: all retries exhausted: %w", lastErr)
	span.RecordError(finalErr)
	span.SetStatus(codes.Error, finalErr.Error())
	return "", finalErr
}

// doRequestTraced wraps a single doRequest attempt in a child span, recorded
// under whatever span is already active in ctx (CompleteWithMessages' parent
// "llm.complete" span). attempt is the zero-based retry index, recorded as
// the llm.retry.attempt attribute so a specific attempt's latency/failure
// can be correlated with its position in the retry sequence.
func (c *Client) doRequestTraced(ctx context.Context, reqBody chatRequest, attempt int) (string, *tokenUsage, error) {
	ctx, span := tracer().Start(ctx, "llm.complete.attempt", oteltrace.WithAttributes(
		attribute.Int(telemetry.AttrLLMRetryAttempt, attempt),
	))
	defer span.End()

	result, usage, err := c.doRequest(ctx, reqBody)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", nil, err
	}
	if usage != nil {
		span.SetAttributes(
			attribute.Int(telemetry.AttrGenAIUsageInputTokens, usage.PromptTokens),
			attribute.Int(telemetry.AttrGenAIUsageOutputTokens, usage.CompletionTokens),
		)
	}
	return result, usage, nil
}

// StreamWithMessages sends a multi-turn chat completion request with
// stream: true and invokes onDelta once per received content delta.
//
// Unlike CompleteWithMessages, a failed request is never retried: once the
// HTTP response body starts streaming, retrying risks emitting duplicate
// tokens to a caller that has already forwarded earlier deltas downstream
// (e.g. to an SSE client). A 4xx/5xx status returned before any body is
// read is reported as a plain error with onDelta never called.
//
// Tracing: the whole call is wrapped in a single "llm.stream" span (no
// retry, so no attempt/parent split is needed — see CompleteWithMessages).
// err is a named return so the deferred span finalizer can record whatever
// error value the function ultimately returns, from any of its several
// return points, without duplicating span.RecordError/SetStatus calls at
// each one.
func (c *Client) StreamWithMessages(ctx context.Context, system string, messages []Message, onDelta func(string)) (err error) {
	if !c.Enabled() {
		return fmt.Errorf("llm: client is not configured (missing base URL or model)")
	}

	ctx, span := tracer().Start(ctx, "llm.stream", oteltrace.WithAttributes(
		attribute.String(telemetry.AttrGenAISystem, genAISystem),
		attribute.String(telemetry.AttrGenAIRequestModel, c.model),
		attribute.Int(telemetry.AttrGenAIRequestMaxTokens, c.maxTokens),
	))
	defer span.End()
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
	}()

	allMessages := make([]Message, 0, len(messages)+1)
	allMessages = append(allMessages, Message{Role: "system", Content: system})
	allMessages = append(allMessages, messages...)

	reqBody := chatRequest{
		Model:       c.model,
		Messages:    allMessages,
		Temperature: c.temperature,
		MaxTokens:   c.maxTokens,
		Stream:      true,
		Thinking:    c.thinking,
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("llm: marshal request: %w", err)
	}

	url := c.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.tokens != nil {
		tok, err := c.tokens.Token()
		if err != nil {
			return fmt.Errorf("llm: token source: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("llm: HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return &clientError{statusCode: resp.StatusCode, body: string(body)}
	}

	// finishReason keeps the last non-empty finish_reason seen on the stream:
	// providers report it on the final chunk (often one carrying no content),
	// and it is the only signal distinguishing a complete answer from one cut
	// off by the token budget. Discarding it is what made budget exhaustion
	// look like a JSON parse failure to callers.
	var finishReason string
	// contentLen accumulates delivered delta bytes so a truncation error can
	// report a length without retaining the content itself.
	var contentLen int
	var sawDone bool

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			sawDone = true
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// Skip a single malformed chunk rather than aborting a stream
			// that has already delivered good tokens to the caller.
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if fr := chunk.Choices[0].FinishReason; fr != nil && *fr != "" {
			finishReason = *fr
		}
		if content := chunk.Choices[0].Delta.Content; content != "" {
			contentLen += len(content)
			onDelta(content)
		}
	}

	if !sawDone {
		// Prefer ctx.Err() over the raw scanner error: when the caller cancels
		// (client disconnect, timeout), the underlying transport error message
		// varies by platform/Go version, but the caller only needs to reliably
		// detect "this was a cancellation" via errors.Is(err, context.Canceled).
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("llm: stream canceled: %w", ctxErr)
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("llm: read stream: %w", err)
		}
	}

	// Same verdict as the non-streaming path. Deltas already handed to
	// onDelta are NOT retracted — the caller has typically forwarded them
	// downstream already — but the call still fails so the caller does not
	// treat a cut-off answer as complete. Streaming responses carry no usage
	// block, hence CompletionTokens is left at 0.
	if finishReason == finishReasonLength {
		return &TruncatedError{
			FinishReason:  finishReason,
			ContentLength: contentLen,
		}
	}
	return nil
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
// doRequest performs one HTTP round-trip and returns the completion text
// plus, when the response body includes an OpenAI-format "usage" object, the
// token counts it reported (nil when absent — some OpenAI-compatible proxies
// omit it). The usage return value feeds the gen_ai.usage.* span attributes
// set by doRequestTraced/CompleteWithMessages.
func (c *Client) doRequest(ctx context.Context, reqBody chatRequest) (string, *tokenUsage, error) {
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", nil, fmt.Errorf("llm: marshal request: %w", err)
	}

	url := c.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", nil, fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.tokens != nil {
		tok, err := c.tokens.Token()
		if err != nil {
			return "", nil, fmt.Errorf("llm: token source: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("llm: HTTP request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, fmt.Errorf("llm: read response body: %w", err)
	}

	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return "", nil, &clientError{statusCode: resp.StatusCode, body: string(body)}
	}
	if resp.StatusCode >= 500 {
		return "", nil, fmt.Errorf("llm: server error %d: %s", resp.StatusCode, string(body))
	}

	var chatResp chatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return "", nil, fmt.Errorf("llm: unmarshal response: %w", err)
	}

	// Surface API-level errors embedded in a 200 response (some proxies do this).
	if chatResp.Error != nil {
		return "", nil, &clientError{
			statusCode: http.StatusOK,
			body:       chatResp.Error.Message,
		}
	}

	if len(chatResp.Choices) == 0 {
		return "", nil, fmt.Errorf("llm: response contains no choices")
	}

	choice := chatResp.Choices[0]

	// A budget-truncated completion is an error, whether the content is empty
	// (reasoning ate the budget) or partial (silently lossy). Reported before
	// returning any content so no caller can accidentally parse half a JSON
	// document as a complete one.
	if choice.FinishReason == finishReasonLength {
		completionTokens := 0
		if chatResp.Usage != nil {
			completionTokens = chatResp.Usage.CompletionTokens
		}
		return "", chatResp.Usage, &TruncatedError{
			FinishReason:     choice.FinishReason,
			CompletionTokens: completionTokens,
			ContentLength:    len(choice.Message.Content),
		}
	}

	return choice.Message.Content, chatResp.Usage, nil
}
