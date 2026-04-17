package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ReindexState represents a single reindex event persisted in the reindex_state table.
type ReindexState struct {
	ID                int64
	LastReindexAt     time.Time
	DocCountAtReindex int
	TriggerReason     string
}

// ReindexStateStore persists and retrieves reindex events.
type ReindexStateStore struct {
	pg *Postgres
}

// NewReindexStateStore returns a ReindexStateStore backed by the given Postgres instance.
func NewReindexStateStore(pg *Postgres) *ReindexStateStore {
	return &ReindexStateStore{pg: pg}
}

// Save inserts a new reindex event. The last_reindex_at field is set by the
// database DEFAULT (NOW()) so callers do not need to populate it.
func (s *ReindexStateStore) Save(ctx context.Context, state ReindexState) error {
	_, err := s.pg.Pool().Exec(ctx,
		`INSERT INTO reindex_state (doc_count_at_reindex, trigger_reason) VALUES ($1, $2)`,
		state.DocCountAtReindex, state.TriggerReason,
	)
	if err != nil {
		return fmt.Errorf("reindex state: save: %w", err)
	}
	return nil
}

// Latest returns the most recent reindex event.
// Returns nil, nil when the table is empty — this is not an error condition.
func (s *ReindexStateStore) Latest(ctx context.Context) (*ReindexState, error) {
	var st ReindexState
	err := s.pg.Pool().QueryRow(ctx,
		`SELECT id, last_reindex_at, doc_count_at_reindex, trigger_reason
		 FROM reindex_state
		 ORDER BY last_reindex_at DESC
		 LIMIT 1`,
	).Scan(&st.ID, &st.LastReindexAt, &st.DocCountAtReindex, &st.TriggerReason)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("reindex state: latest: %w", err)
	}
	return &st, nil
}
