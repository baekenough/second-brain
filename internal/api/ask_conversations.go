package api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/baekenough/second-brain/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// AskConversationStore is the subset of *store.AskSessionStore used by the
// multi-turn ask pipeline: persisting each turn (askHandler via
// saveAskTurn, ask_history.go) and serving the two read-only
// conversation-browsing routes below. Kept narrow — mirroring
// DocumentStore/FeedbackRecorder's existing interface-per-need convention
// in this package — so tests can inject an in-memory fake instead of a
// real Postgres-backed *store.AskSessionStore.
type AskConversationStore interface {
	Insert(ctx context.Context, session store.AskSession) (store.AskSession, error)
	ListConversationTurns(ctx context.Context, conversationID uuid.UUID) ([]store.AskSession, error)
	ListRecentConversations(ctx context.Context, limit int) ([]store.AskSession, error)
}

// defaultAskConversationsLimit mirrors AskSessionStore.ListRecentConversations'
// own internal default (store/ask_sessions.go) — kept in sync here so an
// omitted/invalid ?limit query param on GET /api/v1/ask/conversations
// behaves identically to calling the store method with limit<=0 directly.
const defaultAskConversationsLimit = 50

// WithAskSessions enables conversation persistence for POST /api/v1/ask
// (turn history + query rewrite, ask_history.go) and registers
// GET /api/v1/ask/conversations and GET /api/v1/ask/conversations/{id}.
// Optional — a Server that never calls this still serves /api/v1/ask, just
// without cross-request memory: every request behaves like the first turn
// of a brand-new conversation (see resolveConversation), and the two GET
// routes are not registered at all (mirrors how WithNotes/WithIngestFile
// gate their own optional routes in router.go).
func (s *Server) WithAskSessions(sessions AskConversationStore) *Server {
	s.askSessions = sessions
	return s
}

// askConversationSummary is one element of the GET /api/v1/ask/conversations
// response body: the latest turn of a conversation, letting the client
// render a title (question) and preview without a second round-trip per
// conversation.
type askConversationSummary struct {
	ConversationID string    `json:"conversation_id"`
	TurnIndex      int       `json:"turn_index"`
	Question       string    `json:"question"`
	Answer         string    `json:"answer"`
	FinishReason   string    `json:"finish_reason"`
	CreatedAt      time.Time `json:"created_at"`
}

// askConversationTurn is one element of the
// GET /api/v1/ask/conversations/{id} response body: a full turn, including
// its sources (unlike askConversationSummary, which omits them — the list
// view does not need per-conversation evidence, only the detail view does).
type askConversationTurn struct {
	ID           string          `json:"id"`
	TurnIndex    int             `json:"turn_index"`
	Question     string          `json:"question"`
	Answer       string          `json:"answer"`
	FinishReason string          `json:"finish_reason"`
	Sources      []AskSourceItem `json:"sources"`
	CreatedAt    time.Time       `json:"created_at"`
}

func toAskConversationSummary(session store.AskSession) askConversationSummary {
	return askConversationSummary{
		ConversationID: session.ConversationID.String(),
		TurnIndex:      session.TurnIndex,
		Question:       session.Question,
		Answer:         session.Answer,
		FinishReason:   session.FinishReason,
		CreatedAt:      session.CreatedAt,
	}
}

func toAskConversationTurn(session store.AskSession) askConversationTurn {
	return askConversationTurn{
		ID:           session.ID.String(),
		TurnIndex:    session.TurnIndex,
		Question:     session.Question,
		Answer:       session.Answer,
		FinishReason: session.FinishReason,
		Sources:      toAskSourceItems(session.Sources),
		CreatedAt:    session.CreatedAt,
	}
}

// toAskSourceItems converts store.AskSource (persistence shape) to
// AskSourceItem (wire shape) — the two are duplicated by design, see
// store.AskSource's doc comment for why store cannot import internal/api.
func toAskSourceItems(sources []store.AskSource) []AskSourceItem {
	items := make([]AskSourceItem, 0, len(sources))
	for _, src := range sources {
		items = append(items, AskSourceItem{
			ID: src.ID, Title: src.Title, SourceType: src.SourceType, Score: src.Score,
			OccurredAt: src.OccurredAt,
		})
	}
	return items
}

// askConversationsHandler handles GET /api/v1/ask/conversations?limit=N:
// the latest turn of each conversation, most recently active first. Only
// registered when s.askSessions != nil (see WithAskSessions), so
// s.askSessions is guaranteed non-nil inside this handler.
func (s *Server) askConversationsHandler(w http.ResponseWriter, r *http.Request) {
	limit := defaultAskConversationsLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
		// A non-numeric or <=0 limit is treated as "not provided" (falls
		// back to the default) rather than a 400 — mirrors
		// resolveConversation's "malformed input degrades gracefully"
		// philosophy for this same handler family.
	}

	conversations, err := s.askSessions.ListRecentConversations(r.Context(), limit)
	if err != nil {
		slog.Error("ask: list recent conversations failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list conversations")
		return
	}

	out := make([]askConversationSummary, 0, len(conversations))
	for _, c := range conversations {
		out = append(out, toAskConversationSummary(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// askConversationDetailHandler handles GET /api/v1/ask/conversations/{id}:
// every turn of one conversation, ordered turn_index ASC. Only registered
// when s.askSessions != nil (see WithAskSessions).
func (s *Server) askConversationDetailHandler(w http.ResponseWriter, r *http.Request) {
	conversationID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid conversation id")
		return
	}

	turns, err := s.askSessions.ListConversationTurns(r.Context(), conversationID)
	if err != nil {
		slog.Error("ask: list conversation turns failed", "error", err, "conversation_id", conversationID)
		writeError(w, http.StatusInternalServerError, "failed to load conversation")
		return
	}
	if len(turns) == 0 {
		writeError(w, http.StatusNotFound, "conversation not found")
		return
	}

	out := make([]askConversationTurn, 0, len(turns))
	for _, t := range turns {
		out = append(out, toAskConversationTurn(t))
	}
	writeJSON(w, http.StatusOK, out)
}
