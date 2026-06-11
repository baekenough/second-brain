package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RecentKind identifies the dashboard card kind for recent-item queries.
type RecentKind string

const (
	// RecentKindSMS returns documents with source_type = 'sms'.
	RecentKindSMS RecentKind = "sms"

	// RecentKindCallRecording returns call-log documents that have an
	// audio_file entry in their metadata JSON (i.e. a recording was captured).
	RecentKindCallRecording RecentKind = "call-recording"

	// RecentKindVoiceMemo returns call-log documents whose metadata field
	// recording_type equals 'voice-memo'.
	RecentKindVoiceMemo RecentKind = "voice-memo"
)

// RecentItem is a lightweight projection of a document used by the dashboard
// recent-items API.  Large fields (content, embedding) are intentionally omitted.
type RecentItem struct {
	ID          uuid.UUID  `json:"id"`
	Title       string     `json:"title"`
	OccurredAt  *time.Time `json:"occurred_at"`
	CollectedAt time.Time  `json:"collected_at"`
}

// ListRecentByKind returns the most recent active documents for the given kind,
// ordered by occurred_at DESC NULLS LAST, then collected_at DESC.
// limit is already validated and capped by the caller.
func (s *DocumentStore) ListRecentByKind(ctx context.Context, kind RecentKind, limit int) ([]RecentItem, error) {
	var (
		rows pgx.Rows
		err  error
	)

	// Build the filter predicate in Go to avoid injecting user input into SQL.
	// All three variants operate on the same two columns
	// (source_type, metadata) so the SELECT/ORDER BY is identical.
	//
	// NOTE: the filter conditions below are compile-time string constants, never
	// derived from user input, so there is no SQL-injection risk.
	const selectCols = `
		SELECT id, title, occurred_at, collected_at
		FROM documents
		WHERE status = 'active'`
	const orderBy = `
		ORDER BY occurred_at DESC NULLS LAST, collected_at DESC
		LIMIT $1`

	switch kind {
	case RecentKindSMS:
		const q = selectCols + `
		  AND source_type = 'sms'` + orderBy
		rows, err = s.pg.pool.Query(ctx, q, limit)
		if err != nil {
			return nil, fmt.Errorf("list recent sms: %w", err)
		}

	case RecentKindCallRecording:
		// Calls that have a recording but are NOT a voice-memo:
		//   - metadata ? 'audio_file'          → a recording file was captured
		//   - IS DISTINCT FROM 'voice-memo'    → excludes voice-memo rows (including
		//     those where recording_type is NULL, i.e. older call-log rows)
		// IS DISTINCT FROM treats NULL as not-equal-to-'voice-memo', so legacy
		// call-log rows without a recording_type field are correctly included.
		const q = selectCols + `
		  AND source_type = 'call-log'
		  AND metadata ? 'audio_file'
		  AND (metadata->>'recording_type' IS DISTINCT FROM 'voice-memo')` + orderBy
		rows, err = s.pg.pool.Query(ctx, q, limit)
		if err != nil {
			return nil, fmt.Errorf("list recent call-recording: %w", err)
		}

	case RecentKindVoiceMemo:
		const q = selectCols + `
		  AND source_type = 'call-log'
		  AND metadata->>'recording_type' = 'voice-memo'` + orderBy
		rows, err = s.pg.pool.Query(ctx, q, limit)
		if err != nil {
			return nil, fmt.Errorf("list recent voice-memo: %w", err)
		}

	default:
		return nil, fmt.Errorf("unknown kind %q", kind)
	}

	defer rows.Close()

	var items []RecentItem
	for rows.Next() {
		var item RecentItem
		if err := rows.Scan(&item.ID, &item.Title, &item.OccurredAt, &item.CollectedAt); err != nil {
			return nil, fmt.Errorf("list recent %s scan: %w", kind, err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list recent %s iter: %w", kind, err)
	}

	// Return an empty slice (not nil) so the JSON response encodes as [] not null.
	if items == nil {
		items = []RecentItem{}
	}
	return items, nil
}
