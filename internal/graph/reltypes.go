package graph

import (
	"sort"

	"github.com/baekenough/second-brain/internal/model"
)

// This file exists for exactly one reason: Cypher cannot parameterise
// relationship types or node labels. `-[r:$type]->` is a syntax error, so the
// type has to be a literal inside the query string. If that literal were a
// value read from the database, that single spot would be the only Cypher
// injection path in the whole system.
//
// The maps below close it. Nothing outside them is ever concatenated into a
// query: callers must go through CypherRelType / CypherEntityLabel and skip
// rows whose lookup fails.

// MentionedInRelType is the projection of document_entities. It is not part of
// the 8-item closed vocabulary (which describes entity-to-entity relations),
// so it lives as its own constant rather than inside the map.
const MentionedInRelType = "MENTIONED_IN"

// cypherRelTypes maps the lowercase wire vocabulary stored in
// entity_relations.type to the uppercase-snake Cypher literal.
var cypherRelTypes = map[model.RelationType]string{
	model.RelationCommunicatedWith: "COMMUNICATED_WITH",
	model.RelationRequestedOf:      "REQUESTED_OF",
	model.RelationCommittedTo:      "COMMITTED_TO",
	model.RelationMentions:         "MENTIONS",
	model.RelationBelongsTo:        "BELONGS_TO",
	model.RelationScheduledWith:    "SCHEDULED_WITH",
	model.RelationAboutTopic:       "ABOUT_TOPIC",
	model.RelationRelatedTo:        "RELATED_TO",
}

// cypherEntityLabels maps entities.type to the secondary node label.
// CONCEPT becomes Topic — the graph model's naming, kept in one place so the
// two vocabularies cannot drift silently.
var cypherEntityLabels = map[string]string{
	string(model.EntityTypePerson):  "Person",
	string(model.EntityTypeOrg):     "Org",
	string(model.EntityTypeConcept): "Topic",
	string(model.EntityTypeOther):   "Other",
}

// CypherRelType returns the Cypher literal for t, and false when t is not one
// of the 8 closed-vocabulary values. A false return means "do not project this
// row" — never "fall back to something".
func CypherRelType(t model.RelationType) (string, bool) {
	v, ok := cypherRelTypes[t]
	return v, ok
}

// CypherEntityLabel returns the secondary node label for an entities.type
// value, and false for anything outside the 4 known types.
func CypherEntityLabel(entityType string) (string, bool) {
	v, ok := cypherEntityLabels[entityType]
	return v, ok
}

// AllCypherRelTypes returns the 8 entity-to-entity literals, sorted, for use
// as the allow-set when validating caller-supplied filters.
func AllCypherRelTypes() []string {
	out := make([]string, 0, len(cypherRelTypes))
	for _, v := range cypherRelTypes {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
