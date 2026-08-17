package model

import "testing"

// TestSourceNoteAndInsight_StringValues pins the wire-format string values of
// the two new source types added for Capture (spec §3.3). Downstream SQL
// filters (internal/store) and the search API's default insight-exclusion
// guard (internal/api/search.go) hard-code these literals, so a silent rename
// here would break both without a compile error.
func TestSourceNoteAndInsight_StringValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  SourceType
		want string
	}{
		{"SourceNote", SourceNote, "note"},
		{"SourceInsight", SourceInsight, "insight"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if string(tc.got) != tc.want {
				t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
			}
		})
	}
}

// TestSourceNoteAndInsight_DistinctFromLLMMemory verifies the three
// note-shaped source types remain pairwise distinct — a collision would
// silently merge MCP add_note memories, Capture notes, and derived insights
// into one source_type bucket (spec §3.3 requires them kept separate).
func TestSourceNoteAndInsight_DistinctFromLLMMemory(t *testing.T) {
	t.Parallel()

	seen := map[SourceType]string{}
	candidates := map[string]SourceType{
		"SourceLLMMemory": SourceLLMMemory,
		"SourceNote":       SourceNote,
		"SourceInsight":    SourceInsight,
	}
	for name, st := range candidates {
		if prev, ok := seen[st]; ok {
			t.Errorf("%s and %s share the same SourceType value %q", name, prev, st)
		}
		seen[st] = name
	}
}
