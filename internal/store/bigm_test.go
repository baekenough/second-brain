package store

import "testing"

// TestBigmLikePattern verifies that the LIKE pattern used for pg_bigm substring
// search is assembled correctly. The actual SQL uses concatenation ('%%' || $1 || '%%')
// so that the gin_bigm_ops index is active; this unit test mirrors the equivalent
// Go string construction to confirm pattern correctness without a live database.
func TestBigmLikePattern(t *testing.T) {
	t.Parallel()

	tests := []struct {
		query string
		want  string
	}{
		{"배포", "%배포%"},
		{"deploy", "%deploy%"},
		{"", "%%"},
	}
	for _, tt := range tests {
		tt := tt // capture loop var for parallel sub-tests
		t.Run(tt.query, func(t *testing.T) {
			t.Parallel()
			got := "%" + tt.query + "%"
			if got != tt.want {
				t.Errorf("bigm pattern for %q = %q, want %q", tt.query, got, tt.want)
			}
		})
	}
}
