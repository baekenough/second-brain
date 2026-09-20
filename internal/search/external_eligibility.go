package search

import (
	"context"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// External indexes are hints, never authorities for content or eligibility.
// Without an authoritative reader the entire external lane fails closed.
type candidateDocumentReader interface {
	GetByID(context.Context, uuid.UUID) (*model.Document, error)
}

func (s *Service) hydrateExternalCandidates(ctx context.Context, q model.SearchQuery, candidates []*model.SearchResult) []*model.SearchResult {
	reader, ok := s.store.(candidateDocumentReader)
	if !ok {
		return nil
	}
	out := make([]*model.SearchResult, 0, len(candidates))
	seen := make(map[uuid.UUID]bool)
	for _, hit := range candidates {
		if hit == nil || seen[hit.ID] {
			continue
		}
		seen[hit.ID] = true
		doc, err := reader.GetByID(ctx, hit.ID)
		if err != nil || doc == nil || doc.ID != hit.ID {
			continue
		}
		if !q.IncludeDeleted && (doc.Status != "active" || doc.DeletedAt != nil) {
			continue
		}
		if q.OccurredFrom != nil && (doc.OccurredAt == nil || doc.OccurredAt.Before(*q.OccurredFrom)) {
			continue
		}
		if q.OccurredTo != nil && (doc.OccurredAt == nil || !doc.OccurredAt.Before(*q.OccurredTo)) {
			continue
		}
		out = append(out, &model.SearchResult{Document: *doc, Score: hit.Score})
	}
	return out
}
