package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/baekenough/second-brain/internal/model"
)

// ActionStore provides persistence for actions + action_status (migration 022).
type ActionStore struct {
	pg *Postgres
}

// NewActionStore returns an ActionStore backed by the given Postgres instance.
func NewActionStore(pg *Postgres) *ActionStore {
	return &ActionStore{pg: pg}
}

// UpsertAction inserts or updates an action row keyed by identity_key (spec
// §5.5/§7.2). On conflict, detected_by is merged rather than overwritten:
// a structural candidate followed later by an LLM candidate for the SAME
// identity_key (or vice versa) becomes "both"; the same source re-asserting
// the same identity_key is a no-op on detected_by. Every other field is
// refreshed from the latest observation (EXCLUDED) — this is NOT a
// RETURNING clause, so referencing EXCLUDED here is safe (this project's
// feedback_postgres_returning_excluded lesson is specifically about
// RETURNING, not SET).
//
// Caller MUST call this before EnsureOpenStatus for the same identity_key —
// action_status.identity_key has an FK to actions.identity_key.
//
// counterpart_entity_id is resolved here rather than by the caller: the
// structural signal worker holds no entity store, so before this it wrote
// every awaiting_my_reply action with a NULL counterpart and the /actions
// screen had no name to render. A caller states the label
// (model.Action.CounterpartName) and the store canonicalises it through the
// same (normalized_name, type) dedup every other entity goes through. On
// conflict the column is COALESCEd, never overwritten: a re-assertion that
// carries no label must not blank a resolved counterpart, and — the reason the
// existing NULL rows repair themselves — a row written before this change
// picks the id up on the worker's next tick.
func (s *ActionStore) UpsertAction(ctx context.Context, a model.Action) error {
	counterpartID := s.resolveCounterpart(ctx, a)

	const q = `
		INSERT INTO actions (identity_key, document_id, thread_key, kind, summary, counterpart_entity_id, due_at, detected_by, confidence, observed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (identity_key) DO UPDATE SET
			document_id = EXCLUDED.document_id,
			summary     = EXCLUDED.summary,
			due_at      = EXCLUDED.due_at,
			confidence  = EXCLUDED.confidence,
			observed_at = EXCLUDED.observed_at,
			counterpart_entity_id = COALESCE(EXCLUDED.counterpart_entity_id, actions.counterpart_entity_id),
			detected_by = CASE
				WHEN actions.detected_by = EXCLUDED.detected_by THEN actions.detected_by
				ELSE 'both'
			END`
	_, err := s.pg.pool.Exec(ctx, q,
		a.IdentityKey, a.DocumentID, a.ThreadKey, string(a.Kind), a.Summary,
		counterpartID, a.DueAt, string(a.DetectedBy), a.Confidence, a.ObservedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert action %s: %w", a.IdentityKey, err)
	}
	return nil
}

// resolveCounterpart turns a.CounterpartName into an entities.id, or returns
// a.CounterpartEntityID unchanged when the caller already resolved one.
//
// A failed resolution is logged and swallowed on purpose: the action itself is
// what the user needs to see, and losing the whole row because one label could
// not be filed would be a strictly worse outcome than a card with no name.
func (s *ActionStore) resolveCounterpart(ctx context.Context, a model.Action) *int64 {
	if a.CounterpartEntityID != nil {
		return a.CounterpartEntityID
	}
	if strings.TrimSpace(a.CounterpartName) == "" {
		return nil
	}
	entityType := a.CounterpartType
	if entityType == "" {
		entityType = model.EntityTypePerson
	}
	id, err := NewEntityStore(s.pg).UpsertEntity(ctx, a.CounterpartName, entityType)
	if err != nil {
		// Only the CAUSE is logged, never err itself: UpsertEntity formats the
		// entity name into its message, and a counterpart name is personal
		// data that must not reach the log. Unwrap yields the database error
		// (or nil for the "empty after normalization" sentinel, which carries
		// no name).
		cause := errors.Unwrap(err)
		if cause == nil {
			cause = err
		}
		slog.Warn("resolve action counterpart entity failed; action keeps a NULL counterpart",
			"identity_key", a.IdentityKey, "entity_type", entityType, "error", cause)
		return nil
	}
	return &id
}

// EnsureOpenStatus inserts an 'open' action_status row for identityKey if
// none exists yet. ON CONFLICT DO NOTHING is the entire point (spec §5.5):
// a user's prior 'done'/'ignored' decision must survive re-extraction —
// this method is only ever called AFTER UpsertAction succeeds for the same
// identityKey (satisfying the FK), and never overwrites an existing state.
func (s *ActionStore) EnsureOpenStatus(ctx context.Context, identityKey string) error {
	const q = `INSERT INTO action_status (identity_key, state) VALUES ($1, 'open') ON CONFLICT (identity_key) DO NOTHING`
	_, err := s.pg.pool.Exec(ctx, q, identityKey)
	if err != nil {
		return fmt.Errorf("ensure open status %s: %w", identityKey, err)
	}
	return nil
}
