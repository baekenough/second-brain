package model

import "time"

// Types in this file are the wire shape between the PostgreSQL read queries
// (store.GraphSource) and the Neo4j projector (graph.Projector). They are
// deliberately flat: one row of the projection query is one struct, so the
// projector never has to re-join anything.

// ProjectionRelation is one entity_relations row joined with both endpoint
// entities. Relations and their endpoints are fetched together on purpose —
// projecting entities in a separate pass would create a "relation arrived
// before its node" ordering dependency, and skipping such a relation while
// still advancing the watermark would lose it permanently.
type ProjectionRelation struct {
	ID           int64
	FromEntityID int64
	FromName     string
	FromType     string
	ToEntityID   int64
	ToName       string
	ToType       string
	Type         RelationType
	// EvidenceDocumentID is the UUID in string form: it is used as a Neo4j
	// node key, and Neo4j has no UUID type.
	EvidenceDocumentID string
	Confidence         float64
	ObservedAt         time.Time
}

// ProjectionMention is one document_entities row joined with its document and
// entity. OccurredAt is nullable (documents.occurred_at) and is passed to
// Neo4j as null rather than a zero time, so "unknown" stays distinguishable
// from "epoch".
//
// Note what is absent: title and content. The graph never stores document
// text (privacy policy §1) — a leaked Neo4j volume must expose the shape of
// the graph, not the contents of conversations.
type ProjectionMention struct {
	DocumentID string
	SourceType string
	OccurredAt *time.Time
	EntityID   int64
	EntityName string
	EntityType string
}

// GraphProjectionState is the projector's bookkeeping, stored inside Neo4j
// (not PostgreSQL) so that deleting the Neo4j volume also deletes the
// watermark. A watermark that outlives the graph it describes would make a
// rebuild silently incomplete.
type GraphProjectionState struct {
	LastRelationID int64
	ResetToken     string
}
