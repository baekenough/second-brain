package model

import "testing"

func TestDowngradeRelationType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want RelationType
	}{
		{"communicated_with valid", "communicated_with", RelationCommunicatedWith},
		{"requested_of valid", "requested_of", RelationRequestedOf},
		{"committed_to valid", "committed_to", RelationCommittedTo},
		{"mentions valid", "mentions", RelationMentions},
		{"belongs_to valid", "belongs_to", RelationBelongsTo},
		{"scheduled_with valid", "scheduled_with", RelationScheduledWith},
		{"about_topic valid", "about_topic", RelationAboutTopic},
		{"related_to valid", "related_to", RelationRelatedTo},
		{"unknown type downgraded", "friends_with", RelationRelatedTo},
		{"empty string downgraded", "", RelationRelatedTo},
		{"case mismatch downgraded", "Communicated_With", RelationRelatedTo},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := DowngradeRelationType(tc.raw); got != tc.want {
				t.Errorf("DowngradeRelationType(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}
