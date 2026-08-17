package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// askHistoryTurn is the trimmed-down shape of a previous conversation turn
// replayed into the rewrite prompt (ask_rewrite.go) and the Stage 3
// synthesis prompt (buildAskMessages). It deliberately carries only
// question/answer TEXT — never Sources, FinishReason, or any other field
// from store.AskSession — because those are the only two fields either
// prompt is allowed to see (see buildAskMessages' doc comment for why
// source-document bodies must never be replayed).
type askHistoryTurn struct {
	Question string
	Answer   string
}

// askMaxHistoryTurns caps how many previous turns are replayed into both
// the query-rewrite prompt and the Stage 3 synthesis prompt.
//
// Only question/answer TEXT is replayed (never source-document bodies —
// see buildAskMessages), so even at this cap the added context grows with
// conversation LENGTH, not with how much evidence backed each answer.
// Still, an unbounded conversation would eventually dominate the prompt
// budget and drown out the current turn's actual retrieved evidence, so a
// cap is required regardless. 6 is a deliberately conservative starting
// point: it keeps a realistic multi-topic follow-up chain fully coherent
// (this spec's own follow-up examples — "그거 더 자세히", "그럼 그 사람은?" — are
// 1-2 turns deep) while bounding worst-case prompt growth to a small,
// predictable number of turns even for a long-running chat session. Raise
// this only alongside an actual token-budget accounting pass, not casually
// — see buildAskRewritePrompt/buildAskMessages for where this cap is spent.
const askMaxHistoryTurns = 6

// recentAskHistory trims turns (already ordered turn_index ASC, per
// AskSessionStore.ListConversationTurns) down to the most recent max
// entries and projects them to askHistoryTurn.
func recentAskHistory(turns []store.AskSession, max int) []askHistoryTurn {
	if max > 0 && len(turns) > max {
		turns = turns[len(turns)-max:]
	}
	history := make([]askHistoryTurn, 0, len(turns))
	for _, t := range turns {
		history = append(history, askHistoryTurn{Question: t.Question, Answer: t.Answer})
	}
	return history
}

// resolveConversation determines the conversation_id/turn_index for this
// /ask request and loads capped recent history for the rewrite/synthesis
// stages.
//
//   - No conversation_id in the request, or one that fails to parse as a
//     UUID: starts a brand-new conversation (fresh server-generated uuid,
//     turn 0, no history). An unparsable id is treated identically to "none
//     supplied" rather than rejected with a 400 — a stale/mistyped
//     client-supplied id must not block the user from asking a question.
//   - s.askSessions == nil (server not wired with conversation persistence):
//     behaves like a fresh conversation on every call — turn_index is
//     always 0 and history is always empty, since there is nowhere to read
//     prior turns from. /ask still answers correctly in this configuration,
//     just single-turn (no cross-request memory).
//   - History fetch failure (DB error): logged and treated as "no history"
//     rather than failing the whole request — this one request degrades to
//     a single-turn answer instead of erroring out, matching the "재작성
//     실패 시 원문으로 폴백" fail-open philosophy applied one layer up.
func (s *Server) resolveConversation(ctx context.Context, requestedID string) (conversationID uuid.UUID, turnIndex int, history []askHistoryTurn) {
	if requestedID != "" {
		if parsed, err := uuid.Parse(requestedID); err == nil {
			conversationID = parsed
		}
	}
	if conversationID == uuid.Nil {
		conversationID = uuid.New()
	}
	if s.askSessions == nil {
		return conversationID, 0, nil
	}

	turns, err := s.askSessions.ListConversationTurns(ctx, conversationID)
	if err != nil {
		slog.Error("ask: failed to load conversation history, continuing single-turn",
			"error", err, "conversation_id", conversationID)
		return conversationID, 0, nil
	}
	return conversationID, len(turns), recentAskHistory(turns, askMaxHistoryTurns)
}

// saveAskTurn persists one conversation turn best-effort: a storage failure
// is logged and swallowed, never surfaced to the client, because by the
// time askHandler calls this the SSE stream has already delivered its
// "done"/final event (or the connection is already gone) — there is no
// remaining channel to report a save failure back to the caller through,
// and failing the whole request over it would be strictly worse than an
// answer the user already has but that simply isn't saved for next time.
//
// When ctx is already canceled/expired (client disconnected mid-stream,
// server-side askTimeout hit), Insert is retried against a short-lived
// detached context instead of the dead one — otherwise every disconnect
// would also silently skip persistence, defeating the point of saving a
// partial answer at all (see askHandler's finishReason=="" branch).
func (s *Server) saveAskTurn(ctx context.Context, conversationID uuid.UUID, turnIndex int, question, answer, finishReason string, sources []AskSourceItem) {
	if s.askSessions == nil {
		return
	}

	saveCtx := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		saveCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
	}

	storeSources := make([]store.AskSource, 0, len(sources))
	for _, src := range sources {
		storeSources = append(storeSources, store.AskSource{
			ID: src.ID, Title: src.Title, SourceType: src.SourceType, Score: src.Score,
		})
	}

	if _, err := s.askSessions.Insert(saveCtx, store.AskSession{
		ConversationID: conversationID,
		TurnIndex:      turnIndex,
		Question:       question,
		Answer:         answer,
		FinishReason:   finishReason,
		Sources:        storeSources,
	}); err != nil {
		slog.Error("ask: failed to save conversation turn",
			"error", err, "conversation_id", conversationID, "turn_index", turnIndex)
	}
}
