package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// EvalPair is a single (query, relevant_document) evaluation pair
// derived from positive feedback or explicit ratings.
type EvalPair struct {
	ID             int64          `json:"id"`
	Query          string         `json:"query"`
	RelevantDocIDs []int64        `json:"relevant_doc_ids"`
	Source         string         `json:"source"`   // "feedback", "manual"
	CreatedAt      time.Time      `json:"created_at"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

// EvalStore derives evaluation pairs from feedback data.
type EvalStore struct {
	pg *Postgres
}

// NewEvalStore returns an EvalStore backed by the given Postgres instance.
func NewEvalStore(pg *Postgres) *EvalStore {
	return &EvalStore{pg: pg}
}

// BuildFromFeedback derives eval pairs from positive feedback rows.
// A pair is created when thumbs >= 1 and at least one document_id is present.
// Queries are grouped so that multiple positive votes for the same query are
// merged into a single EvalPair with all relevant document IDs collected.
//
// Results are ordered by the earliest positive feedback creation time (DESC)
// and capped at 5 000 pairs to bound memory usage.
func (s *EvalStore) BuildFromFeedback(ctx context.Context) ([]EvalPair, error) {
	rows, err := s.pg.Pool().Query(ctx, `
		SELECT query,
		       ARRAY_AGG(DISTINCT document_id) FILTER (WHERE document_id IS NOT NULL) AS doc_ids,
		       MIN(created_at) AS created_at
		FROM feedback
		WHERE thumbs >= 1
		  AND query IS NOT NULL
		  AND query != ''
		GROUP BY query
		HAVING COUNT(DISTINCT document_id) FILTER (WHERE document_id IS NOT NULL) > 0
		ORDER BY created_at DESC
		LIMIT 5000
	`)
	if err != nil {
		return nil, fmt.Errorf("eval: build from feedback: %w", err)
	}
	defer rows.Close()

	var pairs []EvalPair
	idx := int64(0)
	for rows.Next() {
		var p EvalPair
		p.Source = "feedback"
		if err := rows.Scan(&p.Query, &p.RelevantDocIDs, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("eval: scan row: %w", err)
		}
		idx++
		p.ID = idx
		pairs = append(pairs, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("eval: iterate rows: %w", err)
	}
	return pairs, nil
}

// ExportJSONL writes eval pairs as newline-delimited JSON (JSONL) to w.
// It returns the number of pairs written and any write error.
// Each line is a self-contained JSON object that can be streamed directly
// to an HTTP response without buffering the entire result set in memory.
func (s *EvalStore) ExportJSONL(ctx context.Context, w io.Writer) (int, error) {
	pairs, err := s.BuildFromFeedback(ctx)
	if err != nil {
		return 0, err
	}
	enc := json.NewEncoder(w)
	for _, p := range pairs {
		if err := enc.Encode(p); err != nil {
			return 0, fmt.Errorf("eval: encode pair: %w", err)
		}
	}
	return len(pairs), nil
}
