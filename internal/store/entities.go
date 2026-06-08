package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/model"
)

// EntityStore provides persistence for named entities and their document links.
// It is backed by the entities and document_entities tables added in migration 017.
type EntityStore struct {
	pg *Postgres
}

// NewEntityStore returns an EntityStore backed by the given Postgres instance.
func NewEntityStore(pg *Postgres) *EntityStore {
	return &EntityStore{pg: pg}
}

// UpsertEntity inserts a canonical entity or returns the existing one when
// (normalized_name, type) already exists. The normalized_name is derived from
// the entity name: lower-cased and trimmed. Returns the entity ID.
func (s *EntityStore) UpsertEntity(ctx context.Context, name string, entityType model.EntityType) (int64, error) {
	normalized := normalizeEntityName(name)
	if normalized == "" {
		return 0, fmt.Errorf("entity name is empty after normalization")
	}

	var id int64
	err := s.pg.pool.QueryRow(ctx, `
		INSERT INTO entities (name, type, normalized_name)
		VALUES ($1, $2, $3)
		ON CONFLICT (normalized_name, type) DO UPDATE
			SET name = CASE
				WHEN length(EXCLUDED.name) > length(entities.name) THEN EXCLUDED.name
				ELSE entities.name
			END
		RETURNING id`,
		name, string(entityType), normalized,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert entity %q (%s): %w", name, entityType, err)
	}
	return id, nil
}

// LinkDocumentEntity creates a join between a document and an entity.
// It is idempotent: conflicting rows are silently ignored via ON CONFLICT DO NOTHING.
func (s *EntityStore) LinkDocumentEntity(ctx context.Context, documentID uuid.UUID, entityID int64) error {
	_, err := s.pg.pool.Exec(ctx, `
		INSERT INTO document_entities (document_id, entity_id)
		VALUES ($1, $2)
		ON CONFLICT DO NOTHING`,
		documentID, entityID,
	)
	if err != nil {
		return fmt.Errorf("link document %s → entity %d: %w", documentID, entityID, err)
	}
	return nil
}

// UpsertAndLinkEntities upserts each entity and links it to the given document
// in a single call. It is the primary entry-point used by the entity extraction
// worker. Failures for individual entities are accumulated and returned as a
// combined error so that partial success is still persisted.
func (s *EntityStore) UpsertAndLinkEntities(ctx context.Context, documentID uuid.UUID, entities []model.Entity) error {
	var errs []string
	for _, e := range entities {
		id, err := s.UpsertEntity(ctx, e.Name, e.Type)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if linkErr := s.LinkDocumentEntity(ctx, documentID, id); linkErr != nil {
			errs = append(errs, linkErr.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("entity link errors (%d): %s", len(errs), strings.Join(errs, "; "))
	}
	return nil
}

// EntitiesForDocuments fetches the entities linked to the given document IDs.
// The result map is keyed by document UUID; missing entries mean no entities
// were extracted for that document. Returns an empty map (not nil) on success.
func (s *EntityStore) EntitiesForDocuments(ctx context.Context, docIDs []uuid.UUID) (map[uuid.UUID][]model.Entity, error) {
	out := make(map[uuid.UUID][]model.Entity, len(docIDs))
	if len(docIDs) == 0 {
		return out, nil
	}

	rows, err := s.pg.pool.Query(ctx, `
		SELECT de.document_id, e.id, e.name, e.type, e.normalized_name, e.created_at
		FROM document_entities de
		JOIN entities e ON e.id = de.entity_id
		WHERE de.document_id = ANY($1)
		ORDER BY de.document_id, e.type, e.normalized_name`,
		docIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("entities for documents: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			docID uuid.UUID
			ent   model.Entity
		)
		if err := rows.Scan(
			&docID,
			&ent.ID,
			&ent.Name,
			&ent.Type,
			&ent.NormalizedName,
			&ent.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan entity row: %w", err)
		}
		out[docID] = append(out[docID], ent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("entities for documents iter: %w", err)
	}
	return out, nil
}

// normalizeEntityName returns a canonical form of the entity name used for
// deduplication: lower-cased and whitespace-trimmed.
func normalizeEntityName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
