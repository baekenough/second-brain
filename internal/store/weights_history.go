package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/jackc/pgx/v5"
)

// Lifecycle of a candidate in search_weights_history.
//
//	proposed ──promote──> active ──promote(other)──> superseded ──rollback──> active
//	                         └────────rollback────────> rolled_back
const (
	// WeightsStatusProposed is a candidate that was measured but not promoted.
	// Refused candidates are recorded too — otherwise there is no way to ask
	// later why the weights did not change.
	WeightsStatusProposed = "proposed"
	// WeightsStatusActive is the live configuration. At most one row may hold
	// this status; a partial unique index enforces it.
	WeightsStatusActive = "active"
	// WeightsStatusRolledBack is a configuration that was live and was undone.
	WeightsStatusRolledBack = "rolled_back"
	// WeightsStatusSuperseded is a configuration that was live and was replaced
	// by a newer promotion. Rollback restores the most recent of these.
	WeightsStatusSuperseded = "superseded"
)

// ErrWeightsRecordNotFound is returned when a promotion targets a row that does
// not exist.
var ErrWeightsRecordNotFound = errors.New("weights history: record not found")

// WeightsRecord is one row of search_weights_history.
//
// The metric fields are pointers because "not measured" and "measured as zero"
// are different facts: a proposed candidate that never reached the holdout
// evaluation has no holdout score, and recording 0.0 would make it look like a
// catastrophically bad configuration in any later review.
//
// No query text and no document IDs appear here by design — see migration 026.
type WeightsRecord struct {
	ID             int64
	Weights        model.SearchWeights
	Status         string
	TrainObjective *float64
	HoldoutNDCG10  *float64
	HoldoutFP10    *float64
	BaselineNDCG10 *float64
	BaselineFP10   *float64
	HoldoutQueries int
	// GateReason is written for a human: "holdout distinct queries 12 < 30".
	GateReason *string
	// Metadata carries run context such as
	// {"rerank": false, "train_holdout_gap": 0.04, "sweeps": 2}.
	// train_holdout_gap is the overfitting tell-tale.
	Metadata   map[string]any
	CreatedAt  time.Time
	PromotedAt *time.Time
}

// WeightsHistoryStore persists search weight candidates and their promotion
// state.
type WeightsHistoryStore struct {
	pg *Postgres
}

// NewWeightsHistoryStore returns a WeightsHistoryStore backed by Postgres.
func NewWeightsHistoryStore(pg *Postgres) *WeightsHistoryStore {
	return &WeightsHistoryStore{pg: pg}
}

func validWeightsStatus(s string) bool {
	switch s {
	case WeightsStatusProposed, WeightsStatusActive, WeightsStatusRolledBack, WeightsStatusSuperseded:
		return true
	default:
		return false
	}
}

// Insert records a candidate and returns its ID.
//
// Insert never writes status 'active' on its own: becoming live is what
// Promote does, in a transaction that first stands the previous holder down.
// Allowing a direct active insert here would give a caller a way to create the
// second active row that the whole design exists to prevent.
func (s *WeightsHistoryStore) Insert(ctx context.Context, rec WeightsRecord) (int64, error) {
	if !validWeightsStatus(rec.Status) {
		return 0, fmt.Errorf("weights history insert: invalid status %q", rec.Status)
	}
	if rec.Status == WeightsStatusActive {
		return 0, fmt.Errorf("weights history insert: use Promote to make a record active")
	}

	weightsJSON, err := json.Marshal(rec.Weights)
	if err != nil {
		return 0, fmt.Errorf("weights history insert marshal weights: %w", err)
	}
	meta := rec.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return 0, fmt.Errorf("weights history insert marshal metadata: %w", err)
	}

	var id int64
	err = s.pg.pool.QueryRow(ctx, `
		INSERT INTO search_weights_history
			(weights, status, train_objective, holdout_ndcg10, holdout_fp10,
			 baseline_ndcg10, baseline_fp10, holdout_queries, gate_reason, metadata)
		VALUES ($1::jsonb, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb)
		RETURNING id`,
		string(weightsJSON), rec.Status, rec.TrainObjective, rec.HoldoutNDCG10, rec.HoldoutFP10,
		rec.BaselineNDCG10, rec.BaselineFP10, rec.HoldoutQueries, rec.GateReason, string(metaJSON),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("weights history insert: %w", err)
	}
	return id, nil
}

const weightsSelectColumns = `
	id, weights, status, train_objective, holdout_ndcg10, holdout_fp10,
	baseline_ndcg10, baseline_fp10, holdout_queries, gate_reason, metadata,
	created_at, promoted_at`

// Active returns the live configuration, or (nil, nil) when nothing has been
// promoted. A nil result is not an error: it is the normal state before the
// first promotion and after a rollback with nothing to restore, and it means
// "use the compiled-in defaults".
func (s *WeightsHistoryStore) Active(ctx context.Context) (*WeightsRecord, error) {
	row := s.pg.pool.QueryRow(ctx,
		`SELECT`+weightsSelectColumns+`
		 FROM search_weights_history WHERE status = '`+WeightsStatusActive+`'`)
	rec, err := scanWeightsRecord(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("weights history active: %w", err)
	}
	return &rec, nil
}

// Promote makes one record live in a single transaction.
//
// The order matters and is not interchangeable: the incumbent is stood down to
// 'superseded' first, then the target is raised to 'active'. Doing it the other
// way round puts two active rows in the table for the duration of a statement,
// and the partial unique index rejects that — the transaction would fail even
// though the end state is legal.
func (s *WeightsHistoryStore) Promote(ctx context.Context, id int64) error {
	tx, err := s.pg.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("weights history promote begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		UPDATE search_weights_history
		SET status = '`+WeightsStatusSuperseded+`'
		WHERE status = '`+WeightsStatusActive+`' AND id <> $1`, id); err != nil {
		return fmt.Errorf("weights history promote supersede: %w", err)
	}

	tag, err := tx.Exec(ctx, `
		UPDATE search_weights_history
		SET status = '`+WeightsStatusActive+`', promoted_at = now()
		WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("weights history promote activate: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("weights history promote %d: %w", id, ErrWeightsRecordNotFound)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("weights history promote commit: %w", err)
	}
	return nil
}

// Rollback stands the current configuration down and restores the one it
// replaced. It returns the restored record, or (nil, nil) when there was
// nothing to restore — in which case the system falls back to the compiled-in
// defaults, which is itself a safe state.
//
// Runs in one transaction for the same reason as Promote: between the two
// statements the table would otherwise be readable with zero or two active
// rows.
func (s *WeightsHistoryStore) Rollback(ctx context.Context) (*WeightsRecord, error) {
	tx, err := s.pg.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("weights history rollback begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var currentID int64
	err = tx.QueryRow(ctx,
		`SELECT id FROM search_weights_history WHERE status = '`+WeightsStatusActive+`' FOR UPDATE`).Scan(&currentID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Nothing is live; there is nothing to undo.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("weights history rollback read active: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE search_weights_history SET status = '`+WeightsStatusRolledBack+`'
		WHERE id = $1`, currentID); err != nil {
		return nil, fmt.Errorf("weights history rollback stand down: %w", err)
	}

	// The most recently promoted superseded row is the one this configuration
	// replaced. promoted_at orders them; id breaks ties deterministically.
	var previousID int64
	err = tx.QueryRow(ctx, `
		SELECT id FROM search_weights_history
		WHERE status = '`+WeightsStatusSuperseded+`'
		ORDER BY promoted_at DESC NULLS LAST, id DESC
		LIMIT 1`).Scan(&previousID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("weights history rollback commit: %w", err)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("weights history rollback read previous: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE search_weights_history SET status = '`+WeightsStatusActive+`', promoted_at = now()
		WHERE id = $1`, previousID); err != nil {
		return nil, fmt.Errorf("weights history rollback restore: %w", err)
	}

	row := tx.QueryRow(ctx, `SELECT`+weightsSelectColumns+` FROM search_weights_history WHERE id = $1`, previousID)
	rec, err := scanWeightsRecord(row)
	if err != nil {
		return nil, fmt.Errorf("weights history rollback read restored: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("weights history rollback commit: %w", err)
	}
	return &rec, nil
}

func scanWeightsRecord(row pgx.Row) (WeightsRecord, error) {
	var (
		rec         WeightsRecord
		weightsJSON []byte
		metaJSON    []byte
	)
	if err := row.Scan(
		&rec.ID, &weightsJSON, &rec.Status, &rec.TrainObjective,
		&rec.HoldoutNDCG10, &rec.HoldoutFP10, &rec.BaselineNDCG10, &rec.BaselineFP10,
		&rec.HoldoutQueries, &rec.GateReason, &metaJSON, &rec.CreatedAt, &rec.PromotedAt,
	); err != nil {
		return WeightsRecord{}, err
	}
	if err := json.Unmarshal(weightsJSON, &rec.Weights); err != nil {
		return WeightsRecord{}, fmt.Errorf("unmarshal weights: %w", err)
	}
	if err := json.Unmarshal(metaJSON, &rec.Metadata); err != nil {
		rec.Metadata = map[string]any{}
	}
	return rec, nil
}
