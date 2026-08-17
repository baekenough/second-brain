package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
)

// AskRequest is the JSON body accepted by POST /api/v1/ask.
type AskRequest struct {
	Question string `json:"question"`
}

// AskSourceItem mirrors web/src/lib/types.ts:122-127 EXACTLY — field names
// and JSON types are a wire contract with the already-shipped frontend, not
// free to rename. id is mandatory: it is the only future path to
// (query, document_id, thumbs) feedback labels (see feedback.go's
// FeedbackRequest.DocumentID).
type AskSourceItem struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	SourceType string  `json:"source_type"`
	Score      float64 `json:"score"`
}

// askSourcesPayload is the "sources" SSE event body (spec §5.1).
type askSourcesPayload struct {
	Sources []AskSourceItem `json:"sources"`
}

// askTokenPayload is the "token" SSE event body (spec §5.1).
type askTokenPayload struct {
	Text string `json:"text"`
}

// askDonePayload is the "done" SSE event body (spec §5.1).
// FinishReason is one of "stop" | "error" | "no_evidence".
type askDonePayload struct {
	FinishReason string `json:"finish_reason"`
}

// askErrorPayload is the "error" SSE event body (spec §5.1).
type askErrorPayload struct {
	Message string `json:"message"`
}

// askSystemPrompt instructs the model to answer strictly from the supplied
// context, degrade to "모른다" when the context is insufficient, and never
// present [추론] (insight) content as established fact — the echo-chamber
// guard from spec §3.2/§6.5 extends into the synthesized answer itself, not
// just the retrieval layer (search.applyInsightExclusionDefault).
const askSystemPrompt = `당신은 사용자의 개인 지식 베이스에서 검색된 문서를 근거로 질문에 답하는 어시스턴트입니다.

규칙:
1. 반드시 아래 제공된 [관측된 사실] 및 [추론] 섹션의 내용에만 근거해 답변하세요. 컨텍스트 밖의 지식을 사용하지 마세요.
2. 컨텍스트만으로 답할 수 없으면 솔직하게 "제공된 정보로는 답변할 수 없습니다"라고 답하세요. 추측해서 답하지 마세요.
3. [추론] 섹션은 AI가 문서에서 도출한 가설이며 확정된 사실이 아닙니다. 이 섹션의 내용을 인용할 때는 반드시 "~로 추정됩니다" 등 가설임을 밝히세요. 사실인 것처럼 단정하지 마세요.
4. 한국어로, 간결하고 명확하게 답변하세요.`

// writeSSEEvent writes one "event: <name>\ndata: <json>\n\n" frame and
// flushes immediately so the client sees it without buffering. Returns an
// error if marshaling fails; write/flush errors are not returned (the
// connection may already be half-closed by a disconnected client) but the
// caller can still detect a marshal failure, which indicates a programming
// bug rather than a network condition.
func writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("ask: marshal %s payload: %w", event, err)
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return nil //nolint:nilerr // write failure is expected on client disconnect; not fatal to the caller.
	}
	flusher.Flush()
	return nil
}

// askHandler handles POST /api/v1/ask: the 3-stage RAG pipeline
// (의도분석 → RAG검색 → 종합·배출), streamed to the client via SSE.
func (s *Server) askHandler(w http.ResponseWriter, r *http.Request) {
	var req AskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Question) == "" {
		writeError(w, http.StatusBadRequest, "question is required")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		// Should never happen with chi's default net/http server; fail loud
		// rather than silently buffer the whole response.
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	// Headers required by the Cloudflare tunnel this deployment sits behind
	// (per project history: intermediate proxies buffer streaming responses
	// unless explicitly told not to).
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx, cancel := context.WithTimeout(r.Context(), s.askTimeout)
	defer cancel()

	// Stage 1: intent classification. Classify's contract never returns a
	// non-nil error in the shipped implementation, but the defensive
	// fallback keeps this handler correct even against a future
	// Classifier implementation that does fail (spec §4.4).
	params, err := s.intentClassifier.Classify(ctx, req.Question)
	if err != nil {
		params = intent.Params{RawQuery: req.Question, Kind: intent.KindGeneral}
	}

	// Stage 2: retrieval assembly (원문/정리 vs 추론 layers, spec §5.3).
	result, err := assembleRetrieval(ctx, s.search, params, s.askTopK, s.askInsightM)
	if err != nil {
		slog.Error("ask: retrieval failed", "error", err)
		_ = writeSSEEvent(w, flusher, "error", askErrorPayload{Message: "retrieval failed"})
		_ = writeSSEEvent(w, flusher, "done", askDonePayload{FinishReason: "error"})
		return
	}

	// Observed first, then Inferred — determines the frontend's rendering
	// order (capture-frontend plan Task 9: insight-layer sources are
	// visually distinct).
	sources := make([]AskSourceItem, 0, len(result.Observed)+len(result.Inferred))
	sources = append(sources, mapAskSources(result.Observed)...)
	sources = append(sources, mapAskSources(result.Inferred)...)
	if err := writeSSEEvent(w, flusher, "sources", askSourcesPayload{Sources: sources}); err != nil {
		slog.Error("ask: failed to write sources event", "error", err)
		return
	}

	// No primary evidence → do not burn tokens synthesizing an answer
	// (spec §6.3). insight-only results never satisfy this check.
	if len(result.Observed) == 0 {
		_ = writeSSEEvent(w, flusher, "done", askDonePayload{FinishReason: "no_evidence"})
		return
	}

	// Stage 3: synthesis + streaming.
	finishReason := s.synthesize(ctx, w, flusher, req.Question, result)
	if finishReason == "" {
		// Sentinel for "client disconnected mid-stream" (context.Canceled):
		// the connection is already gone, so attempting a final "done"
		// write would be a doomed w.Write call. A server-side timeout
		// (context.DeadlineExceeded) is NOT this case — synthesize already
		// wrote an "error" event and returned "error" for that path.
		return
	}
	_ = writeSSEEvent(w, flusher, "done", askDonePayload{FinishReason: finishReason})
}

// mapAskSources converts search results into the wire-contract AskSourceItem
// shape. ID is always sr.Document.ID.String() — the one field the ask
// backend plan calls out as non-negotiable for the future
// (query, document_id, thumbs) feedback loop.
func mapAskSources(results []*model.SearchResult) []AskSourceItem {
	items := make([]AskSourceItem, 0, len(results))
	for _, r := range results {
		items = append(items, AskSourceItem{
			ID:         r.Document.ID.String(),
			Title:      r.Document.Title,
			SourceType: string(r.Document.SourceType),
			Score:      r.Score,
		})
	}
	return items
}

// synthesize is Stage 3. It writes "token" SSE events directly (streaming
// side effect) as the LLM produces them, rather than returning the full
// answer.
//
// Return value is the finish_reason to use for the "done" event, with one
// exception: an empty string is a sentinel meaning "the client disconnected
// mid-stream (context.Canceled) — do not write a done event at all", since
// askHandler cannot distinguish "stop" from "the caller already handled it"
// any other way without an extra return value.
func (s *Server) synthesize(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, question string, result RetrievalResult) string {
	if !s.llmClient.Enabled() {
		_ = writeSSEEvent(w, flusher, "error", askErrorPayload{Message: "LLM is not configured"})
		return "error"
	}

	messages := buildAskMessages(question, result)

	if sc, ok := s.llmClient.(llm.StreamCompleter); ok {
		err := sc.StreamWithMessages(ctx, askSystemPrompt, messages, func(delta string) {
			_ = writeSSEEvent(w, flusher, "token", askTokenPayload{Text: delta})
		})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				slog.Info("ask: client disconnected during synthesis", "error", err)
				return ""
			}
			slog.Error("ask: streaming synthesis failed", "error", err)
			_ = writeSSEEvent(w, flusher, "error", askErrorPayload{Message: "synthesis failed"})
			return "error"
		}
		return "stop"
	}

	// Fallback for a Completer that does not implement StreamCompleter
	// (e.g. a test fake, or a future non-streaming-only backend): emit the
	// full answer as a single "token" event.
	answer, err := s.llmClient.CompleteWithMessages(ctx, askSystemPrompt, messages)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			slog.Info("ask: client disconnected during synthesis", "error", err)
			return ""
		}
		slog.Error("ask: synthesis failed", "error", err)
		_ = writeSSEEvent(w, flusher, "error", askErrorPayload{Message: "synthesis failed"})
		return "error"
	}
	_ = writeSSEEvent(w, flusher, "token", askTokenPayload{Text: answer})
	return "stop"
}

// buildAskMessages assembles the multi-turn prompt for Stage 3: a
// [관측된 사실] section built from result.Observed, an optional
// [추론 — 가설이며 사실로 인용 불가] section built from result.Inferred, and the
// user's raw question as the final turn.
func buildAskMessages(question string, result RetrievalResult) []llm.Message {
	var b strings.Builder
	b.WriteString("[관측된 사실]\n")
	if len(result.Observed) == 0 {
		b.WriteString("(없음)\n")
	}
	for _, r := range result.Observed {
		fmt.Fprintf(&b, "- (%s) %s: %s\n", r.Document.SourceType, r.Document.Title, r.Document.Content)
	}
	if len(result.Inferred) > 0 {
		b.WriteString("\n[추론 — 가설이며 사실로 인용 불가]\n")
		for _, r := range result.Inferred {
			fmt.Fprintf(&b, "- (%s) %s: %s\n", r.Document.SourceType, r.Document.Title, r.Document.Content)
		}
	}

	return []llm.Message{
		{Role: "user", Content: b.String()},
		{Role: "user", Content: question},
	}
}
