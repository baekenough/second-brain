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
	ID              int64          `json:"id"`
	Query           string         `json:"query"`
	RelevantDocIDs  []string       `json:"relevant_doc_ids"`
	IrrelevantDocIDs []string      `json:"irrelevant_doc_ids,omitempty"` // thumbs=-1 docs for this query
	Source          string         `json:"source"` // "feedback", "manual"
	CreatedAt       time.Time      `json:"created_at"`
	Metadata        map[string]any `json:"metadata,omitempty"`
}

// EvalStore derives evaluation pairs from feedback data.
type EvalStore struct {
	pg *Postgres
}

// NewEvalStore returns an EvalStore backed by the given Postgres instance.
func NewEvalStore(pg *Postgres) *EvalStore {
	return &EvalStore{pg: pg}
}

// BuildFromFeedback derives eval pairs from positive feedback rows,
// enriched with negative signals (thumbs=-1) and manual source pairs.
//
// Positive pairs: queries where thumbs >= 1 and at least one document_id is
// present. Queries are grouped so that multiple positive votes are merged into
// a single EvalPair with all relevant document IDs collected.
//
// Negative signals: for each query that has positive pairs, documents rated
// thumbs = -1 are added as IrrelevantDocIDs. These are used to compute
// FalsePositivePenalty in the eval pipeline.
//
// Manual source: rows with source = 'manual' are always included regardless
// of thumbs value. This expands the eval pool beyond already-shown documents,
// addressing self-confirming bias.
//
// Results are ordered by the earliest positive feedback creation time (DESC)
// and capped at 5 000 pairs to bound memory usage.
func (s *EvalStore) BuildFromFeedback(ctx context.Context) ([]EvalPair, error) {
	// --- Step 1: Positive pairs from feedback (thumbs >= 1) ---
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

	// queryIndex maps query text → slice index for O(1) lookup when attaching
	// negative signals and manual pairs.
	queryIndex := make(map[string]int)
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
		queryIndex[p.Query] = len(pairs)
		pairs = append(pairs, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("eval: iterate rows: %w", err)
	}

	// --- Step 2: Negative signals (thumbs = -1) ---
	// Attach irrelevant doc IDs to existing pairs; create new pairs for queries
	// that only have negative feedback (so FP penalty can be applied).
	negRows, err := s.pg.Pool().Query(ctx, `
		SELECT query,
		       ARRAY_AGG(DISTINCT document_id) FILTER (WHERE document_id IS NOT NULL) AS neg_doc_ids,
		       MIN(created_at) AS created_at
		FROM feedback
		WHERE thumbs = -1
		  AND query IS NOT NULL
		  AND query != ''
		GROUP BY query
		HAVING COUNT(DISTINCT document_id) FILTER (WHERE document_id IS NOT NULL) > 0
	`)
	if err != nil {
		return nil, fmt.Errorf("eval: build negative feedback: %w", err)
	}
	defer negRows.Close()

	for negRows.Next() {
		var query string
		var negDocIDs []string
		var createdAt time.Time
		if err := negRows.Scan(&query, &negDocIDs, &createdAt); err != nil {
			return nil, fmt.Errorf("eval: scan negative row: %w", err)
		}
		if i, ok := queryIndex[query]; ok {
			// Attach negative signals to existing positive pair.
			pairs[i].IrrelevantDocIDs = negDocIDs
		} else {
			// Query only has negative feedback; include as a negative-only pair
			// so the FP penalty can be computed for it.
			idx++
			pairs = append(pairs, EvalPair{
				ID:               idx,
				Query:            query,
				RelevantDocIDs:   []string{},
				IrrelevantDocIDs: negDocIDs,
				Source:           "feedback",
				CreatedAt:        createdAt,
			})
			queryIndex[query] = len(pairs) - 1
		}
	}
	if err := negRows.Err(); err != nil {
		return nil, fmt.Errorf("eval: iterate negative rows: %w", err)
	}

	// --- Step 3: Manual source pairs (expand eval pool beyond shown docs) ---
	// The 'manual' source is reserved for hand-curated judgements (eval.go:17).
	// These expand the eval pool beyond "documents already shown by the search"
	// and break the self-confirming bias caused by relying solely on thumbs>=1.
	manualRows, err := s.pg.Pool().Query(ctx, `
		SELECT query,
		       ARRAY_AGG(DISTINCT document_id) FILTER (WHERE document_id IS NOT NULL) AS doc_ids,
		       MIN(created_at) AS created_at
		FROM feedback
		WHERE source = 'manual'
		  AND query IS NOT NULL
		  AND query != ''
		GROUP BY query
		HAVING COUNT(DISTINCT document_id) FILTER (WHERE document_id IS NOT NULL) > 0
		ORDER BY created_at DESC
		LIMIT 1000
	`)
	if err != nil {
		return nil, fmt.Errorf("eval: build manual pairs: %w", err)
	}
	defer manualRows.Close()

	for manualRows.Next() {
		var query string
		var docIDs []string
		var createdAt time.Time
		if err := manualRows.Scan(&query, &docIDs, &createdAt); err != nil {
			return nil, fmt.Errorf("eval: scan manual row: %w", err)
		}
		if i, ok := queryIndex[query]; ok {
			// Merge manual doc IDs into existing pair (deduplicate).
			existing := make(map[string]struct{}, len(pairs[i].RelevantDocIDs))
			for _, id := range pairs[i].RelevantDocIDs {
				existing[id] = struct{}{}
			}
			for _, id := range docIDs {
				if _, seen := existing[id]; !seen {
					pairs[i].RelevantDocIDs = append(pairs[i].RelevantDocIDs, id)
					existing[id] = struct{}{}
				}
			}
		} else {
			// New query from manual judgements.
			idx++
			pairs = append(pairs, EvalPair{
				ID:             idx,
				Query:          query,
				RelevantDocIDs: docIDs,
				Source:         "manual",
				CreatedAt:      createdAt,
			})
			queryIndex[query] = len(pairs) - 1
		}
	}
	if err := manualRows.Err(); err != nil {
		return nil, fmt.Errorf("eval: iterate manual rows: %w", err)
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
