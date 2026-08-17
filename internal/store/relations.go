package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/baekenough/second-brain/internal/model"
)

// RelationStore provides persistence for entity_relations (migration 021).
type RelationStore struct {
	pg *Postgres
}

// NewRelationStore returns a RelationStore backed by the given Postgres instance.
func NewRelationStore(pg *Postgres) *RelationStore {
	return &RelationStore{pg: pg}
}

// UpsertEntityRelations inserts relations, relying on the UNIQUE(from, to,
// type, evidence_document_id) constraint (migration 021) to silently skip a
// relation already observed from the SAME evidence document — that is not
// new information (spec §5.4). A DIFFERENT evidence document observing the
// same (from, to, type) pair is a separate row by design (spec §5.4: "김대표"
// and "박부장" observed communicated_with across 3 emails → 3 rows).
//
// Individual failures are accumulated so partial success is still
// persisted, mirroring EntityStore.UpsertAndLinkEntities.
func (s *RelationStore) UpsertEntityRelations(ctx context.Context, relations []model.EntityRelation) error {
	if len(relations) == 0 {
		return nil
	}
	var errs []string
	for _, r := range relations {
		_, err := s.pg.pool.Exec(ctx, `
			INSERT INTO entity_relations (from_entity_id, to_entity_id, type, evidence_document_id, confidence, observed_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (from_entity_id, to_entity_id, type, evidence_document_id) DO NOTHING`,
			r.FromEntityID, r.ToEntityID, string(r.Type), r.EvidenceDocumentID, r.Confidence, r.ObservedAt,
		)
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("entity relation upsert errors (%d): %s", len(errs), strings.Join(errs, "; "))
	}
	return nil
}
