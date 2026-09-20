package store

import (
	"context"
	"fmt"
)

// EvalExclusions describes labels outside the default production retrieval
// corpus. Filtering affects this evaluation snapshot only, never stored labels.
type EvalExclusions struct {
	Relevant   int `json:"relevant_labels"`
	Irrelevant int `json:"irrelevant_labels"`
	Queries    int `json:"queries"`
}

func (s *EvalStore) FilterSearchEligible(ctx context.Context, pairs []EvalPair) ([]EvalPair, EvalExclusions, error) {
	var excluded EvalExclusions
	ids := make([]string, 0)
	seen := map[string]bool{}
	for _, p := range pairs {
		for _, list := range [][]string{p.RelevantDocIDs, p.IrrelevantDocIDs} {
			for _, id := range list {
				if !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
			}
		}
	}
	if len(ids) == 0 {
		return nil, excluded, nil
	}
	rows, err := s.pg.Pool().Query(ctx, `SELECT id::text FROM documents WHERE id=ANY($1::uuid[])
 AND status='active' AND deleted_at IS NULL AND source_type<>'insight'
 AND COALESCE(metadata->>'retention','')<>'disposable'`, ids)
	if err != nil {
		return nil, excluded, fmt.Errorf("eval label eligibility: %w", err)
	}
	defer rows.Close()
	eligible := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, excluded, err
		}
		eligible[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, excluded, err
	}
	filtered := make([]EvalPair, 0, len(pairs))
	for _, p := range pairs {
		positive, negative := []string{}, []string{}
		for _, id := range p.RelevantDocIDs {
			if eligible[id] {
				positive = append(positive, id)
			} else {
				excluded.Relevant++
			}
		}
		for _, id := range p.IrrelevantDocIDs {
			if eligible[id] {
				negative = append(negative, id)
			} else {
				excluded.Irrelevant++
			}
		}
		if len(positive)+len(negative) == 0 {
			excluded.Queries++
			continue
		}
		p.RelevantDocIDs, p.IrrelevantDocIDs = positive, negative
		filtered = append(filtered, p)
	}
	return filtered, excluded, nil
}
