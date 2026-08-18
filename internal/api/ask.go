package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/timeutil"
)

// AskRequest is the JSON body accepted by POST /api/v1/ask. ConversationID
// is optional: omitted (or an unparsable value) starts a brand-new
// conversation with a server-generated id — see resolveConversation.
type AskRequest struct {
	Question       string `json:"question"`
	ConversationID string `json:"conversation_id,omitempty"`
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

// askConversationPayload is the "conversation" SSE event body. It is sent
// once, before "sources", so the client can learn the conversation_id for
// a brand-new conversation (or have the requested one echoed back) and the
// 0-based turn_index of the turn currently being answered — both needed to
// pass conversation_id back on the client's next follow-up request. This
// event was added after the frontend shipped; parseAskEvent
// (web/src/lib/sseEvents.ts) returns null for unrecognized event names and
// the caller ignores nulls, so adding it here does not break the deployed
// frontend (frontend wiring to actually use it is separate work).
type askConversationPayload struct {
	ConversationID string `json:"conversation_id"`
	TurnIndex      int    `json:"turn_index"`
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

// Every date/time shown to the model in the Stage 3 prompt — the current-date
// line in buildAskSystemPrompt and each document's occurred_at in
// buildAskMessages — is rendered in timeutil.KST(), never in the server
// process's local timezone (which is UTC in the production container).
// Second Brain's users and ingested data are Korea-based, so a request served
// near the container's local midnight would otherwise show the model the wrong
// calendar date and reintroduce the exact bug this file fixes.
//
// The location is shared with internal/intent, which resolves the
// "오늘"/"이번 주"/"지난달" retrieval windows in the same zone. One definition is
// deliberate: a second copy would let the prompt's rendered date and the
// retrieval window drift into disagreeing about which day "today" is. See
// internal/timeutil/kst.go for the tzdata/fallback rationale.

// koreanWeekdayNames is indexed by time.Weekday (Sunday=0 ... Saturday=6).
var koreanWeekdayNames = [...]string{"일", "월", "화", "수", "목", "금", "토"}

// formatKoreanDateTime renders t (already converted to the desired
// location by the caller) as "YYYY-MM-DD(요일) HH:MM".
func formatKoreanDateTime(t time.Time) string {
	return fmt.Sprintf("%04d-%02d-%02d(%s) %02d:%02d",
		t.Year(), int(t.Month()), t.Day(), koreanWeekdayNames[t.Weekday()], t.Hour(), t.Minute())
}

// formatOccurredAt renders a document's OccurredAt in KST, or an explicit
// "no timestamp" label when nil. nil is common and expected: every
// llm-memory-source document has OccurredAt == nil (see
// internal/model/document.go's field comment), so this must not panic and
// must not silently omit the field — an omitted field could read to the
// model as "same day as now" rather than "unknown".
func formatOccurredAt(t *time.Time) string {
	if t == nil {
		return "시각 정보 없음"
	}
	return formatKoreanDateTime(t.In(timeutil.KST()))
}

// askSystemPromptTemplate is buildAskSystemPrompt's format string. It
// instructs the model to answer strictly from the supplied context,
// degrade to "모른다" when the context is insufficient, and never present
// [추론] (insight) content as established fact — the echo-chamber guard
// from spec §3.2/§6.5 extends into the synthesized answer itself, not just
// the retrieval layer (search.applyInsightExclusionDefault). %s is the
// current date/time rendered by buildAskSystemPrompt.
const askSystemPromptTemplate = `당신은 사용자의 개인 지식 베이스에서 검색된 문서를 근거로 질문에 답하는 어시스턴트입니다.

현재 시각: %s (KST, 한국 표준시) 기준입니다.

규칙:
1. 반드시 아래 제공된 [관측된 사실] 및 [추론] 섹션의 내용에만 근거해 답변하세요. 컨텍스트 밖의 지식을 사용하지 마세요.
2. 컨텍스트만으로 답할 수 없으면 솔직하게 "제공된 정보로는 답변할 수 없습니다"라고 답하세요. 추측해서 답하지 마세요.
3. [추론] 섹션은 AI가 문서에서 도출한 가설이며 확정된 사실이 아닙니다. 이 섹션의 내용을 인용할 때는 반드시 "~로 추정됩니다" 등 가설임을 밝히세요. 사실인 것처럼 단정하지 마세요.
4. "내일", "어제", "지난주", "이번 달" 등 상대적인 시간 표현이 질문이나 문서 내용에 등장하면, 반드시 위의 현재 시각을 기준으로 날짜를 계산해 해석하세요. 각 문서 하단의 [발생] 시각도 이 기준으로 판단하세요. 현재 시각을 알 수 없다는 이유로 답변을 회피하지 마세요.
5. 한국어로, 간결하고 명확하게 답변하세요.`

// buildAskSystemPrompt renders askSystemPromptTemplate with now converted
// to KST. now is normally s.nowFunc() (real time.Now, or an injected clock
// in tests) — see Server.now's doc comment for why this exists: the prior
// compile-time askSystemPrompt const gave the model no way to know "today",
// so it could not resolve "내일"/"어제" and answered that it didn't know
// what day it was.
func buildAskSystemPrompt(now time.Time) string {
	return fmt.Sprintf(askSystemPromptTemplate, formatKoreanDateTime(now.In(timeutil.KST())))
}

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

	// Multi-turn setup: resolve/mint the conversation id + this turn's
	// index, and load capped recent history (question/answer text only —
	// see askHistoryTurn). Emitted BEFORE "sources" so the client can learn
	// conversation_id even if retrieval subsequently fails.
	conversationID, turnIndex, history := s.resolveConversation(ctx, req.ConversationID)
	if err := writeSSEEvent(w, flusher, "conversation", askConversationPayload{
		ConversationID: conversationID.String(),
		TurnIndex:      turnIndex,
	}); err != nil {
		slog.Error("ask: failed to write conversation event", "error", err)
		return
	}

	// Query rewrite: a follow-up like "그거 더 자세히" carries no searchable
	// content on its own, so Stage 1/2 must see a standalone question
	// instead of the user's raw wording. No-op (returns req.Question
	// unchanged) when this is the first turn of the conversation — see
	// rewriteStandaloneQuestion's doc comment for the full fallback
	// contract. The user's ORIGINAL question (req.Question), not this
	// rewritten one, is what Stage 3 shows the model as the current turn
	// (buildAskMessages) and what gets persisted (saveAskTurn) — the
	// rewrite is a search-only artifact.
	searchQuestion, _ := s.rewriteStandaloneQuestion(ctx, history, req.Question)

	// Stage 1: intent classification. Classify's contract never returns a
	// non-nil error in the shipped implementation, but the defensive
	// fallback keeps this handler correct even against a future
	// Classifier implementation that does fail (spec §4.4).
	params, err := s.intentClassifier.Classify(ctx, searchQuestion)
	if err != nil {
		params = intent.Params{RawQuery: searchQuestion, Kind: intent.KindGeneral}
	}

	// Stage 2: retrieval assembly (원문/정리 vs 추론 layers, spec §5.3).
	result, err := assembleRetrieval(ctx, s.search, params, s.askTopK, s.askInsightM)
	if err != nil {
		slog.Error("ask: retrieval failed", "error", err)
		_ = writeSSEEvent(w, flusher, "error", askErrorPayload{Message: "retrieval failed"})
		_ = writeSSEEvent(w, flusher, "done", askDonePayload{FinishReason: "error"})
		// No turn is persisted here: this is a total-pipeline failure with
		// no sources and no answer text — there is nothing worth replaying
		// into a future turn's history, and persisting an empty/error stub
		// would just consume a turn_index for no benefit.
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
		s.saveAskTurn(ctx, conversationID, turnIndex, req.Question, "", "no_evidence", sources)
		return
	}

	// Stage 3: synthesis + streaming. question passed here is the user's
	// ORIGINAL wording (not searchQuestion) — see the rewrite comment above.
	finishReason, answer := s.synthesize(ctx, w, flusher, req.Question, result, history)
	if finishReason == "" {
		// Sentinel for "client disconnected mid-stream" (context.Canceled):
		// the connection is already gone, so attempting a final "done"
		// write would be a doomed w.Write call. A server-side timeout
		// (context.DeadlineExceeded) is NOT this case — synthesize already
		// wrote an "error" event and returned "error" for that path.
		//
		// Still persist a partial turn when synthesize managed to stream at
		// least one token before the disconnect: it is more useful to a
		// same-conversation retry than nothing (the rewrite/synthesis
		// stages for the NEXT turn can see how far the previous answer
		// got), and no client is waiting on this write so it costs nothing
		// to attempt. finish_reason "error" (not a new enum value) because
		// the answer never legitimately completed. An empty partial answer
		// (cancelled before the first token) is not worth a turn_index.
		if answer != "" {
			s.saveAskTurn(ctx, conversationID, turnIndex, req.Question, answer, "error", sources)
		}
		return
	}
	_ = writeSSEEvent(w, flusher, "done", askDonePayload{FinishReason: finishReason})
	// Saved AFTER the client-visible "done" write so a slow/failing DB
	// write never delays or breaks the response the client already has —
	// saveAskTurn itself also swallows its own errors (see its doc
	// comment), this ordering is the second half of that guarantee.
	s.saveAskTurn(ctx, conversationID, turnIndex, req.Question, answer, finishReason, sources)
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
// answer to the client — but it also accumulates that same text into the
// answer return value so askHandler can persist it (saveAskTurn) without
// synthesize needing to know anything about ask_sessions/persistence.
//
// finishReason is the value to use for the "done" event, with one
// exception: an empty string is a sentinel meaning "the client disconnected
// mid-stream (context.Canceled) — do not write a done event at all", since
// askHandler cannot distinguish "stop" from "the caller already handled it"
// any other way without an extra return value. answer holds whatever text
// was produced before that happened, which may be empty (disconnected
// before the first token) or partial (disconnected mid-stream) — it is
// never the empty string for finishReason "stop".
func (s *Server) synthesize(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, question string, result RetrievalResult, history []askHistoryTurn) (finishReason, answer string) {
	if !s.llmClient.Enabled() {
		_ = writeSSEEvent(w, flusher, "error", askErrorPayload{Message: "LLM is not configured"})
		return "error", ""
	}

	messages := buildAskMessages(question, result, history)
	systemPrompt := buildAskSystemPrompt(s.nowFunc())

	if sc, ok := s.llmClient.(llm.StreamCompleter); ok {
		var answerBuilder strings.Builder
		err := sc.StreamWithMessages(ctx, systemPrompt, messages, func(delta string) {
			answerBuilder.WriteString(delta)
			_ = writeSSEEvent(w, flusher, "token", askTokenPayload{Text: delta})
		})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				slog.Info("ask: client disconnected during synthesis", "error", err)
				return "", answerBuilder.String()
			}
			slog.Error("ask: streaming synthesis failed", "error", err)
			_ = writeSSEEvent(w, flusher, "error", askErrorPayload{Message: "synthesis failed"})
			return "error", answerBuilder.String()
		}
		return "stop", answerBuilder.String()
	}

	// Fallback for a Completer that does not implement StreamCompleter
	// (e.g. a test fake, or a future non-streaming-only backend): emit the
	// full answer as a single "token" event.
	full, err := s.llmClient.CompleteWithMessages(ctx, systemPrompt, messages)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			slog.Info("ask: client disconnected during synthesis", "error", err)
			return "", ""
		}
		slog.Error("ask: synthesis failed", "error", err)
		_ = writeSSEEvent(w, flusher, "error", askErrorPayload{Message: "synthesis failed"})
		return "error", ""
	}
	_ = writeSSEEvent(w, flusher, "token", askTokenPayload{Text: full})
	return "stop", full
}

// buildAskMessages assembles the multi-turn prompt for Stage 3.
//
// history (askHistoryTurn: question/answer TEXT only, capped at
// askMaxHistoryTurns) is replayed FIRST as genuine alternating
// user/assistant turns, exactly as the LLM originally produced/received
// them. It deliberately carries no source-document content — evidence for
// a previous turn already shaped that turn's answer text, and re-attaching
// its full document bodies here would both (a) duplicate information the
// model already has via the answer text and (b) make prompt size grow with
// total conversation evidence rather than just conversation length. This
// is the single reviewer-visible guarantee behind requirement (c) of the
// multi-turn ask feature: no prior turn's document body is ever replayed.
//
// After history, the CURRENT turn is appended as: a [관측된 사실] section
// built from result.Observed, an optional
// [추론 — 가설이며 사실로 인용 불가] section built from result.Inferred, and the
// user's raw question as the final turn. Each document line includes its
// occurred_at (formatOccurredAt, KST, "시각 정보 없음" when nil) so the model
// can reason about when the document's content happened, not just what it
// says — the second half of the date-context fix (see buildAskSystemPrompt
// for the first half, "what day is today").
func buildAskMessages(question string, result RetrievalResult, history []askHistoryTurn) []llm.Message {
	messages := make([]llm.Message, 0, len(history)*2+2)
	for _, h := range history {
		messages = append(messages,
			llm.Message{Role: "user", Content: h.Question},
			llm.Message{Role: "assistant", Content: h.Answer},
		)
	}

	var b strings.Builder
	b.WriteString("[관측된 사실]\n")
	if len(result.Observed) == 0 {
		b.WriteString("(없음)\n")
	}
	for _, r := range result.Observed {
		fmt.Fprintf(&b, "- (%s) %s [발생: %s]: %s\n", r.Document.SourceType, r.Document.Title, formatOccurredAt(r.Document.OccurredAt), r.Document.Content)
	}
	if len(result.Inferred) > 0 {
		b.WriteString("\n[추론 — 가설이며 사실로 인용 불가]\n")
		for _, r := range result.Inferred {
			fmt.Fprintf(&b, "- (%s) %s [발생: %s]: %s\n", r.Document.SourceType, r.Document.Title, formatOccurredAt(r.Document.OccurredAt), r.Document.Content)
		}
	}

	messages = append(messages,
		llm.Message{Role: "user", Content: b.String()},
		llm.Message{Role: "user", Content: question},
	)
	return messages
}
