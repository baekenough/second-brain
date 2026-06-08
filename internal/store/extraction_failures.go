package store

import (
	"context"
	"time"

	"github.com/baekenough/second-brain/internal/durable"
	"github.com/jackc/pgx/v5"
)

// ExtractionFailure represents a single row in the extraction_failures table.
type ExtractionFailure struct {
	ID           int64
	SourceType   string
	SourceID     string
	FilePath     string
	ErrorMessage string
	Attempts     int
	NextRetryAt  time.Time
	DeadLetter   bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ExtractionFailureStore provides persistence for extraction failure records.
type ExtractionFailureStore struct {
	pg *Postgres
}

// NewExtractionFailureStore returns an ExtractionFailureStore backed by the given Postgres instance.
func NewExtractionFailureStore(pg *Postgres) *ExtractionFailureStore {
	return &ExtractionFailureStore{pg: pg}
}

// Record inserts a new failure row, or increments attempts on an existing one.
//
// Back-off and dead-letter logic is delegated to [durable.ExtractionBackoff]
// so that the policy can be unit-tested without a running Postgres instance.
// The SQL upsert now receives explicit nextRetryAt and deadLetter values
// computed in Go, keeping the database as a dumb store.
//
// Back-off schedule (matches the previous inline SQL formula):
//
//	attempt 0 (first insert): +5 minutes
//	attempt n (ON CONFLICT):  +LEAST(60, 2^n) minutes
//
// Dead-letter threshold: 10 attempts (i.e. currentAttempts+1 >= 10).
func (s *ExtractionFailureStore) Record(ctx context.Context, f ExtractionFailure) error {
	now := time.Now()
	nextRetryAt, deadLetter := durable.ExtractionBackoff.NextRetryAt(f.Attempts, now)

	_, err := s.pg.pool.Exec(ctx, `
		INSERT INTO extraction_failures
			(source_type, source_id, file_path, error_message, attempts, next_retry_at, dead_letter)
		VALUES ($1, $2, $3, $4, 1, $5, $6)
		ON CONFLICT (source_type, source_id) DO UPDATE
		SET attempts      = extraction_failures.attempts + 1,
		    error_message = EXCLUDED.error_message,
		    next_retry_at = EXCLUDED.next_retry_at,
		    dead_letter   = EXCLUDED.dead_letter,
		    updated_at    = now()
	`, f.SourceType, f.SourceID, f.FilePath, f.ErrorMessage, nextRetryAt, deadLetter)
	return err
}

// DueForRetry returns up to limit rows whose next_retry_at has elapsed and are
// not yet dead-lettered, ordered by next_retry_at ascending (oldest-first).
func (s *ExtractionFailureStore) DueForRetry(ctx context.Context, limit int) ([]ExtractionFailure, error) {
	rows, err := s.pg.pool.Query(ctx, `
		SELECT id, source_type, source_id, file_path, error_message,
		       attempts, next_retry_at, dead_letter, created_at, updated_at
		FROM extraction_failures
		WHERE NOT dead_letter
		  AND next_retry_at <= now()
		ORDER BY next_retry_at
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ExtractionFailure, error) {
		var f ExtractionFailure
		err := row.Scan(
			&f.ID, &f.SourceType, &f.SourceID, &f.FilePath, &f.ErrorMessage,
			&f.Attempts, &f.NextRetryAt, &f.DeadLetter, &f.CreatedAt, &f.UpdatedAt,
		)
		return f, err
	})
}

// Resolve deletes the failure record for (sourceType, sourceID) after a
// successful re-extraction. A no-op when the record does not exist.
func (s *ExtractionFailureStore) Resolve(ctx context.Context, sourceType, sourceID string) error {
	_, err := s.pg.pool.Exec(ctx,
		`DELETE FROM extraction_failures WHERE source_type = $1 AND source_id = $2`,
		sourceType, sourceID,
	)
	return err
}
