package worker

import (
	"testing"

	"github.com/baekenough/second-brain/internal/model"
)

// TestParseExtractionResponse exercises the JSON-parsing and validation logic
// of parseExtractionResponse without invoking a live LLM.
func TestParseExtractionResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		raw       string
		wantLen   int
		wantTypes []model.EntityType
		wantNames []string
		wantErr   bool
	}{
		{
			name: "valid response with all types",
			raw: `{"entities": [
				{"name": "Alice", "type": "PERSON"},
				{"name": "Acme Corp", "type": "ORG"},
				{"name": "Machine Learning", "type": "CONCEPT"},
				{"name": "RFC 9999", "type": "OTHER"}
			]}`,
			wantLen:   4,
			wantTypes: []model.EntityType{model.EntityTypePerson, model.EntityTypeOrg, model.EntityTypeConcept, model.EntityTypeOther},
			wantNames: []string{"Alice", "Acme Corp", "Machine Learning", "RFC 9999"},
		},
		{
			name: "unknown type falls back to OTHER",
			raw: `{"entities": [{"name": "Bob", "type": "UNKNOWN_TYPE"}]}`,
			wantLen:   1,
			wantTypes: []model.EntityType{model.EntityTypeOther},
			wantNames: []string{"Bob"},
		},
		{
			name: "empty entities array",
			raw:  `{"entities": []}`,
			wantLen: 0,
		},
		{
			name: "entities with empty names are skipped",
			raw: `{"entities": [
				{"name": "", "type": "PERSON"},
				{"name": "  ", "type": "ORG"},
				{"name": "Valid Name", "type": "CONCEPT"}
			]}`,
			wantLen:   1,
			wantNames: []string{"Valid Name"},
		},
		{
			name:    "invalid JSON returns error",
			raw:     `not json at all`,
			wantErr: true,
		},
		{
			name:    "empty string returns error",
			raw:     ``,
			wantErr: true,
		},
		{
			name: "markdown fenced JSON is stripped and parsed",
			raw: "```json\n{\"entities\": [{\"name\": \"Go\", \"type\": \"CONCEPT\"}]}\n```",
			wantLen:   1,
			wantNames: []string{"Go"},
			wantTypes: []model.EntityType{model.EntityTypeConcept},
		},
		{
			name: "cap enforced at maxEntitiesPerDoc",
			raw: func() string {
				// Build a JSON array with maxEntitiesPerDoc+5 entries.
				s := `{"entities": [`
				for i := 0; i < maxEntitiesPerDoc+5; i++ {
					if i > 0 {
						s += ","
					}
					s += `{"name": "Entity","type":"OTHER"}`
				}
				s += `]}`
				return s
			}(),
			wantLen: maxEntitiesPerDoc,
		},
		{
			name: "type strings are case-insensitive",
			raw: `{"entities": [
				{"name": "Alice", "type": "person"},
				{"name": "Acme", "type": "org"},
				{"name": "AI", "type": "concept"}
			]}`,
			wantLen:   3,
			wantTypes: []model.EntityType{model.EntityTypePerson, model.EntityTypeOrg, model.EntityTypeConcept},
		},
	}

	for _, tc := range tests {
		tc := tc // capture loop variable
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseExtractionResponse(tc.raw)

			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(got) != tc.wantLen {
				t.Errorf("len(entities) = %d, want %d", len(got), tc.wantLen)
			}

			for i, want := range tc.wantTypes {
				if i >= len(got) {
					break
				}
				if got[i].Type != want {
					t.Errorf("entities[%d].Type = %q, want %q", i, got[i].Type, want)
				}
			}

			for i, want := range tc.wantNames {
				if i >= len(got) {
					break
				}
				if got[i].Name != want {
					t.Errorf("entities[%d].Name = %q, want %q", i, got[i].Name, want)
				}
			}
		})
	}
}

// TestCanonicalEntityType verifies that all entity type strings are mapped
// correctly, including edge cases.
func TestCanonicalEntityType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  model.EntityType
	}{
		{"PERSON", model.EntityTypePerson},
		{"person", model.EntityTypePerson},
		{"Person", model.EntityTypePerson},
		{"ORG", model.EntityTypeOrg},
		{"org", model.EntityTypeOrg},
		{"CONCEPT", model.EntityTypeConcept},
		{"concept", model.EntityTypeConcept},
		{"OTHER", model.EntityTypeOther},
		{"other", model.EntityTypeOther},
		{"UNKNOWN", model.EntityTypeOther},
		{"", model.EntityTypeOther},
		{"  PERSON  ", model.EntityTypePerson},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			got := canonicalEntityType(tc.input)
			if got != tc.want {
				t.Errorf("canonicalEntityType(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
