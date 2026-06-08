package model

import "time"

// EntityType classifies a named entity extracted from a document.
type EntityType string

const (
	EntityTypePerson  EntityType = "PERSON"
	EntityTypeOrg     EntityType = "ORG"
	EntityTypeConcept EntityType = "CONCEPT"
	EntityTypeOther   EntityType = "OTHER"
)

// Entity is a canonical named entity stored in the entities table.
// Entities are deduplicated by (NormalizedName, Type) so that multiple
// documents referencing the same real-world entity converge to a single row.
type Entity struct {
	ID             int64      `json:"id"`
	Name           string     `json:"name"`
	Type           EntityType `json:"type"`
	NormalizedName string     `json:"normalized_name"`
	CreatedAt      time.Time  `json:"created_at"`
}
