package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// GraphSource is the read-only PostgreSQL side of the Neo4j projection.
// It is a separate type (not a method set bolted onto an existing store) so
// that the projection pipeline owns its own queries and cannot accidentally
// acquire write methods.
type GraphSource struct {
	pg *Postgres
}

// NewGraphSource returns a GraphSource backed by the given Postgres instance.
func NewGraphSource(pg *Postgres) *GraphSource {
	return &GraphSource{pg: pg}
}

// errGraphSourceNoDB is returned instead of panicking on a zero-value
// GraphSource, which is what unit tests construct to exercise validation.
var errGraphSourceNoDB = errors.New("graph source: no database handle")

const listRelationsSQL = `
	SELECT r.id, fe.id, fe.name, fe.type, te.id, te.name, te.type,
	       r.type, r.evidence_document_id, r.confidence, r.observed_at
	  FROM entity_relations r
	  JOIN entities fe ON fe.id = r.from_entity_id
	  JOIN entities te ON te.id = r.to_entity_id
	 WHERE r.id > $1
	 ORDER BY r.id
	 LIMIT $2`

// ListRelationsAfter returns relations with id greater than afterID, oldest
// first, together with both endpoint entities.
//
// entity_relations is append-only (inserted with ON CONFLICT DO NOTHING and
// never updated), which is what makes an id watermark correct here. The one
// gap is that BIGSERIAL ids are handed out before commit, so a transaction
// holding id 100 can commit after one holding 101 — a strictly increasing
// watermark can step over it. That is handled by the worker re-reading an
// overlap window each cycle, which is only safe because the projection is
// idempotent.
func (s *GraphSource) ListRelationsAfter(ctx context.Context, afterID int64, limit int) ([]model.ProjectionRelation, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("graph source: limit must be positive, got %d", limit)
	}
	if s == nil || s.pg == nil {
		return nil, errGraphSourceNoDB
	}

	rows, err := s.pg.pool.Query(ctx, listRelationsSQL, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("graph source: list relations: %w", err)
	}
	defer rows.Close()

	out := make([]model.ProjectionRelation, 0, limit)
	for rows.Next() {
		var (
			rel        model.ProjectionRelation
			evidenceID uuid.UUID
		)
		if err := rows.Scan(
			&rel.ID,
			&rel.FromEntityID, &rel.FromName, &rel.FromType,
			&rel.ToEntityID, &rel.ToName, &rel.ToType,
			&rel.Type, &evidenceID, &rel.Confidence, &rel.ObservedAt,
		); err != nil {
			return nil, fmt.Errorf("graph source: scan relation: %w", err)
		}
		rel.EvidenceDocumentID = evidenceID.String()
		out = append(out, rel)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("graph source: list relations iter: %w", err)
	}
	return out, nil
}

// mentionCursorClause returns the keyset predicate for a non-empty cursor and
// the empty string otherwise.
//
// Two statement variants instead of one with `($1 = '' OR (…) > ($1::uuid, $2))`:
// PostgreSQL is free to evaluate both sides of an OR, and casting '' to uuid
// raises "invalid input syntax for type uuid". The cursor VALUE is still bound
// as a parameter — only the presence of the clause is decided in Go.
func mentionCursorClause(afterDocumentID string) string {
	if afterDocumentID == "" {
		return ""
	}
	return `AND (de.document_id, de.entity_id) > ($1::uuid, $2)`
}

// ListMentionsAfter returns document_entities rows (joined to their document
// and entity) after the given keyset cursor, ordered by (document_id,
// entity_id).
//
// document_entities has no monotonic key — its primary key is composite and
// document ids are random UUIDs — so a keyset cursor can only paginate one
// sweep; it cannot be used as an incremental watermark across cycles, because
// a newly inserted row can land "below" a saved cursor. The worker therefore
// restarts each cycle from an empty cursor and sweeps the whole table. That is
// cheap at the current table size and is flagged in the plan's open risks for
// revisiting past ~100k rows.
func (s *GraphSource) ListMentionsAfter(ctx context.Context, afterDocumentID string, afterEntityID int64, limit int) ([]model.ProjectionMention, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("graph source: limit must be positive, got %d", limit)
	}
	if s == nil || s.pg == nil {
		return nil, errGraphSourceNoDB
	}

	var (
		sql  string
		args []any
	)
	if clause := mentionCursorClause(afterDocumentID); clause != "" {
		sql = `
			SELECT d.id, d.source_type, d.occurred_at, e.id, e.name, e.type
			  FROM document_entities de
			  JOIN documents d ON d.id = de.document_id
			  JOIN entities  e ON e.id = de.entity_id
			 WHERE d.status = 'active'
			   ` + clause + `
			 ORDER BY de.document_id, de.entity_id
			 LIMIT $3`
		args = []any{afterDocumentID, afterEntityID, limit}
	} else {
		sql = `
			SELECT d.id, d.source_type, d.occurred_at, e.id, e.name, e.type
			  FROM document_entities de
			  JOIN documents d ON d.id = de.document_id
			  JOIN entities  e ON e.id = de.entity_id
			 WHERE d.status = 'active'
			 ORDER BY de.document_id, de.entity_id
			 LIMIT $1`
		args = []any{limit}
	}

	rows, err := s.pg.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("graph source: list mentions: %w", err)
	}
	defer rows.Close()

	out := make([]model.ProjectionMention, 0, limit)
	for rows.Next() {
		var (
			m     model.ProjectionMention
			docID uuid.UUID
		)
		if err := rows.Scan(&docID, &m.SourceType, &m.OccurredAt, &m.EntityID, &m.EntityName, &m.EntityType); err != nil {
			return nil, fmt.Errorf("graph source: scan mention: %w", err)
		}
		m.DocumentID = docID.String()
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("graph source: list mentions iter: %w", err)
	}
	return out, nil
}
