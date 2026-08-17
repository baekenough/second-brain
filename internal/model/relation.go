package model

import (
	"time"

	"github.com/google/uuid"
)

// RelationType is one of the 8 closed-vocabulary relation types extracted
// by Part A (spec §5.3), or the downgrade target for anything a model
// returns outside this set.
type RelationType string

const (
	RelationCommunicatedWith RelationType = "communicated_with"
	RelationRequestedOf      RelationType = "requested_of"
	RelationCommittedTo      RelationType = "committed_to"
	RelationMentions         RelationType = "mentions"
	RelationBelongsTo        RelationType = "belongs_to"
	RelationScheduledWith    RelationType = "scheduled_with"
	RelationAboutTopic       RelationType = "about_topic"
	// RelationRelatedTo is both a normal relation type AND the downgrade
	// target for out-of-vocabulary model output (spec §5.3).
	RelationRelatedTo RelationType = "related_to"
)

var validRelationTypes = map[RelationType]bool{
	RelationCommunicatedWith: true,
	RelationRequestedOf:      true,
	RelationCommittedTo:      true,
	RelationMentions:         true,
	RelationBelongsTo:        true,
	RelationScheduledWith:    true,
	RelationAboutTopic:       true,
	RelationRelatedTo:        true,
}

// DowngradeRelationType returns raw unchanged (as RelationType) when it is
// one of the 8 closed-vocabulary values, and RelationRelatedTo otherwise
// (spec §5.3). Comparison is case-sensitive against the exact wire values —
// the extraction prompt instructs the model to use these exact lowercase
// strings, so a case mismatch is itself a signal of model drift worth
// downgrading rather than silently normalizing.
func DowngradeRelationType(raw string) RelationType {
	rt := RelationType(raw)
	if validRelationTypes[rt] {
		return rt
	}
	return RelationRelatedTo
}

// EntityRelation is a row in the entity_relations table (migration 021).
type EntityRelation struct {
	ID                 int64
	FromEntityID       int64
	ToEntityID         int64
	Type               RelationType
	EvidenceDocumentID uuid.UUID
	Confidence         float64
	ObservedAt         time.Time
}
