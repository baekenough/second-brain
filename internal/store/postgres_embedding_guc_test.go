package store

import (
	"testing"
)

// TestNeedsEmbeddingDimGUC verifies that the helper correctly identifies
// migration files that require the app.embedding_dim GUC to be set before
// execution. This guards against accidental filename changes that would break
// the per-migration GUC injection path in RunMigrations.
func TestNeedsEmbeddingDimGUC(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		file string
		want bool
	}{
		{
			name: "migration 011 requires GUC",
			file: "011_configurable_embedding_dim.sql",
			want: true,
		},
		{
			name: "migration 015 requires GUC",
			file: "015_chunk_embeddings.sql",
			want: true,
		},
		{
			name: "migration 001 does not require GUC",
			file: "001_init.sql",
			want: false,
		},
		{
			name: "migration 004 does not require GUC",
			file: "004_chunks.sql",
			want: false,
		},
		{
			name: "empty string does not require GUC",
			file: "",
			want: false,
		},
		{
			name: "partial match does not require GUC",
			file: "011_configurable_embedding_dim",
			want: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := needsEmbeddingDimGUC(tc.file)
			if got != tc.want {
				t.Errorf("needsEmbeddingDimGUC(%q) = %v, want %v", tc.file, got, tc.want)
			}
		})
	}
}
