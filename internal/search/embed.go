package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	tiktoken "github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/baekenough/second-brain/internal/auth"
	"github.com/baekenough/second-brain/internal/httperr"
	"github.com/baekenough/second-brain/internal/telemetry"
)

// embedTracerName is the OTel instrumentation scope name for every span
// created in this file. Looked up fresh via otel.Tracer() on each call
// rather than cached in a package-level var — see the identical rationale
// in internal/llm/client.go's tracer() helper.
const embedTracerName = "github.com/baekenough/second-brain/internal/search"

// embedGenAISystem is the gen_ai.system value for every embedding span:
// EmbedClient always speaks OpenAI's /v1/embeddings protocol (see the
// EmbedClient doc comment — embeddings are routed to OpenAI directly, never
// through the generic chat proxy).
const embedGenAISystem = "openai"

func embedTracer() oteltrace.Tracer { return otel.Tracer(embedTracerName) }

// embedRetryDelays defines the exponential backoff delays applied between
// embed request retries. Consistent with the R004 error handling policy and
// the llm.Client pattern (maxRetries=2): attempt 0 succeeds or falls through
// to attempt 1 after 1s, then attempt 2 after 2s, then attempt 3 after 4s.
var embedRetryDelays = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
}

// embedMaxRetries is the maximum number of retry attempts for transient errors
// (5xx, network failures) and 429 rate-limit responses. Must equal len(embedRetryDelays).
const embedMaxRetries = 3

// parseRetryAfter parses the Retry-After header value. It supports both the
// delay-seconds form (integer number of seconds) and the HTTP-date form.
// Returns 0 when the header is absent or cannot be parsed.
func parseRetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	// Try delay-seconds first (most common for OpenAI / 429 responses).
	if secs, err := strconv.Atoi(h); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	// Try HTTP-date form.
	if t, err := http.ParseTime(h); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 0
}

// maxEmbedTokens is the token ceiling for a single embedding input.
// OpenAI text-embedding-3-small hard limit is 8,192 tokens; we use 8,000
// to leave a small safety margin.
const maxEmbedTokens = 8_000

// embedEncoding is "cl100k_base", the BPE encoding used by
// text-embedding-3-small (and GPT-4 / GPT-3.5-turbo).
const embedEncoding = "cl100k_base"

// tokenizer holds the lazily-initialised cl100k_base encoder.
// On first use the OfflineLoader reads the BPE vocab from the embedded assets
// bundled inside tiktoken-go-loader — no network access required.
var (
	tokenizerOnce sync.Once
	tokenizer     *tiktoken.Tiktoken // nil when initialisation failed
)

// getTokenizer returns the package-level cl100k_base encoder, initialising it
// exactly once. Returns nil if initialisation fails (caller must fall back to
// the rune-based truncation).
func getTokenizer() *tiktoken.Tiktoken {
	tokenizerOnce.Do(func() {
		tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
		enc, err := tiktoken.GetEncoding(embedEncoding)
		if err != nil {
			slog.Warn("embed: failed to init tiktoken encoder; falling back to rune-based truncation",
				"encoding", embedEncoding,
				"err", err,
			)
			return
		}
		tokenizer = enc
	})
	return tokenizer
}

// maxEmbedRunesFallback is used only when the tiktoken encoder is unavailable.
// It applies the same conservative 2 chars/token estimate as the original
// implementation (8 000 tokens × 2 = 16 000 runes).
const maxEmbedRunesFallback = 16_000

// truncateForEmbed returns text truncated so that it fits within maxEmbedTokens
// tokens (cl100k_base encoding). When the tiktoken encoder is unavailable it
// falls back to rune-based truncation at maxEmbedRunesFallback.
//
// Truncation is always performed on exact token boundaries so that the decoded
// output is valid UTF-8 regardless of the input language mix.
func truncateForEmbed(text string) string {
	enc := getTokenizer()
	if enc == nil {
		// Fallback: rune-based truncation (original behaviour).
		runes := []rune(text)
		if len(runes) <= maxEmbedRunesFallback {
			return text
		}
		slog.Debug("embed: rune-based truncation (tiktoken unavailable)",
			"original_runes", len(runes),
			"truncated_runes", maxEmbedRunesFallback,
		)
		return string(runes[:maxEmbedRunesFallback])
	}

	tokens := enc.EncodeOrdinary(text)
	if len(tokens) <= maxEmbedTokens {
		return text
	}

	truncated := enc.Decode(tokens[:maxEmbedTokens])
	slog.Debug("embed: token-based truncation",
		"original_tokens", len(tokens),
		"truncated_tokens", maxEmbedTokens,
	)
	return truncated
}

// EmbedClient calls an OpenAI-compatible /v1/embeddings endpoint to produce
// vector representations of text. When apiURL is empty all methods are no-ops
// and return nil, enabling full-text-only operation.
//
// Token resolution at construction time:
//  1. apiKey non-empty  → static Bearer token
//  2. authFilePath non-empty → CliProxyAPI OAuth token (auto-refreshed with 5-min TTL)
//  3. both empty → no Authorization header sent
//
// EmbedClient satisfies the EmbeddingEngine interface.
type EmbedClient struct {
	apiURL string
	model  string
	dim    int // advisory dimension; 0 means unknown
	// requestDimensions 는 요청 본문에 실어 보낼 OpenAI `dimensions` 값이다.
	// 0 이면 필드를 싣지 않는다(모델 기본 차원). WithRequestDimensions 로 설정.
	requestDimensions int
	client            *http.Client
	tokens            auth.TokenSource // nil when no auth configured
}

// WithRequestDimensions 는 임베딩 요청에 실어 보낼 `dimensions` 값을 설정하고
// 같은 클라이언트를 돌려준다(체이닝용).
//
// 왜 생성자 파라미터가 아니라 별도 메서드인가: NewEmbedClient 의 호출 지점이
// 프로덕션 1곳 + 테스트 다수라, 파라미터를 하나 더 늘리면 임베딩과 무관한
// 테스트까지 전부 손봐야 한다. 기본 동작(필드 미전송)을 유지하는 선택적 설정
// 이므로 옵션 메서드가 더 알맞다.
//
// n 이 0 이하이거나 모델이 dimensions 를 지원하지 않으면 무시한다.
func (c *EmbedClient) WithRequestDimensions(n int) *EmbedClient {
	if n <= 0 {
		return c
	}
	if !supportsDimensionsParam(c.model) {
		slog.Warn("embed: model does not support the dimensions parameter; ignoring",
			"model", c.model,
			"requested_dimensions", n,
		)
		return c
	}
	c.requestDimensions = n
	return c
}

// supportsDimensionsParam 은 모델이 OpenAI `dimensions` 파라미터를 받는지
// 판정한다. Matryoshka 표현 학습으로 차원 축소를 지원하는 text-embedding-3
// 계열만 해당하며, 그 외 모델(ada-002, Ollama 호환 게이트웨이 등)에 보내면
// 400 으로 임베딩 경로 전체가 멈춘다.
func supportsDimensionsParam(model string) bool {
	return strings.HasPrefix(model, "text-embedding-3")
}

// newEmbedPayload 는 /v1/embeddings 요청 본문을 만든다. input 은 단건(string)
// 이거나 배치([]string)다. requestDimensions 가 0 이면 dimensions 키 자체가
// 빠지므로, 설정하지 않은 배포의 요청 본문은 기존과 바이트 단위로 같다.
func (c *EmbedClient) newEmbedPayload(input any) map[string]any {
	payload := map[string]any{
		"input": input,
		"model": c.model,
	}
	if c.requestDimensions > 0 {
		payload["dimensions"] = c.requestDimensions
	}
	return payload
}

// NewEmbedClient returns an EmbedClient. When apiURL is empty the client is
// disabled — Embed and EmbedBatch return nil results without error.
//
// Token priority: apiKey > authFilePath > no auth.
//
// As a convenience, when neither apiKey nor authFilePath is set we force the
// client into the disabled state regardless of apiURL. This lets operators
// disable embeddings by clearing EMBEDDING_API_KEY/CLIPROXY_AUTH_FILE without
// also having to override the default EMBEDDING_API_URL.
//
// dim is the advisory vector dimension (e.g. 1536 for text-embedding-3-small).
// Pass 0 when unknown.
func NewEmbedClient(apiURL, apiKey, authFilePath, model string, dim int) *EmbedClient {
	if apiKey == "" && authFilePath == "" {
		apiURL = ""
	}
	return &EmbedClient{
		apiURL: apiURL,
		model:  model,
		dim:    dim,
		client: &http.Client{Timeout: 30 * time.Second},
		tokens: auth.Resolve(apiKey, authFilePath),
	}
}

// Dimension returns the advisory vector dimension configured for this client.
// A value of 0 indicates that the dimension is unknown.
func (c *EmbedClient) Dimension() int { return c.dim }

// Enabled reports whether an embedding API is configured.
func (c *EmbedClient) Enabled() bool { return c.apiURL != "" }

// Embed returns the embedding vector for text, or nil if the client is disabled.
// text is silently truncated to maxEmbedTokens cl100k_base tokens before
// sending to the API (falls back to rune-based truncation when the offline
// encoder is unavailable).
//
// Transient errors (5xx, network failures) and 429 rate-limit responses are
// retried up to embedMaxRetries times with exponential backoff (1s/2s/4s).
// When the server returns a Retry-After header with a longer delay, that delay
// is honoured instead of the default backoff interval.
// 4xx errors other than 429 are not retried.
//
// Tracing: wrapped in a single "embedding.single" span. No span is created
// when the client is disabled (Enabled()==false) — that path is a
// configuration no-op, not an API call. err is a named return so the
// deferred finalizer can record whichever error value the retry loop's many
// return points ultimately produces, without repeating
// span.RecordError/SetStatus at each one.
func (c *EmbedClient) Embed(ctx context.Context, text string) (vec []float32, err error) {
	if !c.Enabled() {
		return nil, nil
	}

	ctx, span := embedTracer().Start(ctx, "embedding.single", oteltrace.WithAttributes(
		attribute.String(telemetry.AttrGenAISystem, embedGenAISystem),
		attribute.String(telemetry.AttrGenAIRequestModel, c.model),
	))
	defer span.End()
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
	}()

	text = truncateForEmbed(text)

	body, err := json.Marshal(c.newEmbedPayload(text))
	if err != nil {
		return nil, fmt.Errorf("embed marshal: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= embedMaxRetries; attempt++ {
		if attempt > 0 {
			delay := embedRetryDelays[attempt-1]
			slog.Warn("embed: retrying after transient error",
				"attempt", attempt,
				"max_retries", embedMaxRetries,
				"delay", delay,
				"error", lastErr,
			)
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("embed: context cancelled during retry: %w", ctx.Err())
			case <-time.After(delay):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.apiURL+"/embeddings", bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("embed build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if err := c.setAuth(req); err != nil {
			return nil, err
		}

		res, err := c.client.Do(req)
		if err != nil {
			// Network-level error — retryable.
			lastErr = fmt.Errorf("embed request: %w", err)
			continue
		}

		if res.StatusCode != http.StatusOK {
			// 비-200 본문은 오류에 싣지 않는다(#288 3항): 상태 코드와 정제한
			// error.type/code 만 남긴다. 재시도 판정은 classifyStatus 가 상태
			// 코드로만 한다.
			statusErr := httperr.ReadStatusError("embed API status", res)
			res.Body.Close()
			if retry, err := c.classifyStatus(ctx, "embed", attempt, res, statusErr); !retry {
				return nil, err
			}
			lastErr = statusErr
			continue
		}

		b, readErr := httperr.ReadBody(res.Body, c.responseLimit(1))
		res.Body.Close()
		if readErr != nil {
			if errors.Is(readErr, httperr.ErrBodyTooLarge) {
				// 상한 초과는 다시 보내도 같다 — 재시도하지 않는다.
				c.logBodyTooLarge("embed", 1)
				return nil, fmt.Errorf("embed read response: %w", readErr)
			}
			lastErr = fmt.Errorf("embed read response: %w", readErr)
			continue
		}

		var resp struct {
			Data []struct {
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}
		if err := json.Unmarshal(b, &resp); err != nil {
			return nil, fmt.Errorf("embed unmarshal: %w", err)
		}
		if len(resp.Data) == 0 || len(resp.Data[0].Embedding) == 0 {
			return nil, fmt.Errorf("embed: empty embedding in response")
		}
		return resp.Data[0].Embedding, nil
	}

	return nil, fmt.Errorf("embed: all retries exhausted: %w", lastErr)
}

// Sub-batch character budget constants.
//
// OpenAI limits each /v1/embeddings request to 300,000 tokens. We apply a
// conservative character-based estimate (1 token ≈ 2 chars) for the batch
// budget because accurate per-batch token counting would require encoding every
// text twice. The per-document truncation already guarantees each individual
// text stays within maxEmbedTokens, so the batch budget only needs to bound
// the aggregate.
//
//	safeTokenLimit  = 250,000 tokens   (leave 50k headroom below the 300k cap)
//	charsPerToken   = 2                (conservative: 1 token ≈ 2 chars)
//	maxBatchChars   = 500,000 chars    (= safeTokenLimit × charsPerToken)
const (
	safeTokenLimit = 250_000
	charsPerToken  = 2
	maxBatchChars  = safeTokenLimit * charsPerToken // 500,000 chars per sub-batch
)

// EmbedBatch generates embeddings for multiple texts. Each text is silently
// truncated to maxEmbedTokens tokens before processing.
//
// When the total character count of all texts exceeds maxBatchChars the input
// is automatically split into sub-batches, each dispatched as a separate API
// call. The resulting vectors are concatenated in the original input order
// before being returned. This prevents 400 max_tokens_per_request errors that
// occur when a large backfill batch exceeds the per-request token limit.
//
// A single item whose character count alone exceeds maxBatchChars is sent as
// its own sub-batch (the existing per-document rune truncation makes this case
// practically unreachable, but we handle it defensively).
//
// Tracing: wrapped in a top-level "embedding.batch" span, with one child
// "embedding.batch.subbatch" span per API call dispatched by embedBatchOnce
// — so a slow or failing sub-batch is attributable within the overall batch
// operation rather than only visible as aggregate latency. No span is
// created when the client is disabled.
func (c *EmbedClient) EmbedBatch(ctx context.Context, texts []string) (_ [][]float32, err error) {
	if !c.Enabled() {
		return make([][]float32, len(texts)), nil
	}

	ctx, span := embedTracer().Start(ctx, "embedding.batch", oteltrace.WithAttributes(
		attribute.String(telemetry.AttrGenAISystem, embedGenAISystem),
		attribute.String(telemetry.AttrGenAIRequestModel, c.model),
	))
	defer span.End()
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
	}()

	truncated := make([]string, len(texts))
	for i, t := range texts {
		truncated[i] = truncateForEmbed(t)
	}

	// Split into sub-batches that each stay within the character budget.
	out := make([][]float32, len(texts))
	start := 0
	for start < len(truncated) {
		end, charSum := start, 0
		for end < len(truncated) {
			n := len(truncated[end]) // byte length ≈ char length for budget purposes
			if end > start && charSum+n > maxBatchChars {
				// This item would push us over budget; flush current sub-batch.
				break
			}
			charSum += n
			end++
		}

		sub := truncated[start:end]
		vecs, err := c.embedBatchOnce(ctx, sub)
		if err != nil {
			return nil, fmt.Errorf("embed batch [%d:%d]: %w", start, end, err)
		}
		copy(out[start:], vecs)

		slog.Debug("embed: sub-batch dispatched",
			"start", start, "end", end,
			"count", len(sub), "chars", charSum,
		)
		start = end
	}
	return out, nil
}

// embedBatchOnce sends a single /v1/embeddings request for the given texts and
// returns the embedding vectors in the order returned by the API (reordered by
// the index field). Callers are responsible for ensuring the texts fit within
// the API's per-request token limit.
//
// Transient errors (5xx, network failures) and 429 rate-limit responses are
// retried up to embedMaxRetries times with exponential backoff (1s/2s/4s),
// honouring the Retry-After header when present.
//
// Tracing: each call is one "embedding.batch.subbatch" child span (nested
// under whatever span is active in ctx — EmbedBatch's parent "embedding.batch"
// span when called from there), tagged with embedding.batch_size so a
// specific sub-batch's size can be correlated with its latency/failure.
func (c *EmbedClient) embedBatchOnce(ctx context.Context, texts []string) (_ [][]float32, err error) {
	ctx, span := embedTracer().Start(ctx, "embedding.batch.subbatch", oteltrace.WithAttributes(
		attribute.String(telemetry.AttrGenAISystem, embedGenAISystem),
		attribute.String(telemetry.AttrGenAIRequestModel, c.model),
		attribute.Int(telemetry.AttrEmbeddingBatchSize, len(texts)),
	))
	defer span.End()
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
	}()

	body, err := json.Marshal(c.newEmbedPayload(texts))
	if err != nil {
		return nil, fmt.Errorf("embed batch marshal: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= embedMaxRetries; attempt++ {
		if attempt > 0 {
			delay := embedRetryDelays[attempt-1]
			slog.Warn("embed batch: retrying after transient error",
				"attempt", attempt,
				"max_retries", embedMaxRetries,
				"delay", delay,
				"error", lastErr,
			)
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("embed batch: context cancelled during retry: %w", ctx.Err())
			case <-time.After(delay):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.apiURL+"/embeddings", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if err := c.setAuth(req); err != nil {
			return nil, err
		}

		res, err := c.client.Do(req)
		if err != nil {
			// Network-level error — retryable.
			lastErr = fmt.Errorf("embed batch request: %w", err)
			continue
		}

		if res.StatusCode != http.StatusOK {
			statusErr := httperr.ReadStatusError("embed batch API status", res)
			res.Body.Close()
			if retry, err := c.classifyStatus(ctx, "embed batch", attempt, res, statusErr); !retry {
				return nil, err
			}
			lastErr = statusErr
			continue
		}

		b, readErr := httperr.ReadBody(res.Body, c.responseLimit(len(texts)))
		res.Body.Close()
		if readErr != nil {
			if errors.Is(readErr, httperr.ErrBodyTooLarge) {
				c.logBodyTooLarge("embed batch", len(texts))
				return nil, fmt.Errorf("embed batch read response: %w", readErr)
			}
			lastErr = fmt.Errorf("embed batch read response: %w", readErr)
			continue
		}

		var resp struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}
		if err := json.Unmarshal(b, &resp); err != nil {
			return nil, fmt.Errorf("embed batch unmarshal: %w", err)
		}

		out := make([][]float32, len(texts))
		for _, d := range resp.Data {
			if d.Index < len(out) {
				out[d.Index] = d.Embedding
			}
		}
		return out, nil
	}

	return nil, fmt.Errorf("embed batch: all retries exhausted: %w", lastErr)
}

// classifyStatus 는 비-200 응답의 재시도 여부를 상태 코드로만 정한다. 단건과
// 배치가 같은 규칙을 쓴다(오류 문자열에 기대지 않는다).
//
//   - 429: Retry-After 헤더(없으면 백오프 표)만큼 기다린 뒤 재시도한다.
//     마지막 시도였으면 기다리지 않고 재시도 루프가 끝나게 둔다.
//   - 5xx: 재시도한다.
//   - 그 밖(429 가 아닌 4xx 등): 재시도하지 않고 statusErr 를 돌려준다.
//
// retry=false 이면 호출자는 err 를 그대로 반환한다. 429 대기 중 ctx 가
// 끝나면 retry=false 와 취소 오류를 돌려준다. label 은 기존 로그·오류 문구
// ("embed", "embed batch")를 유지하려고 받는다. 헤더는 본문을 드레인한
// 뒤에도 그대로 읽을 수 있다.
func (c *EmbedClient) classifyStatus(ctx context.Context, label string, attempt int, res *http.Response, statusErr error) (retry bool, err error) {
	switch {
	case res.StatusCode == http.StatusTooManyRequests:
		retryAfter := parseRetryAfter(res.Header.Get("Retry-After"))
		if attempt < embedMaxRetries {
			delay := embedRetryDelays[attempt]
			if retryAfter > delay {
				delay = retryAfter
			}
			slog.Warn(label+": rate limited (429), backing off",
				"attempt", attempt,
				"delay", delay,
				"retry_after_header", res.Header.Get("Retry-After"),
			)
			select {
			case <-ctx.Done():
				return false, fmt.Errorf("%s: context cancelled during 429 backoff: %w", label, ctx.Err())
			case <-time.After(delay):
			}
		}
		return true, nil
	case res.StatusCode >= 500:
		return true, nil
	default:
		return false, statusErr
	}
}

// embedResponseDefaultDims 는 차원을 모를 때 응답 상한 계산에 쓰는 값이다.
// 이 저장소가 쓰는 가장 큰 모델(text-embedding-3-large)의 기본 차원이다.
const embedResponseDefaultDims = 3072

// embedResponseBytesPerFloat 는 JSON 으로 적힌 float 하나의 바이트 상한 추정치다.
//
// OpenAI 응답은 들여쓰기한 JSON 이고 float 하나를 한 줄에 적는다. float32
// 값을 float64 최단 표기로 적으면 약 20자(예: -0.018034566193819046)이고,
// 여기에 들여쓰기·쉼표·개행이 붙는다. 실측(정규화한 3072차원, 2칸
// 들여쓰기)은 약 30.4B/float 였다. 32 로 두면 정상 응답이 상한의 94%(CRLF
// 97%)이고 4칸 들여쓰기면 넘는다. 상한 초과는 재시도하지 않으므로 백필이
// 매 틱 실패한다. 그래서 4칸 들여쓰기 + CRLF(약 40B)까지 덮도록 48 로 잡았다.
const embedResponseBytesPerFloat = 48

// embedResponseOverhead 는 벡터 외의 응답 부분(object·model·usage 와 항목별
// index 키)을 덮는 여유분이다.
const embedResponseOverhead = 1 << 20

// responseLimit 은 입력 n 개에 대한 성공 응답 본문의 바이트 상한이다.
//
// 배치는 문자 수(maxBatchChars)로만 나누므로 입력 개수에 고정 상한이 없다.
// OpenAI 최대 입력 2048개 × 1536차원이면 응답이 약 63MB 가 된다. 고정
// 상한은 너무 크거나(방어 효과 없음) 너무 작아(정상 배치 거부) 입력 개수와
// 차원으로 계산한다. 차원은 요청에 싣는 dimensions → 설정된 dim →
// embedResponseDefaultDims 순으로 쓴다.
func (c *EmbedClient) responseLimit(n int) int64 {
	if n < 1 {
		n = 1
	}
	return int64(n)*int64(c.responseDims())*embedResponseBytesPerFloat + embedResponseOverhead
}

// responseDims 는 응답 상한 계산에 쓰는 차원이다.
func (c *EmbedClient) responseDims() int {
	if c.requestDimensions > 0 {
		return c.requestDimensions
	}
	if c.dim > 0 {
		return c.dim
	}
	return embedResponseDefaultDims
}

// logBodyTooLarge 는 성공 응답이 상한을 넘었을 때 상한 계산의 입력(limit·n·
// dims)만 남긴다. 본문은 남기지 않는다. 운영자가 상한이 정상 응답을 자른
// 것인지(상수 조정 필요) 고장 난 응답인지 가를 수 있게 한다.
func (c *EmbedClient) logBodyTooLarge(label string, n int) {
	slog.Warn(label+": response body exceeds limit",
		"limit", c.responseLimit(n),
		"n", n,
		"dims", c.responseDims(),
	)
}

// setAuth attaches the Authorization header when a token source is configured.
func (c *EmbedClient) setAuth(req *http.Request) error {
	if c.tokens == nil {
		return nil
	}
	tok, err := c.tokens.Token()
	if err != nil {
		return fmt.Errorf("embed auth token: %w", err)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return nil
}
