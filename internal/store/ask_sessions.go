package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AskSource mirrors internal/api.AskSourceItem's wire shape (id, title,
// source_type, score, occurred_at) — that struct lives in internal/api,
// which this package must not depend on (store is a lower layer than api),
// so the shape is duplicated here rather than imported. Keep field
// names/JSON tags in sync with internal/api/ask.go's AskSourceItem if
// either changes.
//
// OccurredAt was added after ask_sessions rows already existed (issue
// #218); no migration was needed because this struct is marshaled straight
// into the sources JSONB column (migration 024) rather than its own SQL
// columns. Rows written before this change have no "occurred_at" key at
// all, so json.Unmarshal leaves OccurredAt as its zero value, nil — the
// same value a row written after this change uses for "no known event
// time". A pre-existing row therefore silently reads back as "no event
// time" rather than erroring, which is the correct degradation: no
// backfill can recover a value that was never captured.
type AskSource struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	SourceType string     `json:"source_type"`
	Score      float64    `json:"score"`
	OccurredAt *time.Time `json:"occurred_at"`
}

// AskSession represents one row in ask_sessions (migration 024): a single
// turn (one question/answer exchange) within a multi-turn conversation,
// persisted so a page refresh does not lose the answer. TurnIndex is 0-based
// and unique within ConversationID (see migration 024's UNIQUE constraint).
// There is deliberately no separate "conversations" table — the first turn's
// Question doubles as the conversation's title (YAGNI; add one later if
// conversation-level metadata becomes necessary).
type AskSession struct {
	ID             uuid.UUID
	ConversationID uuid.UUID
	TurnIndex      int
	Question       string
	Answer         string
	FinishReason   string // "stop" | "error" | "no_evidence"
	Sources        []AskSource
	CreatedAt      time.Time
}

// AskSessionStore provides persistence for ask_sessions (migration 024).
type AskSessionStore struct {
	pg *Postgres
}

// NewAskSessionStore returns an AskSessionStore backed by the given Postgres instance.
func NewAskSessionStore(pg *Postgres) *AskSessionStore {
	return &AskSessionStore{pg: pg}
}

// Insert records one conversation turn and returns the persisted row with
// id/created_at filled in from the database (id is DB-generated via
// gen_random_uuid() when session.ID is uuid.Nil; a caller-supplied ID is
// honored otherwise). The caller is responsible for supplying
// ConversationID and TurnIndex — Insert does not compute the next turn
// index itself, since the caller (the /ask handler) already knows the
// conversation's current length from the request context.
//
// The UNIQUE(conversation_id, turn_index) constraint (migration 024)
// surfaces as a plain error here if the caller races two writers for the
// same turn; it is not treated as a special "ok, already inserted" case
// because, unlike UpsertAction/UpsertEntityRelations, a turn collision
// means a real logic error upstream (two answers claiming the same turn),
// not a harmless re-observation.
//
// Sources defaults to an empty slice (never nil) before marshaling so the
// stored JSONB is always a valid array, matching the column's
// NOT NULL DEFAULT '[]'::jsonb.
func (s *AskSessionStore) Insert(ctx context.Context, session AskSession) (AskSession, error) {
	if session.Sources == nil {
		session.Sources = []AskSource{}
	}
	sourcesJSON, err := json.Marshal(session.Sources)
	if err != nil {
		return AskSession{}, fmt.Errorf("ask session insert: marshal sources: %w", err)
	}

	const q = `
		INSERT INTO ask_sessions (id, conversation_id, turn_index, question, answer, finish_reason, sources)
		VALUES (COALESCE(NULLIF($1, '00000000-0000-0000-0000-000000000000'::uuid), gen_random_uuid()), $2, $3, $4, $5, $6, $7::jsonb)
		RETURNING id, created_at`

	row := s.pg.pool.QueryRow(ctx, q,
		session.ID, session.ConversationID, session.TurnIndex,
		session.Question, session.Answer, session.FinishReason, string(sourcesJSON),
	)
	if err := row.Scan(&session.ID, &session.CreatedAt); err != nil {
		return AskSession{}, fmt.Errorf("ask session insert: %w", err)
	}
	return session, nil
}

// ListConversationTurns returns every turn of one conversation ordered by
// turn_index ASC — used both to rebuild multi-turn LLM context (question 1,
// answer 1, question 2, answer 2, ...) and to restore the /ask screen after
// a page refresh.
func (s *AskSessionStore) ListConversationTurns(ctx context.Context, conversationID uuid.UUID) ([]AskSession, error) {
	const q = `
		SELECT id, conversation_id, turn_index, question, answer, finish_reason, sources, created_at
		FROM ask_sessions
		WHERE conversation_id = $1
		ORDER BY turn_index ASC`

	rows, err := s.pg.pool.Query(ctx, q, conversationID)
	if err != nil {
		return nil, fmt.Errorf("ask session list conversation turns %s: %w", conversationID, err)
	}
	defer rows.Close()

	var results []AskSession
	for rows.Next() {
		session, err := scanAskSession(rows)
		if err != nil {
			return nil, fmt.Errorf("ask session list conversation turns scan: %w", err)
		}
		results = append(results, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ask session list conversation turns iter: %w", err)
	}
	return results, nil
}

// ListRecentConversations returns the latest turn of each conversation,
// ordered by that turn's created_at DESC — the /ask screen's conversation
// list view (one row per conversation, most recently active first).
//
// Implemented with DISTINCT ON (conversation_id) rather than a
// ROW_NUMBER() window function: verified via EXPLAIN ANALYZE on real
// Postgres (Mac mini prod, rolled back afterward) against migration 024's
// indexes, with synthetic data (500 conversations x 10 turns = 5,003 rows).
// Both plans do the same Seq Scan + Sort on (conversation_id, created_at
// DESC) underneath; DISTINCT ON's planner cost was lower (475.12..475.24
// vs 596.61..596.67) because Unique is a single pass over the sorted rows
// with no per-row window-function bookkeeping, while WindowAgg materializes
// a rn column before the outer filter discards all but rn=1. At this data
// size actual execution time was statistically indistinguishable (1.96ms
// DISTINCT ON vs 1.87ms window — within noise); DISTINCT ON was chosen for
// its lower, more predictable cost estimate as conversation volume grows,
// not for a measured speed win at this scale.
func (s *AskSessionStore) ListRecentConversations(ctx context.Context, limit int) ([]AskSession, error) {
	if limit <= 0 {
		limit = 50
	}

	const q = `
		SELECT id, conversation_id, turn_index, question, answer, finish_reason, sources, created_at
		FROM (
			SELECT DISTINCT ON (conversation_id)
				id, conversation_id, turn_index, question, answer, finish_reason, sources, created_at
			FROM ask_sessions
			ORDER BY conversation_id, created_at DESC
		) latest_turn
		ORDER BY created_at DESC
		LIMIT $1`

	rows, err := s.pg.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("ask session list recent conversations: %w", err)
	}
	defer rows.Close()

	var results []AskSession
	for rows.Next() {
		session, err := scanAskSession(rows)
		if err != nil {
			return nil, fmt.Errorf("ask session list recent conversations scan: %w", err)
		}
		results = append(results, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ask session list recent conversations iter: %w", err)
	}
	return results, nil
}

// Get retrieves a single ask session turn by id. Returns nil, nil when no
// row matches — this is not an error condition (mirrors
// EvalMetricsStore.Latest), leaving 404-vs-500 handling to the caller.
func (s *AskSessionStore) Get(ctx context.Context, id uuid.UUID) (*AskSession, error) {
	const q = `
		SELECT id, conversation_id, turn_index, question, answer, finish_reason, sources, created_at
		FROM ask_sessions
		WHERE id = $1`

	row := s.pg.pool.QueryRow(ctx, q, id)
	session, err := scanAskSession(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ask session get %s: %w", id, err)
	}
	return &session, nil
}

// --- scan helpers ---

type askSessionScanner interface {
	Scan(dest ...any) error
}

func scanAskSession(row askSessionScanner) (AskSession, error) {
	var (
		session     AskSession
		sourcesJSON []byte
	)
	if err := row.Scan(
		&session.ID, &session.ConversationID, &session.TurnIndex,
		&session.Question, &session.Answer, &session.FinishReason,
		&sourcesJSON, &session.CreatedAt,
	); err != nil {
		return AskSession{}, err
	}
	if err := json.Unmarshal(sourcesJSON, &session.Sources); err != nil {
		return AskSession{}, fmt.Errorf("unmarshal sources: %w", err)
	}
	return session, nil
}
