package graph

import (
	"testing"

	"github.com/baekenough/second-brain/internal/model"
)

func TestCypherRelType(t *testing.T) {
	cases := map[model.RelationType]string{
		model.RelationCommunicatedWith: "COMMUNICATED_WITH",
		model.RelationRequestedOf:      "REQUESTED_OF",
		model.RelationCommittedTo:      "COMMITTED_TO",
		model.RelationMentions:         "MENTIONS",
		model.RelationBelongsTo:        "BELONGS_TO",
		model.RelationScheduledWith:    "SCHEDULED_WITH",
		model.RelationAboutTopic:       "ABOUT_TOPIC",
		model.RelationRelatedTo:        "RELATED_TO",
	}
	for in, want := range cases {
		if got, ok := CypherRelType(in); !ok || got != want {
			t.Errorf("CypherRelType(%q) = %q,%v; want %q,true", in, got, ok, want)
		}
	}
}

// TestCypherRelType_RejectsUnknown is the injection guard. The uppercase
// "RELATED_TO" case matters: input always comes from the lowercase vocabulary
// stored in Postgres, so an already-uppercased value means it travelled
// through some other conversion path — which must not be trusted.
func TestCypherRelType_RejectsUnknown(t *testing.T) {
	for _, raw := range []string{"", "drop_all", "RELATED_TO", "MENTIONS`]->() DETACH DELETE n //"} {
		if got, ok := CypherRelType(model.RelationType(raw)); ok {
			t.Errorf("CypherRelType(%q) = %q,true; want ok=false", raw, got)
		}
	}
}

func TestAllCypherRelTypes_HasEightEntries(t *testing.T) {
	if n := len(AllCypherRelTypes()); n != 8 {
		t.Errorf("length = %d, want 8", n)
	}
}

// TestAllCypherRelTypes_IsSorted keeps the slice deterministic — it is used to
// build query parameters and test expectations.
func TestAllCypherRelTypes_IsSorted(t *testing.T) {
	got := AllCypherRelTypes()
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("AllCypherRelTypes not sorted/unique at %d: %v", i, got)
		}
	}
}

// TestCypherEntityLabel pins the second literal that cannot be parameterised:
// node labels. Same whitelist discipline as relationship types.
func TestCypherEntityLabel(t *testing.T) {
	cases := map[string]string{
		string(model.EntityTypePerson):  "Person",
		string(model.EntityTypeOrg):     "Org",
		string(model.EntityTypeConcept): "Topic",
		string(model.EntityTypeOther):   "Other",
	}
	for in, want := range cases {
		if got, ok := CypherEntityLabel(in); !ok || got != want {
			t.Errorf("CypherEntityLabel(%q) = %q,%v; want %q,true", in, got, ok, want)
		}
	}
	for _, raw := range []string{"", "person", "Person", "PERSON`) DETACH DELETE n //"} {
		if got, ok := CypherEntityLabel(raw); ok {
			t.Errorf("CypherEntityLabel(%q) = %q,true; want ok=false", raw, got)
		}
	}
}
