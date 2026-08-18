package graph

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// projectionStateID is the fixed key of the single bookkeeping node. The
// watermark lives in Neo4j rather than PostgreSQL so that deleting the Neo4j
// volume also deletes the watermark: a watermark that outlived its graph would
// make the next rebuild resume mid-stream and silently skip everything before
// it.
const projectionStateID = "singleton"

// wipeBatchSize bounds one DETACH DELETE transaction. CALL {...} IN
// TRANSACTIONS is not usable inside a managed transaction, so Wipe loops
// instead.
const wipeBatchSize = 10000

// Projector performs the PostgreSQL → Neo4j projection. It is the ONLY writer
// to the graph; that fact is what replaces the relationship uniqueness
// constraint Neo4j Community does not offer.
type Projector struct {
	c *Client
}

// NewProjector returns a Projector writing through c.
func NewProjector(c *Client) *Projector { return &Projector{c: c} }

// EnsureSchema forwards to the client so callers only depend on the projector.
func (p *Projector) EnsureSchema(ctx context.Context) error { return p.c.EnsureSchema(ctx) }

// normalizeEntityName mirrors store.normalizeEntityName (lower + trim), which
// is how entities.normalized_name is produced. It is recomputed here rather
// than carried through the projection row because the derivation is the same
// pure function on the same input; if that ever stops being true, the column
// has to be added to the projection query instead.
func normalizeEntityName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// entityProps builds the property map merged onto an :Entity node.
func entityProps(name, entityType string) map[string]any {
	return map[string]any{
		"name":           name,
		"normalizedName": normalizeEntityName(name),
		"type":           entityType,
	}
}

// UpsertRelations projects a batch of relations. Rows are grouped by
// relationship type because the type is a Cypher literal that cannot be
// parameterised; the literal always comes from CypherRelType, never from the
// row. Rows whose type is outside the whitelist are dropped (counted, not
// named — privacy policy §4).
//
// The whole batch is one transaction: either the batch lands or the watermark
// does not move.
func (p *Projector) UpsertRelations(ctx context.Context, rows []model.ProjectionRelation) error {
	if len(rows) == 0 {
		return nil
	}

	grouped := map[string][]map[string]any{}
	skipped := 0
	for _, r := range rows {
		relType, ok := CypherRelType(r.Type)
		if !ok {
			skipped++
			continue
		}
		grouped[relType] = append(grouped[relType], map[string]any{
			"pgId":         r.ID,
			"fromId":       r.FromEntityID,
			"toId":         r.ToEntityID,
			"from":         entityProps(r.FromName, r.FromType),
			"to":           entityProps(r.ToName, r.ToType),
			"evidencePgId": r.EvidenceDocumentID,
			"confidence":   r.Confidence,
			"observedAt":   r.ObservedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	if skipped > 0 {
		slog.Warn("graph projector: skipped relations with out-of-vocabulary type", "count", skipped)
	}
	if len(grouped) == 0 {
		return nil
	}

	// Deterministic statement order keeps failures reproducible.
	relTypes := make([]string, 0, len(grouped))
	for k := range grouped {
		relTypes = append(relTypes, k)
	}
	sort.Strings(relTypes)

	labelIDs := entityLabelGroups(rows)

	_, err := p.c.Write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		for _, relType := range relTypes {
			// relType is a whitelist constant; everything else is a $parameter.
			cypher := `
				UNWIND $rows AS row
				MERGE (a:Entity {pgId: row.fromId})
				  ON CREATE SET a += row.from
				  ON MATCH  SET a += row.from
				MERGE (b:Entity {pgId: row.toId})
				  ON CREATE SET b += row.to
				  ON MATCH  SET b += row.to
				MERGE (a)-[r:` + relType + ` {pgId: row.pgId}]->(b)
				  ON CREATE SET r.evidencePgId = row.evidencePgId,
				                r.confidence   = row.confidence,
				                r.observedAt   = datetime(row.observedAt)`
			if err := runWrite(ctx, tx, cypher, map[string]any{"rows": grouped[relType]}); err != nil {
				return nil, err
			}
		}
		return nil, applyEntityLabels(ctx, tx, labelIDs)
	})
	if err != nil {
		return fmt.Errorf("graph projector: upsert relations: %w", err)
	}
	return nil
}

// UpsertMentions projects document_entities rows as (:Entity)-[:MENTIONED_IN]->(:Document).
//
// :Document carries pgId, sourceType and occurredAt only. Title and content
// are deliberately absent: a leaked graph volume must expose structure, not
// conversation text (privacy policy §1).
func (p *Projector) UpsertMentions(ctx context.Context, rows []model.ProjectionMention) error {
	if len(rows) == 0 {
		return nil
	}

	payload := make([]map[string]any, 0, len(rows))
	labels := map[string][]int64{}
	seenLabel := map[int64]bool{}
	for _, m := range rows {
		var occurredAt any // nil stays nil — "unknown" must not become epoch.
		if m.OccurredAt != nil {
			occurredAt = m.OccurredAt.UTC().Format(time.RFC3339Nano)
		}
		payload = append(payload, map[string]any{
			"documentId": m.DocumentID,
			"sourceType": m.SourceType,
			"occurredAt": occurredAt,
			"entityId":   m.EntityID,
			"entity":     entityProps(m.EntityName, m.EntityType),
		})
		if label, ok := CypherEntityLabel(m.EntityType); ok && !seenLabel[m.EntityID] {
			seenLabel[m.EntityID] = true
			labels[label] = append(labels[label], m.EntityID)
		}
	}

	const cypher = `
		UNWIND $rows AS row
		MERGE (d:Document {pgId: row.documentId})
		  ON CREATE SET d.sourceType = row.sourceType,
		                d.occurredAt = CASE WHEN row.occurredAt IS NULL THEN NULL ELSE datetime(row.occurredAt) END
		  ON MATCH  SET d.sourceType = row.sourceType,
		                d.occurredAt = CASE WHEN row.occurredAt IS NULL THEN NULL ELSE datetime(row.occurredAt) END
		MERGE (e:Entity {pgId: row.entityId})
		  ON CREATE SET e += row.entity
		MERGE (e)-[m:MENTIONED_IN]->(d)
		  ON CREATE SET m.evidencePgId = row.documentId`

	_, err := p.c.Write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		if err := runWrite(ctx, tx, cypher, map[string]any{"rows": payload}); err != nil {
			return nil, err
		}
		return nil, applyEntityLabels(ctx, tx, labels)
	})
	if err != nil {
		return fmt.Errorf("graph projector: upsert mentions: %w", err)
	}
	return nil
}

// State reads (creating on first call) the projection bookkeeping node.
func (p *Projector) State(ctx context.Context) (model.GraphProjectionState, error) {
	const cypher = `
		MERGE (p:ProjectionState {id: $id})
		  ON CREATE SET p.lastRelationId = 0, p.resetToken = '', p.updatedAt = datetime()
		RETURN coalesce(p.lastRelationId, 0) AS lastRelationId,
		       coalesce(p.resetToken, '')    AS resetToken`

	out, err := p.c.Write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		res, err := tx.Run(ctx, cypher, map[string]any{"id": projectionStateID})
		if err != nil {
			return nil, err
		}
		rec, err := res.Single(ctx)
		if err != nil {
			return nil, err
		}
		st := model.GraphProjectionState{}
		if v, ok := rec.Get("lastRelationId"); ok {
			if n, ok := v.(int64); ok {
				st.LastRelationID = n
			}
		}
		if v, ok := rec.Get("resetToken"); ok {
			if s, ok := v.(string); ok {
				st.ResetToken = s
			}
		}
		return st, nil
	})
	if err != nil {
		return model.GraphProjectionState{}, fmt.Errorf("graph projector: state: %w", err)
	}
	st, _ := out.(model.GraphProjectionState)
	return st, nil
}

// SetLastRelationID advances the watermark. Callers must only call this after
// the corresponding batch has been written successfully.
func (p *Projector) SetLastRelationID(ctx context.Context, id int64) error {
	const cypher = `
		MERGE (p:ProjectionState {id: $id})
		SET p.lastRelationId = $value, p.updatedAt = datetime()`
	if _, err := p.c.Write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return nil, runWrite(ctx, tx, cypher, map[string]any{"id": projectionStateID, "value": id})
	}); err != nil {
		return fmt.Errorf("graph projector: set last relation id: %w", err)
	}
	return nil
}

// SetResetToken records the rebuild token that produced the current graph.
func (p *Projector) SetResetToken(ctx context.Context, token string) error {
	const cypher = `
		MERGE (p:ProjectionState {id: $id})
		SET p.resetToken = $value, p.updatedAt = datetime()`
	if _, err := p.c.Write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return nil, runWrite(ctx, tx, cypher, map[string]any{"id": projectionStateID, "value": token})
	}); err != nil {
		return fmt.Errorf("graph projector: set reset token: %w", err)
	}
	return nil
}

// Wipe deletes the entire graph, including the ProjectionState node — a
// rebuild has to restart from watermark 0, and leaving the watermark behind
// would make the rebuilt graph permanently missing everything below it.
//
// Deletion is chunked because a single DETACH DELETE of the whole graph would
// build one enormous transaction.
func (p *Projector) Wipe(ctx context.Context) error {
	const cypher = `MATCH (n) WITH n LIMIT $limit DETACH DELETE n RETURN count(*) AS deleted`
	for {
		out, err := p.c.Write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
			res, err := tx.Run(ctx, cypher, map[string]any{"limit": wipeBatchSize})
			if err != nil {
				return nil, err
			}
			rec, err := res.Single(ctx)
			if err != nil {
				return nil, err
			}
			v, _ := rec.Get("deleted")
			n, _ := v.(int64)
			return n, nil
		})
		if err != nil {
			return fmt.Errorf("graph projector: wipe: %w", err)
		}
		if n, _ := out.(int64); n == 0 {
			return nil
		}
	}
}

// --- helpers ---

// runWrite runs a write statement and drains it, so a server-side failure on a
// statement that returns no rows is still reported.
func runWrite(ctx context.Context, tx neo4j.ManagedTransaction, cypher string, params map[string]any) error {
	res, err := tx.Run(ctx, cypher, params)
	if err != nil {
		return err
	}
	_, err = res.Consume(ctx)
	return err
}

// entityLabelGroups buckets the endpoint entity ids of a relation batch by
// their secondary label.
func entityLabelGroups(rows []model.ProjectionRelation) map[string][]int64 {
	out := map[string][]int64{}
	seen := map[int64]bool{}
	add := func(id int64, entityType string) {
		if seen[id] {
			return
		}
		label, ok := CypherEntityLabel(entityType)
		if !ok {
			return
		}
		seen[id] = true
		out[label] = append(out[label], id)
	}
	for _, r := range rows {
		add(r.FromEntityID, r.FromType)
		add(r.ToEntityID, r.ToType)
	}
	return out
}

// applyEntityLabels attaches the secondary type label. Labels, like
// relationship types, cannot be parameterised — the literal comes from
// CypherEntityLabel and the ids are bound as $ids.
func applyEntityLabels(ctx context.Context, tx neo4j.ManagedTransaction, groups map[string][]int64) error {
	labels := make([]string, 0, len(groups))
	for label := range groups {
		labels = append(labels, label)
	}
	sort.Strings(labels)

	for _, label := range labels {
		cypher := `UNWIND $ids AS id MATCH (e:Entity {pgId: id}) SET e:` + label
		if err := runWrite(ctx, tx, cypher, map[string]any{"ids": groups[label]}); err != nil {
			return err
		}
	}
	return nil
}
