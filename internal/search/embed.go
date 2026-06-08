package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	tiktoken "github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"

	"github.com/baekenough/second-brain/internal/auth"
)

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
	dim    int    // advisory dimension; 0 means unknown
	client *http.Client
	tokens auth.TokenSource // nil when no auth configured
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
func (c *EmbedClient) Embed(ctx context.Context, text string) ([]float32, error) {
	if !c.Enabled() {
		return nil, nil
	}

	text = truncateForEmbed(text)

	payload := map[string]interface{}{
		"input": text,
		"model": c.model,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("embed marshal: %w", err)
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
		return nil, fmt.Errorf("embed request: %w", err)
	}
	defer res.Body.Close()

	b, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("embed read response: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed API status %d: %s", res.StatusCode, b)
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

// Sub-batch character budget constants.
//
// OpenAI limits each /v1/embeddings request to 300,000 tokens. We apply a
// conservative character-based estimate (1 token ≈ 2 chars) for the batch
// budget because accurate per-batch token counting would require encoding every
// text twice. The per-document truncation already guarantees each individual
// text stays within maxEmbedTokens, so the batch budget only needs to bound
// the aggregate.
//
//   safeTokenLimit  = 250,000 tokens   (leave 50k headroom below the 300k cap)
//   charsPerToken   = 2                (conservative: 1 token ≈ 2 chars)
//   maxBatchChars   = 500,000 chars    (= safeTokenLimit × charsPerToken)
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
func (c *EmbedClient) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if !c.Enabled() {
		return make([][]float32, len(texts)), nil
	}

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
func (c *EmbedClient) embedBatchOnce(ctx context.Context, texts []string) ([][]float32, error) {
	payload := map[string]interface{}{
		"input": texts,
		"model": c.model,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("embed batch marshal: %w", err)
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
		return nil, fmt.Errorf("embed batch request: %w", err)
	}
	defer res.Body.Close()

	b, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed batch API status %d: %s", res.StatusCode, b)
	}

	var resp struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, err
	}

	out := make([][]float32, len(texts))
	for _, d := range resp.Data {
		if d.Index < len(out) {
			out[d.Index] = d.Embedding
		}
	}
	return out, nil
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
