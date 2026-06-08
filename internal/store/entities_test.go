package store

import "testing"

// TestNormalizeEntityName verifies that entity name normalization behaves as
// expected for deduplication.
func TestNormalizeEntityName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"Alice", "alice"},
		{"  Alice  ", "alice"},
		{"OPENAI", "openai"},
		{"OpenAI", "openai"},
		{"Go Language", "go language"},
		{"", ""},
		{"  ", ""},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			got := normalizeEntityName(tc.input)
			if got != tc.want {
				t.Errorf("normalizeEntityName(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
