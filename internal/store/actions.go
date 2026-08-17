package store

import (
	"context"
	"fmt"

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
func (s *ActionStore) UpsertAction(ctx context.Context, a model.Action) error {
	const q = `
		INSERT INTO actions (identity_key, document_id, thread_key, kind, summary, counterpart_entity_id, due_at, detected_by, confidence, observed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (identity_key) DO UPDATE SET
			document_id = EXCLUDED.document_id,
			summary     = EXCLUDED.summary,
			due_at      = EXCLUDED.due_at,
			confidence  = EXCLUDED.confidence,
			observed_at = EXCLUDED.observed_at,
			detected_by = CASE
				WHEN actions.detected_by = EXCLUDED.detected_by THEN actions.detected_by
				ELSE 'both'
			END`
	_, err := s.pg.pool.Exec(ctx, q,
		a.IdentityKey, a.DocumentID, a.ThreadKey, string(a.Kind), a.Summary,
		a.CounterpartEntityID, a.DueAt, string(a.DetectedBy), a.Confidence, a.ObservedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert action %s: %w", a.IdentityKey, err)
	}
	return nil
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
