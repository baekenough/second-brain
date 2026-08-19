---
name: project-ask-multiturn
description: /api/v1/ask multi-turn conversation extension — query rewrite, turn persistence, conversation browsing endpoints (2026-08-17, uncommitted)
metadata:
  type: project
---

Extended `/api/v1/ask` (previously single-turn, see [[project_ask_date_context_fix]])
into a multi-turn chat backend on top of pre-existing, untouched
infrastructure: `migrations/024_ask_sessions.sql` + `internal/store/ask_sessions.go`
(`AskSessionStore.Insert/ListConversationTurns/ListRecentConversations/Get`).

**Shape** (2026-08-17, no commit made — task forbade it):
- New files: `internal/api/ask_history.go` (askHistoryTurn, askMaxHistoryTurns=6,
  recentAskHistory, resolveConversation, saveAskTurn), `internal/api/ask_rewrite.go`
  (rewriteStandaloneQuestion — LLM query compression, fail-open to raw question),
  `internal/api/ask_conversations.go` (AskConversationStore interface,
  WithAskSessions builder, GET /api/v1/ask/conversations[,/{id}] handlers).
- `ask.go`: AskRequest gained ConversationID; askHandler now emits a new
  "conversation" SSE event (conversation_id/turn_index) BEFORE "sources" —
  the 4 pre-existing events (sources/token/done/error) were NOT touched, only
  a 5th event added (frontend's parseAskEvent silently ignores unknown event
  names, per web/src/lib/sseEvents.ts, so this is non-breaking to the shipped
  frontend). `synthesize`/`buildAskMessages` gained a history param and
  synthesize now returns (finishReason, answer) instead of just finishReason,
  so askHandler can persist the full generated answer.
- `cmd/server/main.go`: wired `store.NewAskSessionStore(pg)` +
  `.WithAskSessions(askSessionStore)` — this file was NOT in the task's
  forbidden list, and without this wiring the feature would compile but never
  actually persist in production (Server.askSessions defaults to nil →
  single-turn-only degraded mode, see resolveConversation's doc comment).

**Key design decisions** (see code doc comments for full rationale):
1. History replayed into Stage 3 prompt as genuine alternating user/assistant
   `llm.Message` turns (question text, answer text — NEVER source-document
   bodies from previous turns). This is structural, not just convention:
   askHistoryTurn only has Question/Answer fields, so there is no code path
   that could leak a prior turn's Sources into the prompt.
2. Rewrite (query compression for search/intent-classification only, never
   shown to the user or to Stage 3 synthesis) fails open at every step: no
   history → skip; LLM disabled → skip; error → fallback to raw question;
   empty response → fallback. A rewrite can never make search worse than no
   rewrite.
3. History cap: askMaxHistoryTurns = 6 (recent-N, not char-budget) — same
   cap reused for both the rewrite prompt and the synthesis prompt.
4. Turn persistence ordering: SSE "done" is written to the client BEFORE
   saveAskTurn runs, and saveAskTurn swallows all its own errors — a DB
   failure can never affect the client response (tested:
   TestAskHandler_SaveFailure_DoesNotBreakResponse).
5. Client-disconnect (context.Canceled) mid-stream: chosen to SAVE the
   partial answer (finish_reason="error") when at least one token was
   produced, using `context.WithoutCancel(ctx)` + a fresh 5s timeout so the
   already-canceled request context doesn't kill the Insert too. An empty
   partial (canceled before any token) is not saved — no turn_index spent
   for nothing.
6. No turn is persisted for the retrieval-failure early-return path
   (assembleRetrieval error, before "sources" is even known) — judged as a
   pure infra hiccup with nothing worth replaying into future turns.

Verification: TDD in the mechanical sense for the pre-existing call-site
changes (go vet caught the buildAskMessages arity break before the fix was
applied — captured as the "red" checkpoint), but the new
history/rewrite/persistence/conversations-endpoint code was written together
with its ~20 new tests (interface-first, not strictly red-green per test) —
worth flagging honestly if this pattern recurs and someone wants a stricter
TDD trail next time.
`go build ./...`, `go vet ./...`, `go test ./internal/api/...` and
`go test ./...` (full repo) all green. `gofmt -l` clean on every touched file
except `cmd/server/main.go`, which has a PRE-EXISTING import-ordering gofmt
diff unrelated to this change (verified via `git stash` — the diff exists on
the pre-task base commit too).

Forbidden paths were not touched: confirmed via `git status --short`
restricted to `internal/`, `cmd/`, `migrations/` — only ask.go, ask_test.go,
router.go (modified), the 4 new ask_*.go files, and cmd/server/main.go
changed; `internal/store/ask_sessions.go` and `migrations/024_*.sql` remain
byte-identical to what the task handed over (never opened with Write/Edit).
No `git add`/commit was performed per instructions.
