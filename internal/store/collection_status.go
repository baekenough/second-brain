package store

import (
	"context"
	"fmt"
	"time"

	"github.com/baekenough/second-brain/internal/model"
)

// SourceDocumentFreshness holds the per-source-type document freshness snapshot
// derived from the documents table (not the collection_log). This is used by the
// FreshnessChecker to detect silent push-ingest failures where no collection_log
// row is written (e.g. SMS documents from the Android push app via /ingest).
type SourceDocumentFreshness struct {
	SourceType  model.SourceType `json:"source_type"`
	LastCreated *time.Time       `json:"last_created"` // MAX(created_at); nil when no active docs exist
	ActiveCount int              `json:"active_count"`
}

// DocumentFreshnessProvider is the interface implemented by DocumentStore to
// expose per-source document freshness data. Defined separately from
// CollectionStatusReader so the FreshnessChecker can be tested with a minimal mock.
type DocumentFreshnessProvider interface {
	DocumentFreshness(ctx context.Context) ([]SourceDocumentFreshness, error)
}

// CollectionSourceStatus describes the freshness state of a single collector
// source computed from the collection_log table.
type CollectionSourceStatus struct {
	SourceType          model.SourceType `json:"source_type"`
	LastSuccessAt       *time.Time       `json:"last_success_at"`      // nil if never succeeded
	LastAttemptAt       *time.Time       `json:"last_attempt_at"`      // nil if never attempted
	ConsecutiveFailures int              `json:"consecutive_failures"` // failures since last success
	TotalRuns           int              `json:"total_runs"`
	StaleSeconds        *float64         `json:"stale_seconds,omitempty"` // seconds since last success; nil when never succeeded
}

// CollectionStatusReader is the interface implemented by DocumentStore to
// support the /api/v1/collect/status endpoint (#137). Defined as a separate
// interface so the API handler can be tested independently.
type CollectionStatusReader interface {
	// CollectionStatus returns the current freshness state per source type.
	// It reads from the collection_log table and computes last-success time
	// and consecutive failure count per source.
	CollectionStatus(ctx context.Context) ([]CollectionSourceStatus, error)
}

// CollectionStatus reads collection_log and computes per-source freshness
// metrics: last successful run time, last attempt time, and consecutive failures
// (runs with a non-NULL error since the last success).
//
// This is used by GET /api/v1/collect/status and the freshness checker (#137).
func (s *DocumentStore) CollectionStatus(ctx context.Context) ([]CollectionSourceStatus, error) {
	// One CTE per source_type:
	//   - total runs
	//   - most recent success (error IS NULL)
	//   - most recent attempt (any)
	//   - consecutive failures = count of consecutive error rows at the tail,
	//     computed via a window function that assigns a group to each run based on
	//     whether it crosses a success boundary.
	//
	// We use a simpler approach that is correct and index-friendly:
	// 1. Get last_success_at and last_attempt_at via MAX with FILTER.
	// 2. Count rows after the last success as consecutive_failures.
	rows, err := s.pg.pool.Query(ctx, `
		WITH per_source AS (
			SELECT
				source_type,
				MAX(finished_at) FILTER (WHERE error IS NULL)  AS last_success_at,
				MAX(finished_at)                               AS last_attempt_at,
				COUNT(*)                                       AS total_runs
			FROM collection_log
			GROUP BY source_type
		),
		consecutive_fail_counts AS (
			SELECT
				cl.source_type,
				COUNT(*) AS consecutive_failures
			FROM collection_log cl
			JOIN per_source ps ON ps.source_type = cl.source_type
			WHERE cl.error IS NOT NULL
			  AND (ps.last_success_at IS NULL OR cl.started_at > ps.last_success_at)
			GROUP BY cl.source_type
		)
		SELECT
			ps.source_type,
			ps.last_success_at,
			ps.last_attempt_at,
			ps.total_runs,
			COALESCE(cfc.consecutive_failures, 0) AS consecutive_failures
		FROM per_source ps
		LEFT JOIN consecutive_fail_counts cfc ON cfc.source_type = ps.source_type
		ORDER BY ps.source_type
	`)
	if err != nil {
		return nil, fmt.Errorf("query collection_status: %w", err)
	}
	defer rows.Close()

	var statuses []CollectionSourceStatus
	now := time.Now().UTC()

	for rows.Next() {
		var st CollectionSourceStatus
		var lastSuccess, lastAttempt *time.Time

		if err := rows.Scan(
			&st.SourceType,
			&lastSuccess,
			&lastAttempt,
			&st.TotalRuns,
			&st.ConsecutiveFailures,
		); err != nil {
			return nil, fmt.Errorf("scan collection_status row: %w", err)
		}

		st.LastSuccessAt = lastSuccess
		st.LastAttemptAt = lastAttempt

		if lastSuccess != nil {
			stale := now.Sub(*lastSuccess).Seconds()
			st.StaleSeconds = &stale
		}

		statuses = append(statuses, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate collection_status rows: %w", err)
	}

	return statuses, nil
}

// DocumentFreshness returns per-source-type MAX(created_at) and active document
// count from the documents table. Only active (non-deleted) documents are
// considered, using the same status='active' filter as CountActiveDocuments.
//
// Results are ordered by source_type ASC. Sources with zero active documents
// are not included (they have no rows to aggregate).
//
// This is used by FreshnessChecker to detect silent push-ingest failures that
// do not write collection_log rows (e.g. SMS via Android push app / /ingest).
func (s *DocumentStore) DocumentFreshness(ctx context.Context) ([]SourceDocumentFreshness, error) {
	rows, err := s.pg.pool.Query(ctx, `
		SELECT
			source_type,
			MAX(created_at) AS last_created,
			COUNT(*)        AS active_count
		FROM documents
		WHERE status = 'active'
		GROUP BY source_type
		ORDER BY source_type
	`)
	if err != nil {
		return nil, fmt.Errorf("query document_freshness: %w", err)
	}
	defer rows.Close()

	var result []SourceDocumentFreshness
	for rows.Next() {
		var f SourceDocumentFreshness
		if err := rows.Scan(&f.SourceType, &f.LastCreated, &f.ActiveCount); err != nil {
			return nil, fmt.Errorf("scan document_freshness row: %w", err)
		}
		result = append(result, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate document_freshness rows: %w", err)
	}
	return result, nil
}
