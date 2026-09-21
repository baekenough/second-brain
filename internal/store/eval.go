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
	ID               int64     `json:"id"`
	Query            string    `json:"query"`
	RelevantDocIDs   []string  `json:"relevant_doc_ids"`
	IrrelevantDocIDs []string  `json:"irrelevant_doc_ids,omitempty"` // thumbs=-1 docs for this query
	Source           string    `json:"source"`                       // "feedback", "manual"
	CreatedAt        time.Time `json:"created_at"`

	// GoldenQueryID / GoldenQuerySource 는 골든셋에서 뽑은 쌍에만 채워진다
	// (GoldenStore.ExportEvalPairs). 각각 golden_queries.id 와 그 행의
	// source("seed" | "ask_history" | "manual" | "hermes" | "document")로,
	// 질의 문구를 드러내지 않고도 진단 출력에서 질의를 지목하고 출처별로
	// 나눠 볼 수 있게 한다. 피드백 기반 쌍에서는 빈 문자열이다.
	GoldenQueryID     string `json:"golden_query_id,omitempty"`
	GoldenQuerySource string `json:"golden_query_source,omitempty"`

	Metadata map[string]any `json:"metadata,omitempty"`
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
// Manual rows obey the same positive/negative vote semantics. Conflicting
// historical votes resolve to negative, never both labels.
//
// Results are ordered by the earliest positive feedback creation time (DESC)
// and capped at 5 000 pairs to bound memory usage.
func (s *EvalStore) BuildFromFeedback(ctx context.Context) ([]EvalPair, error) {
	return s.buildFromFeedback(ctx, "")
}

// EvalPairsBySplit returns the eval pairs belonging to one train/holdout split
// (see migrations/025_feedback_evidence.sql and internal/dataset).
//
// It exists so that internal/dataset can read the two halves with two separate
// queries. There is deliberately no "read everything and partition afterwards"
// path: a variable holding both halves is exactly what must not exist outside
// the dataset package, or the weight optimiser can be handed its own validation
// set by accident.
//
// split must be "train" or "holdout"; anything else is rejected rather than
// silently widened to "all", because a typo that returns the whole labelled set
// would be indistinguishable from success right up until the holdout metric
// stops meaning anything.
func (s *EvalStore) EvalPairsBySplit(ctx context.Context, split string) ([]EvalPair, error) {
	if split != "train" && split != "holdout" {
		return nil, fmt.Errorf("eval: invalid split %q (want \"train\" or \"holdout\")", split)
	}
	return s.buildFromFeedback(ctx, split)
}

// buildFromFeedback is the shared implementation. An empty split means "no
// split filter" and is reachable only from BuildFromFeedback, whose behaviour
// is unchanged.
func (s *EvalStore) buildFromFeedback(ctx context.Context, split string) ([]EvalPair, error) {
	// --- Step 1: Positive pairs from feedback (thumbs >= 1) ---
	rows, err := s.pg.Pool().Query(ctx, `
		SELECT query,
		       ARRAY_AGG(DISTINCT document_id) FILTER (WHERE document_id IS NOT NULL) AS doc_ids,
		       MIN(created_at) AS created_at
		FROM feedback
		WHERE thumbs >= 1
		  AND query IS NOT NULL
		  AND query != ''
		  AND ($1 = '' OR split = $1)
		GROUP BY query
		HAVING COUNT(DISTINCT document_id) FILTER (WHERE document_id IS NOT NULL) > 0
		ORDER BY created_at DESC
		LIMIT 5000
	`, split)
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
		  AND ($1 = '' OR split = $1)
		GROUP BY query
		HAVING COUNT(DISTINCT document_id) FILTER (WHERE document_id IS NOT NULL) > 0
	`, split)
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

	// Manual votes already participate in the signed feedback queries above.
	// Never reinterpret manual negative votes as positive relevance labels.
	// If historical votes conflict, the explicit negative wins deterministically.
	for i := range pairs {
		negative := make(map[string]bool, len(pairs[i].IrrelevantDocIDs))
		for _, id := range pairs[i].IrrelevantDocIDs {
			negative[id] = true
		}
		positive := pairs[i].RelevantDocIDs[:0]
		for _, id := range pairs[i].RelevantDocIDs {
			if !negative[id] {
				positive = append(positive, id)
			}
		}
		pairs[i].RelevantDocIDs = positive
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
