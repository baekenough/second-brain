package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Feedback represents one user feedback row in the feedback table.
type Feedback struct {
	ID         int64
	Query      *string        // nullable — absent for direct doc/chunk rating
	DocumentID *string        // nullable FK → documents.id (UUID)
	ChunkID    *int64         // nullable FK → chunks.id
	Source     string         // "search" | "discord_bot" | "api"
	SessionID  *string        // optional conversation grouping
	UserID     *string        // opaque identifier (Discord user ID in bot context)
	Thumbs     int16          // -1, 0, +1
	Comment    *string        // optional free-text
	Metadata   map[string]any // arbitrary extra context
	CreatedAt  time.Time
}

// FeedbackStats holds aggregate thumbs counts grouped by source.
type FeedbackStats struct {
	BySource map[string]FeedbackSourceStats `json:"by_source"`
}

// FeedbackSourceStats holds positive/negative counts for one source value.
type FeedbackSourceStats struct {
	Positive int64 `json:"positive"`
	Negative int64 `json:"negative"`
}

// FeedbackStore provides feedback persistence operations backed by Postgres.
type FeedbackStore struct {
	pg *Postgres
}

// NewFeedbackStore returns a FeedbackStore backed by the given Postgres instance.
func NewFeedbackStore(pg *Postgres) *FeedbackStore {
	return &FeedbackStore{pg: pg}
}

// Record inserts a new feedback row and returns the generated ID.
func (s *FeedbackStore) Record(ctx context.Context, f Feedback) (int64, error) {
	metaJSON, err := json.Marshal(f.Metadata)
	if err != nil {
		return 0, fmt.Errorf("feedback marshal metadata: %w", err)
	}

	var id int64
	err = s.pg.pool.QueryRow(ctx, `
		INSERT INTO feedback
			(query, document_id, chunk_id, source, session_id, user_id, thumbs, comment, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb)
		RETURNING id`,
		f.Query, f.DocumentID, f.ChunkID, f.Source,
		f.SessionID, f.UserID, f.Thumbs, f.Comment, string(metaJSON),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("feedback record: %w", err)
	}
	return id, nil
}

// Upsert replaces existing feedback for the same (user_id, session_id, source)
// tuple by deleting the prior rows and inserting a fresh one. This supports the
// thumbs-toggle pattern (e.g. Discord reaction add/remove).
//
// When either user_id or session_id is nil the call falls through to Record,
// since there is no meaningful identity to de-duplicate on.
func (s *FeedbackStore) Upsert(ctx context.Context, f Feedback) (int64, error) {
	if f.UserID == nil || f.SessionID == nil {
		return s.Record(ctx, f)
	}

	// Delete previous feedback for this (user, session, source) tuple.
	if _, err := s.pg.pool.Exec(ctx, `
		DELETE FROM feedback
		WHERE user_id = $1 AND session_id = $2 AND source = $3`,
		*f.UserID, *f.SessionID, f.Source,
	); err != nil {
		return 0, fmt.Errorf("feedback upsert delete: %w", err)
	}

	return s.Record(ctx, f)
}

// ListRecent returns the most recent feedback rows ordered by created_at DESC.
// Intended for eval-set construction (issue #18).
func (s *FeedbackStore) ListRecent(ctx context.Context, limit int) ([]Feedback, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := s.pg.pool.Query(ctx, `
		SELECT id, query, document_id, chunk_id, source, session_id,
		       user_id, thumbs, comment, metadata, created_at
		FROM feedback
		ORDER BY created_at DESC
		LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("feedback list recent: %w", err)
	}
	defer rows.Close()

	var results []Feedback
	for rows.Next() {
		f, err := scanFeedback(rows)
		if err != nil {
			return nil, fmt.Errorf("feedback list recent scan: %w", err)
		}
		results = append(results, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("feedback list recent iter: %w", err)
	}
	return results, nil
}

// Stats returns aggregate thumbs counts grouped by source.
// Useful for monitoring dashboard and eval set quality signals.
func (s *FeedbackStore) Stats(ctx context.Context) (FeedbackStats, error) {
	rows, err := s.pg.pool.Query(ctx, `
		SELECT source, thumbs, COUNT(*)::bigint
		FROM feedback
		GROUP BY source, thumbs`)
	if err != nil {
		return FeedbackStats{}, fmt.Errorf("feedback stats: %w", err)
	}
	defer rows.Close()

	bySource := make(map[string]FeedbackSourceStats)
	for rows.Next() {
		var (
			src    string
			thumbs int16
			count  int64
		)
		if err := rows.Scan(&src, &thumbs, &count); err != nil {
			return FeedbackStats{}, fmt.Errorf("feedback stats scan: %w", err)
		}
		entry := bySource[src]
		switch thumbs {
		case 1:
			entry.Positive += count
		case -1:
			entry.Negative += count
		}
		bySource[src] = entry
	}
	if err := rows.Err(); err != nil {
		return FeedbackStats{}, fmt.Errorf("feedback stats iter: %w", err)
	}

	return FeedbackStats{BySource: bySource}, nil
}

// --- scan helpers ---

type feedbackScanner interface {
	Scan(dest ...any) error
}

func scanFeedback(row feedbackScanner) (Feedback, error) {
	var (
		f        Feedback
		metaJSON []byte
	)
	if err := row.Scan(
		&f.ID, &f.Query, &f.DocumentID, &f.ChunkID,
		&f.Source, &f.SessionID, &f.UserID,
		&f.Thumbs, &f.Comment, &metaJSON, &f.CreatedAt,
	); err != nil {
		return Feedback{}, err
	}
	if err := json.Unmarshal(metaJSON, &f.Metadata); err != nil {
		f.Metadata = map[string]any{}
	}
	return f, nil
}
