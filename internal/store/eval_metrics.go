package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// EvalMetricsRecord represents a single eval run stored in the eval_metrics table.
type EvalMetricsRecord struct {
	ID     int64
	NDCG5  float64
	NDCG10 float64
	MRR10  float64
	Pairs  int
	RunAt  time.Time
}

// EvalMetricsStore persists and retrieves eval run metrics.
type EvalMetricsStore struct {
	pg *Postgres
}

// NewEvalMetricsStore returns an EvalMetricsStore backed by the given Postgres instance.
func NewEvalMetricsStore(pg *Postgres) *EvalMetricsStore {
	return &EvalMetricsStore{pg: pg}
}

// Save inserts a new eval metrics record. The RunAt field is set by the database
// DEFAULT (NOW()) so callers do not need to populate it.
func (s *EvalMetricsStore) Save(ctx context.Context, rec EvalMetricsRecord) error {
	_, err := s.pg.Pool().Exec(ctx,
		`INSERT INTO eval_metrics (ndcg5, ndcg10, mrr10, pairs) VALUES ($1, $2, $3, $4)`,
		rec.NDCG5, rec.NDCG10, rec.MRR10, rec.Pairs,
	)
	if err != nil {
		return fmt.Errorf("eval metrics: save: %w", err)
	}
	return nil
}

// List returns the most recent eval metrics records ordered by run_at DESC.
// limit controls the maximum number of records returned; pass 0 to use the
// default of 20 and values above 100 are capped at 100.
func (s *EvalMetricsStore) List(ctx context.Context, limit int) ([]EvalMetricsRecord, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := s.pg.Pool().Query(ctx,
		`SELECT id, ndcg5, ndcg10, mrr10, pairs, run_at
		 FROM eval_metrics
		 ORDER BY run_at DESC
		 LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("eval metrics: list: %w", err)
	}
	records, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (EvalMetricsRecord, error) {
		var rec EvalMetricsRecord
		return rec, row.Scan(&rec.ID, &rec.NDCG5, &rec.NDCG10, &rec.MRR10, &rec.Pairs, &rec.RunAt)
	})
	if err != nil {
		return nil, fmt.Errorf("eval metrics: list: collect: %w", err)
	}
	return records, nil
}

// Latest returns the most recent eval metrics record.
// Returns nil, nil when the table is empty — this is not an error condition.
func (s *EvalMetricsStore) Latest(ctx context.Context) (*EvalMetricsRecord, error) {
	var rec EvalMetricsRecord
	err := s.pg.Pool().QueryRow(ctx,
		`SELECT id, ndcg5, ndcg10, mrr10, pairs, run_at
		 FROM eval_metrics
		 ORDER BY run_at DESC
		 LIMIT 1`,
	).Scan(&rec.ID, &rec.NDCG5, &rec.NDCG10, &rec.MRR10, &rec.Pairs, &rec.RunAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("eval metrics: latest: %w", err)
	}
	return &rec, nil
}
