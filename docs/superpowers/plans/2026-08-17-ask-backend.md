# Ask Backend Implementation Plan — `POST /api/v1/ask`

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the `POST /api/v1/ask` SSE streaming endpoint as the 3-stage pipeline (의도분석 → RAG검색 → 종합·배출) confirmed by the pipeline design spec, satisfying the frontend's already-shipped SSE contract (`web/src/lib/sseEvents.ts` / `types.ts`, 9 passing tests) byte-for-byte.

**Architecture:** All three stages run in-process inside `cmd/server` (no new deployable, no ax/batch layer — see spec §3). Stage 1 is a new `internal/intent` package (deterministic pre-pass + LLM fallback, never fails fatally). Stage 2 is a thin mapping layer in `internal/api` that calls the existing `internal/search.Service.Search` twice (본검색 + 인사이트검색, unchanged). Stage 3 is a new SSE handler in `internal/api/ask.go` that streams tokens from the LLM. Both `*search.Service` and `llm.Completer` are **already wired unconditionally** into `api.Server` (`router.go:34,37` and `NewServer` params) — no new `With*` builder is required for the base wiring, only for ask-specific tuning knobs (topK/insightM/timeout).

**Tech Stack:** Go, chi router, existing `internal/search.Service`, existing `internal/llm.Client` (extended with streaming), Server-Sent Events (`net/http`, no library — this repo has zero prior SSE code, confirmed by `grep -rl "text/event-stream"` returning nothing).

**Spec:** `docs/superpowers/specs/2026-08-17-ask-pipeline-design.md` (Part A, §3–§9). Parent spec (`2026-08-17-ask-capture-design.md` §5) referenced but not re-read in full for this plan — its SSE contract is already captured verbatim by the frontend types below.

---

## Confirmed contract (read directly from code, not from the 104KB/75KB plan docs)

**Request** (`web/src/app/ask/page.tsx:1427`, capture-frontend plan Task 9):
```json
{ "question": "지난달 김대표와 나눈 얘기 요약해줘" }
```

**SSE wire format**, from `web/src/lib/sseEvents.ts` + `types.ts:122-134` — this is the ground truth the Go handler MUST produce exactly:

```
event: sources
data: {"sources":[{"id":"<uuid>","title":"...","source_type":"sms","score":0.83}, ...]}

event: token
data: {"text":"토큰 조각"}

event: done
data: {"finish_reason":"stop"}   // or "error" | "no_evidence"

event: error
data: {"message":"..."}
```

`AskSourceItem = { id: string; title: string; source_type: SourceType; score: number }` — **`id` is mandatory** (frontend type has no `?`), and per the task brief it is the only future path to `(query, document_id, thumbs)` feedback labels. `internal/api/feedback.go` already has `FeedbackRequest.DocumentID *string` wired to a `Feedback` store — confirms this is the intended linkage; `sources[].id` MUST be the same UUID string form (`model.Document.ID.String()`) so it round-trips into `POST /api/v1/feedback`.

`parseAskEvent` returns `null` (does not throw) on unknown event names or malformed JSON — the Go handler does not need to worry about exotic frontend error modes, but it does mean **field names must match exactly** (`finish_reason`, `source_type`, snake_case) or the frontend silently drops the event.

---

## Design-doc vs. code discrepancies found (report these, do not silently patch around them)

1. **`Completer.StreamWithMessages` does not exist.** The pipeline spec (§6.2) writes `Synthesizer.Synthesize` as if `llm.Completer` already has a streaming method. Checked `internal/llm/client.go:82-96`: `Completer` only has `Enabled() bool` and `CompleteWithMessages(...) (string, error)` — single-shot, not streaming. **This plan adds streaming as new work (Task 1)**, not a "reuse" as the spec implies.
2. **No SSE code exists anywhere in the repo** (`grep -rl "text/event-stream\|http.Flusher"` → zero matches). The spec treats SSE as already-confirmed infrastructure (parent spec §5.1/§5.3); it is in fact new for this codebase and needs its own flush/disconnect/timeout handling (Task 4).
3. **`model.SearchQuery` has no date-range field**, confirmed independently (`internal/model/document.go:187-198`) — matches spec §4.5's own finding. This plan treats `KindTemporal` as **explicitly degraded** (approximate via `Sort: "recent"` only) and does **not** add `OccurredFrom`/`OccurredTo` — that schema change is spec §8.1's own open item, out of scope here to keep this plan shippable without a migration.
4. **`internal/api/notes.go`'s `WithNotes` optional-wiring pattern does not apply here** — unlike notes/ingest routes, `/ask` depends only on `s.search` and `s.llmClient`, both already unconditional `NewServer` params. The route is registered unconditionally inside the existing `requireAPIKey` group, same as `/api/v1/search`.
5. **`LLM_ASK_MODEL` is not needed to satisfy this ticket.** Reusing the single already-configured `s.llmClient` for both Stage 1 classification and Stage 3 synthesis avoids wiring a second `llm.Client` instance for zero specified requirement (also consistent with spec §3's anti-over-engineering stance). Listed as deferred in Open Items.

---

## Global Constraints

- No new deployable, no ax, no cross-host calls — everything runs inside `cmd/server` (spec §3).
- Stage 1 (intent) never returns a fatal error to the handler — always degrades to `KindGeneral` (spec §4.4). `intent.Classifier.Classify` keeps an `error` return for interface generality/testability, but the shipped implementation guarantees `err == nil` always.
- Stage 2 keeps the existing 원문/정리/추론 3-layer separation via **two separate `search.Service.Search` calls** (`ExcludeSourceTypes: [insight]` then `SourceType: &insight`) — never merge into one query (spec §5.3).
- `sources` event lists `Observed` results first, then `Inferred` — this determines the frontend's rendering order (capture-frontend plan Task 9 checklist: "insight-layer sources are visually distinct").
- If `Observed` is empty → `finish_reason: "no_evidence"`, **no LLM call is made** (spec §6.3) — do not burn tokens synthesizing an answer with zero primary evidence.
- `insight`-only results never appear in `Observed`; `applyInsightExclusionDefault` (from the sibling capture-backend plan, `internal/api/search.go`) already guarantees this at the `search.Service` boundary — Stage 2 does not need to re-filter.
- Client disconnect (`ctx.Err() == context.Canceled`) during LLM streaming: stop cleanly, do not attempt to write further SSE frames (connection is gone), log at `Info` level only — this is not an error.
- Server-side timeout: wrap the whole request in `context.WithTimeout` (`ASK_TIMEOUT_SECONDS`, default 60s, mirrors `llm.Config.Timeout` default). On timeout, emit `error` + `done{finish_reason:"error"}` best-effort before the deadline closes the connection.
- `Cache-Control: no-cache` + `X-Accel-Buffering: no` headers are required — this deployment sits behind a Cloudflare tunnel (per project memory: `web` was previously the sole ingress point; tunnels/proxies are known to buffer streaming responses unless told not to).
- No DB migration in this plan (no new columns/tables — Stage 1/2/3 are all in-memory transforms over existing `search.Service` output).
- Out of scope: `OccurredFrom`/`OccurredTo` schema extension (spec §4.5/§8.1), exact-token/phone-number index fix (spec §4.3/§8.3), `/metrics` wiring (spec §7), `LLM_ASK_MODEL` dedicated model (see discrepancy #5), HyDE auto-enable heuristic (spec §5.2 last row — "권장, 필수 아님"; left as a follow-up, `UseHyDE` stays `false` in this plan's mapping to avoid unverified behavior change).

---

## File Structure

| File | Action | Responsibility |
|------|--------|-----------------|
| `internal/llm/client.go` | Modify | Add `StreamCompleter` interface + `Client.StreamWithMessages` (SSE-based streaming chat completion) |
| `internal/llm/client_test.go` | Modify | Tests for `StreamWithMessages` (multi-chunk, error mid-stream, context cancel) |
| `internal/intent/intent.go` | Create | `Kind`, `Params`, `Classifier` interface, `LLMClassifier` (deterministic pre-pass + LLM fallback) |
| `internal/intent/intent_test.go` | Create | Table-driven tests: regex date/phone matches, LLM fallback parse, confidence threshold, degrade-to-general |
| `internal/api/ask_retrieval.go` | Create | `RetrievalResult`, `documentSearcher` interface, `assembleRetrieval` (intent → `model.SearchQuery` mapping, spec §5.2) |
| `internal/api/ask_retrieval_test.go` | Create | Fake `documentSearcher` captures call params per `Kind`; 0-result fallback cases |
| `internal/api/ask.go` | Create | `AskRequest`, SSE event wire types, `writeSSEEvent`, `askHandler`, `synthesize` (Stage 3, prompt assembly + streaming) |
| `internal/api/ask_test.go` | Create | `httptest` SSE handler tests (auth, empty question, 0 results → no_evidence, event order, LLM unconfigured, client disconnect) |
| `internal/config/config.go` | Modify | Add `AskTimeoutSeconds`, `AskContextTopK`, `AskContextInsightM` fields + env parsing |
| `internal/api/router.go` | Modify | Register `POST /api/v1/ask` inside existing `requireAPIKey` group; add ask-tuning fields to `Server` + `WithAskConfig` builder |
| `cmd/server/main.go` | Modify | Wire `cfg.AskTimeoutSeconds/AskContextTopK/AskContextInsightM` via `.WithAskConfig(...)` |

---

## Task 1: `internal/llm` — streaming completion support

### Files
- Modify: `internal/llm/client.go`
- Modify: `internal/llm/client_test.go`

### Interfaces

```go
// StreamCompleter is implemented by LLM clients that support token-by-token
// streaming. It is intentionally NOT merged into Completer — existing fake
// Completer implementations across internal/curation, internal/collector,
// internal/search/hyde.go, internal/worker/{summarizer,note_enrichment_worker,
// entity_extractor}.go (6 call sites, confirmed via grep) would all break if
// Completer grew a new required method. Callers that need streaming type-assert:
//
//   if sc, ok := completer.(llm.StreamCompleter); ok { ... } else { /* fallback */ }
//
type StreamCompleter interface {
	// StreamWithMessages sends a multi-turn chat completion request with
	// stream: true. onDelta is called once per received content delta
	// (never called with an empty string). Returns when the stream ends
	// ([DONE] sentinel) or ctx is cancelled/times out.
	StreamWithMessages(ctx context.Context, system string, messages []Message, onDelta func(string)) error
}

// *Client implements both Completer and StreamCompleter.
func (c *Client) StreamWithMessages(ctx context.Context, system string, messages []Message, onDelta func(string)) error
```

### Implementation notes
- Request body: same `chatRequest` shape as `CompleteWithMessages` plus `Stream: true` (new `json:"stream"` field — must default to `false`/omitted for the existing non-streaming path so `CompleteWithMessages` behavior is byte-for-byte unchanged).
- Response parsing: OpenAI-compatible streaming responses are `\n\n`-delimited SSE lines of the form `data: {"choices":[{"delta":{"content":"..."}}]}`, terminated by a literal `data: [DONE]` line. Use `bufio.Scanner` over `resp.Body`, split on `\n`, skip blank lines, strip the `data: ` prefix, stop on `[DONE]`.
- No retry-on-5xx for the streaming path (unlike `CompleteWithMessages`'s 2-retry policy) — a stream that has already started emitting tokens cannot be safely retried without risking duplicate output to the SSE client. On a 4xx/5xx **before** the first byte of the body is read, return the error normally (Stage 3 can still emit a clean `error` event since no tokens have been sent yet).
- Respect `ctx` cancellation inside the scan loop (`select` on `ctx.Done()` is not directly possible with `bufio.Scanner`; use `req = req.WithContext(ctx)` so `resp.Body.Read` unblocks and returns `ctx.Err()` when the client disconnects — same pattern `http.Client` already uses for `CompleteWithMessages`).

### Steps

- [ ] Add `Stream bool \`json:"stream,omitempty"\`` to `chatRequest`.
- [ ] Add `streamChunk` type for parsing `{"choices":[{"delta":{"content":string},"finish_reason":*string}]}`.
- [ ] Implement `Client.StreamWithMessages` per notes above.
- [ ] Write `TestClient_StreamWithMessages_MultipleChunks` — fake `httptest.Server` emits 3 `data:` lines + `[DONE]`, assert `onDelta` called 3 times in order with correct text, no error.
- [ ] Write `TestClient_StreamWithMessages_ContextCancelMidStream` — server sleeps between chunks, cancel ctx after 1st chunk, assert returned error wraps `context.Canceled` and no panic.
- [ ] Write `TestClient_StreamWithMessages_HTTPErrorBeforeFirstByte` — server returns 500 immediately, assert error returned and `onDelta` never called.
- [ ] Write `TestClient_StreamWithMessages_NotEnabled` — `Enabled() == false` → returns error without any HTTP call (mirrors existing `CompleteWithMessages` guard at client.go:133).

### Verification
`go test ./internal/llm/...` — new tests pass, existing `CompleteWithMessages` tests unchanged and still pass (confirms `Stream: false` default preserves old wire format).

---

## Task 2: `internal/intent` — Stage 1 classifier

### Files
- Create: `internal/intent/intent.go`
- Create: `internal/intent/intent_test.go`

### Interfaces

```go
package intent

type Kind string

const (
	KindGeneral    Kind = "general"
	KindEntity     Kind = "entity"
	KindTemporal   Kind = "temporal"
	KindExactToken Kind = "exact_token"
)

type Params struct {
	RawQuery     string
	Kind         Kind
	EntityName   string
	OccurredFrom *time.Time
	OccurredTo   *time.Time
	Confidence   float64
}

type Classifier interface {
	Classify(ctx context.Context, question string) (Params, error)
}

// LLMClassifier runs the deterministic pre-pass (regex) first, falling back
// to an LLM call for anything not matched. Classify NEVER returns a non-nil
// error in this implementation — all failure paths degrade to KindGeneral
// internally (spec §4.4) — the error return exists only so Classifier stays
// a general-purpose interface for future implementations/tests.
type LLMClassifier struct {
	llm llm.Completer
	now func() time.Time // injected for deterministic date-phrase tests; defaults to time.Now
}

func NewLLMClassifier(c llm.Completer) *LLMClassifier
func (c *LLMClassifier) Classify(ctx context.Context, question string) (Params, error)
```

### Deterministic pre-pass (no LLM call)
- Phone-number-shaped token: `\d{2,3}-?\d{3,4}-?\d{4}` → `KindExactToken`, `Confidence: 1.0`.
- Korean relative/absolute date phrases: `지난달`, `이번[ ]?주`, `오늘`, `어제`, `\d{4}년\s*\d{1,2}월` → `KindTemporal`, compute `OccurredFrom`/`OccurredTo` from `c.now()` (e.g. `"2026년 6월"` → `2026-06-01T00:00:00` .. `2026-06-30T23:59:59`, month-end computed via `time.Date(y, m+1, 0, ...)`), `Confidence: 1.0`.
- No match → fall through to LLM.

### LLM fallback
- System prompt instructs the model to classify into `{"kind":"general"|"entity","entity_name":"...","confidence":0.0-1.0}` (temporal/exact_token are deterministic-only per spec §4.2 comment — the LLM path only needs to distinguish `general` vs `entity`).
- Parse failure or `CompleteWithMessages` error → `Params{Kind: KindGeneral, Confidence: 0}`, `nil` error (spec §4.4 row 1–2).
- `Confidence < 0.5` → still return the classified `Kind`/`EntityName` as-is; **Stage 2 is responsible** for treating low confidence as `KindGeneral` (spec §4.4 row 4 — the collapse happens at the consumer, not inside Classify, so the raw classification stays inspectable in logs/tests).

### Steps

- [ ] Write `intent_test.go` table cases for regex date phrases with a fixed injected `now` (per spec §4.6): `"2026년 6월 요약"` → exact range, `"지난달"` relative to fixed `now`, `"010-1234-5678"` → `KindExactToken`.
- [ ] Write fake `llm.Completer` test: valid JSON → `KindEntity`/`EntityName`/`Confidence` parsed; malformed JSON → `KindGeneral` fallback; `CompleteWithMessages` error → `KindGeneral` fallback.
- [ ] Write precedence test: construct input that would regex-match `KindTemporal` — confirm LLM is never called (deterministic pre-pass short-circuits, spec §4.2 point 1 cost rationale).
- [ ] Implement `intent.go` per interfaces above.
- [ ] Run `go vet` — confirm no import cycle (`internal/intent` imports `internal/llm` only, not `internal/api`).

### Verification
`go test ./internal/intent/...`. No DB dependency (spec §4.6) — confirm no `internal/store` import.

---

## Task 3: Stage 2 — retrieval assembly

### Files
- Create: `internal/api/ask_retrieval.go`
- Create: `internal/api/ask_retrieval_test.go`

### Interfaces

```go
package api

// RetrievalResult keeps the 원문/정리 vs 추론 layers physically separate
// (spec §5.3) — Stage 3 renders them into different prompt sections and must
// never see them pre-merged.
type RetrievalResult struct {
	Observed []*model.SearchResult // ExcludeSourceTypes: [insight]
	Inferred []*model.SearchResult // SourceType: &insight
}

// documentSearcher is the subset of *search.Service used by Stage 2 — kept
// as a narrow interface so tests inject a fake without a real DB/embedder
// (mirrors search.DocumentSearcher's own fake-based test convention,
// internal/search/search.go:24).
type documentSearcher interface {
	Search(ctx context.Context, q model.SearchQuery) ([]*model.SearchResult, error)
}

// assembleRetrieval maps intent.Params to two search.Service.Search calls
// per the spec §5.2 table, then wraps the results.
func assembleRetrieval(ctx context.Context, searcher documentSearcher, params intent.Params, topK, insightM int) (RetrievalResult, error)
```

### Mapping (spec §5.2 table — implement exactly these rows, nothing extra)

| `params.Kind` | Query construction |
|---|---|
| `KindEntity` (`Confidence >= 0.5`) | `Query: params.RawQuery`, `Weights.EntityWeight: 1.5` (spec §5.2: entity lane must dominate; `Query` stays the full question because the entity RRF lane LIKE-matches against `Query`, not a separate field — no such field exists) |
| `KindTemporal` (`Confidence >= 0.5`) | `Query: params.RawQuery`, `Sort: "recent"` — **approximation only**, see discrepancy #3; does not hard-filter by date |
| `KindExactToken` | `Query: params.RawQuery`, `Weights.BigmWeight: 1.5` — spec §4.3 explicitly says this cannot fix the underlying tokenization failure; it is a best-effort nudge, not a fix |
| `KindGeneral` or `Confidence < 0.5` | `Query: params.RawQuery` unmodified — base query, no weight overrides |

Both branches build `base := model.SearchQuery{Query: params.RawQuery, Limit: topK}`, apply the row's override, then issue:
1. 본검색: `q := base; q.ExcludeSourceTypes = []model.SourceType{model.SourceInsight}` → `Observed`
2. 인사이트검색: `q := base; q.SourceType = &model.SourceInsight; q.Limit = insightM` → `Inferred`

### Failure modes (spec §5.4)

- `Search` error on either call → `assembleRetrieval` returns the error immediately (handler converts to `error` SSE event).
- 본검색 0 results → return `RetrievalResult{Observed: nil, Inferred: <인사이트 결과>}`, `nil` error — handler decides `no_evidence` (Task 4), not this function.
- 인사이트검색 0 results → normal, not an error (spec §5.4 row 2).

### Steps

- [ ] Write fake `documentSearcher` that records the last `model.SearchQuery` it received per call.
- [ ] Test: `KindEntity` → asserts `EntityWeight == 1.5` on the 본검색 call, `Query` unchanged.
- [ ] Test: `KindTemporal` → asserts `Sort == "recent"`.
- [ ] Test: `KindExactToken` → asserts `BigmWeight == 1.5`.
- [ ] Test: `KindGeneral` and `Confidence < 0.5` (even with `Kind: KindEntity`) → asserts base query, no overrides (confirms the confidence-collapse rule from spec §4.4 row 4 is enforced here, not in `intent`).
- [ ] Test: both calls always carry `ExcludeSourceTypes`/`SourceType` correctly regardless of `Kind` (spec §5.3 invariant).
- [ ] Test: 본검색 0 results → `Observed == nil`, function does not panic, `Inferred` still populated.
- [ ] Test: `Search` error on either call → propagated unchanged.
- [ ] Implement `assembleRetrieval`.

### Verification
`go test ./internal/api/... -run TestAssembleRetrieval`. No DB integration test (repo convention — confirmed absent everywhere per capture-backend plan's own note).

---

## Task 4: Stage 3 + SSE handler — `internal/api/ask.go`

### Files
- Create: `internal/api/ask.go`
- Create: `internal/api/ask_test.go`

### Interfaces

```go
package api

// AskRequest is the POST /api/v1/ask request body.
type AskRequest struct {
	Question string `json:"question"`
}

// AskSourceItem mirrors web/src/lib/types.ts:122-127 EXACTLY — field names
// and types are a wire contract, not free to change.
type AskSourceItem struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	SourceType string  `json:"source_type"`
	Score      float64 `json:"score"`
}

type askSourcesPayload struct{ Sources []AskSourceItem `json:"sources"` }
type askTokenPayload struct{ Text string `json:"text"` }
type askDonePayload struct{ FinishReason string `json:"finish_reason"` }
type askErrorPayload struct{ Message string `json:"message"` }

// writeSSEEvent writes one "event: <name>\ndata: <json>\n\n" frame and
// flushes immediately. Returns an error if marshal fails; write/flush
// errors are intentionally swallowed by the caller in most paths (see
// Client Disconnect note below) but this function itself still surfaces
// them so callers CAN check when it matters (first "sources" write).
func writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, event string, payload any) error

func (s *Server) askHandler(w http.ResponseWriter, r *http.Request)

// synthesize is Stage 3. It writes "token" events directly (streaming
// side effect) rather than returning the full answer, mirroring the
// pipeline spec §6.2 Synthesizer.Synthesize signature.
func (s *Server) synthesize(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, question string, result RetrievalResult) (finishReason string)
```

### Handler flow

1. Decode `AskRequest`. Empty/whitespace-only `Question` → `writeError(w, 400, ...)` (plain JSON, stream has not started yet — same pattern as `feedbackHandler`'s pre-body-write validation).
2. `w.Header().Set("Content-Type", "text/event-stream")`, `Cache-Control: no-cache`, `Connection: keep-alive`, `X-Accel-Buffering: no`. Type-assert `flusher, ok := w.(http.Flusher)` — if not ok, `writeError(w, 500, "streaming unsupported")` (should never happen with chi's default `net/http` server, but fail loud rather than silently buffer).
3. `ctx, cancel := context.WithTimeout(r.Context(), s.askTimeout); defer cancel()`.
4. Stage 1: `params, _ := s.intentClassifier.Classify(ctx, req.Question)` (error always nil per Task 2, but checked defensively — on non-nil error, treat as `intent.Params{Kind: intent.KindGeneral}`).
5. Stage 2: `result, err := assembleRetrieval(ctx, s.search, params, s.askTopK, s.askInsightM)`. On error → `writeSSEEvent(... "error" ...)` + `writeSSEEvent(... "done", {FinishReason:"error"})`, return.
6. Build combined `AskSourceItem` list: `append(mapSources(result.Observed), mapSources(result.Inferred)...)` (Observed first — spec §5.3 layering, frontend renders in this order). Write `sources` event. **`ID` MUST be `sr.Document.ID.String()`** — this is the one field the task brief calls out as non-negotiable for the future feedback loop.
7. If `len(result.Observed) == 0` → `writeSSEEvent(... "done", {FinishReason:"no_evidence"})`, return (spec §6.3 row 1 — no LLM call).
8. Stage 3: call `s.synthesize(ctx, w, flusher, req.Question, result)` → returns `finishReason`; write `done` event with it.

### `synthesize` (Stage 3) flow

1. If `!s.llmClient.Enabled()` → write `error` event `{"message":"LLM not configured"}`, return `"error"` (spec §6.4 references parent spec's "LLM 미설정" test case).
2. Build `system` prompt with the fixed constraints from spec §6.1 (must answer from context only, say "모른다" when insufficient, `insight` cited as hypothesis only — copy verbatim from parent spec §5.2, do not rephrase/weaken).
3. Build `messages []llm.Message` with `[관측된 사실]` section from `result.Observed` and, if non-empty, `[추론 — 가설이며 사실로 인용 불가]` section from `result.Inferred`, plus the user's `question` as the final turn.
4. `if sc, ok := s.llmClient.(llm.StreamCompleter); ok { ... } else { /* fallback below */ }`:
   - Streaming path: `err := sc.StreamWithMessages(ctx, system, messages, func(delta string) { _ = writeSSEEvent(w, flusher, "token", askTokenPayload{Text: delta}) })`.
   - On `errors.Is(err, context.Canceled)` → client disconnected; log `slog.Info`, return `""` (caller must skip writing `done` — connection is already gone; add a sentinel or check `ctx.Err()` before the final write in `askHandler`).
   - On other non-nil `err` → write `error` event, return `"error"`.
   - On nil `err` → return `"stop"`.
   - Fallback (non-streaming `Completer` only, e.g. test fakes that don't implement `StreamCompleter`): call `CompleteWithMessages` once, emit the full answer as a single `token` event, return `"stop"`/`"error"` accordingly.

### Client-disconnect handling (explicit, per task brief)

`askHandler` checks `ctx.Err() != nil` immediately after `synthesize` returns and before writing the final `done` event — if the request context is already cancelled (client gone), skip the write entirely rather than attempting a doomed `w.Write`. This is the one place `writeSSEEvent`'s error return is deliberately ignored elsewhere but checked here first.

### Steps

- [ ] Write `ask_test.go` fake `llm.Completer`+`StreamCompleter` combo (configurable to emit N chunks, or return an error mid-stream, or simulate `!Enabled()`).
- [ ] Write fake `intent.Classifier` (returns fixed `Params`) and fake `documentSearcher` (or reuse Task 3's fakes via a small `search.Service`-shaped wrapper — confirm whether `*search.Service` itself can be constructed with fakes in tests, per `internal/search/search_test.go` convention, before deciding whether `askHandler` depends on `*search.Service` directly or the `documentSearcher` interface — **prefer the narrow interface** for testability, matching Task 3's design).
- [ ] Test: missing/empty `question` → 400, no SSE headers written.
- [ ] Test: valid question, 0 Observed results → `sources` (only Inferred, or empty) then `done{finish_reason:"no_evidence"}`, no `token` events, LLM fake never invoked.
- [ ] Test: valid question, Observed results present, streaming fake emits 3 chunks → event order is exactly `sources`, `token`×3, `done{finish_reason:"stop"}`.
- [ ] Test: `sources` payload — assert each `AskSourceItem.ID` equals the fake result's `Document.ID.String()` (this is the test that pins the feedback-loop requirement).
- [ ] Test: `Enabled() == false` → `sources` written, then `error` event, `done{finish_reason:"error"}`, no `token` events.
- [ ] Test: `assembleRetrieval` returns an error (fake searcher returns error) → `error` + `done{finish_reason:"error"}`, no `sources` event written (search failed before sources could be computed).
- [ ] Test: auth — missing/invalid `Authorization: Bearer` header → 401, verified through the full router (`s.Handler()`), not just the bare handler, so `requireAPIKey` wiring (Task 5) is exercised too.
- [ ] Test: context cancellation mid-stream (cancel the request context after the 1st chunk) → handler returns without panicking, no further writes attempted after cancellation (assert via a `httptest.ResponseRecorder` wrapper that fails the test if `Write` is called after a sentinel "cancelled" flag is set).
- [ ] Implement `ask.go` per the flow above.

### Verification
`go test ./internal/api/... -run TestAsk` — all event-order and field-name assertions pass. Manually diff `AskSourceItem`/event field names against `web/src/lib/types.ts:122-134` one more time after implementation (not just at plan time) — a silent `json:` tag typo (e.g. `sourceType` instead of `source_type`) will not fail Go tests but will silently break the frontend parser (`parseAskEvent` swallows JSON errors).

---

## Task 5: Wiring — config, router, server bootstrap

### Files
- Modify: `internal/config/config.go`
- Modify: `internal/api/router.go`
- Modify: `cmd/server/main.go`

### Config additions (mirrors existing `LLM_*`/`RERANKER_*` getenv pattern, `config.go:535-573`)

```go
AskTimeoutSeconds int // ASK_TIMEOUT_SECONDS, default 60
AskContextTopK    int // ASK_CONTEXT_TOP_K, default 8 (본검색 Limit)
AskContextInsightM int // ASK_CONTEXT_INSIGHT_M, default 3 (인사이트검색 Limit)
```

### Router additions

```go
// Server struct (router.go:32-) — add:
intentClassifier intent.Classifier // always non-nil, constructed from s.llmClient in NewServer or WithAskConfig
askTimeout       time.Duration
askTopK          int
askInsightM      int

// WithAskConfig sets ask-pipeline tuning knobs (Stage 2 result-count caps,
// Stage 3 timeout). Optional — zero values fall back to the same defaults
// as the config package (60s / 8 / 3) so tests that don't call this still
// get sane behavior.
func (s *Server) WithAskConfig(timeout time.Duration, topK, insightM int) *Server
```

`s.intentClassifier` is constructed once inside `NewServer` (`intent.NewLLMClassifier(llmClient)`) — not optional, since `Classify` always degrades safely even when `llmClient.Enabled() == false` (spec §4.4 covers this: LLM call failure → `KindGeneral` fallback).

### Route registration (inside the existing `requireAPIKey` group, `router.go:189-`)

```go
r.Post("/api/v1/ask", s.askHandler)
```
Placed unconditionally (not behind a `s.xxx != nil` guard) since `search`/`llmClient` are already non-optional `NewServer` params — same tier as `/api/v1/search`, not the same tier as `/api/v1/notes` (which IS conditional on `WithNotes`).

### Steps

- [ ] Add the 3 config fields + `getenv`/`strconv.Atoi` parsing with defaults, following the existing `llmMaxTokens` pattern (config.go:474-479).
- [ ] Add `Server.intentClassifier`/`askTimeout`/`askTopK`/`askInsightM` fields + defaults inside `NewServer` (60s/8/3 hardcoded fallback so `WithAskConfig` is truly optional).
- [ ] Add `WithAskConfig` builder.
- [ ] Register `r.Post("/api/v1/ask", s.askHandler)`.
- [ ] Update `cmd/server/main.go`'s `api.NewServer(...)` call chain to append `.WithAskConfig(time.Duration(cfg.AskTimeoutSeconds)*time.Second, cfg.AskContextTopK, cfg.AskContextInsightM)`.
- [ ] `go build ./...` — confirm no signature break to existing `NewServer(docs, svc, feedback, eval, llmClient, filesystemPath, apiKey)` callers (only `cmd/server/main.go` calls it, confirmed via grep — this plan does not change `NewServer`'s positional signature, only adds an optional builder call).

### Verification
`go build ./...` clean. `go test ./...` full suite green (confirms no existing test broke from the `Completer`-adjacent `StreamCompleter` addition — Task 1's non-breaking design should mean zero fakes need updating; verify this assumption by running the 6 previously-identified fake-`Completer` packages explicitly: `go test ./internal/curation/... ./internal/collector/... ./internal/search/... ./internal/worker/...`).

---

## End-to-end verification checklist (after all 5 tasks)

- [ ] `go build ./...` and `go vet ./...` clean.
- [ ] `go test ./...` full suite green, including the 6 packages with existing `Completer` fakes (confirms Task 1 was truly non-breaking).
- [ ] Manual `curl -N -X POST -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" -d '{"question":"..."}' http://localhost:PORT/api/v1/ask` against a dev instance — confirm frames arrive incrementally (not buffered until the connection closes) and `-N` (no-buffer) shows `event: sources` before any `event: token`.
- [ ] Field-by-field diff of the actual emitted JSON against `web/src/lib/sseEvents.ts` `parseAskEvent`'s expected shape (Task 4's own verification step, re-confirmed here at integration level).
- [ ] Confirm the frontend's `web/src/app/api/ask/route.ts` pass-through proxy (once unblocked per the capture-frontend plan) round-trips correctly — out of scope to implement here, but worth a manual smoke test since that plan explicitly deferred Task 8/9 pending this backend.

---

## Open Items (deferred, not blocking this plan)

1. **`OccurredFrom`/`OccurredTo` schema extension** (spec §4.5/§8.1) — `KindTemporal` stays a `Sort: "recent"` approximation until `model.SearchQuery` gains real date-range fields and the 5-CTE hybrid search SQL gets `WHERE occurred_at BETWEEN ...` clauses. Follow-up plan required.
2. **Exact-token (phone number) search fix** (spec §4.3/§8.3) — needs a normalized-phone index column, not fixable by intent/retrieval-layer changes. Follow-up plan required.
3. **`LLM_ASK_MODEL` dedicated model** — deferred; reusing `s.llmClient` for both Stage 1 and Stage 3 is sufficient for this ticket (see discrepancy #5).
4. **HyDE auto-enable for low-confidence short queries** (spec §5.2 last row) — explicitly optional in the spec ("권장, 필수 아님"); `UseHyDE` stays `false` in `assembleRetrieval` for this plan. Revisit after real query-log data from Stage 1 classification exists.
5. **`KindExactToken` user-facing messaging** (spec §8.2) — whether a failed exact-token lookup should surface distinct copy in the synthesized answer vs. generic `no_evidence` framing is left to prompt-tuning, not the wire contract (which stays 3-value `stop|error|no_evidence` unchanged).
