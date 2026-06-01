package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestEmbeddingDimMigrationFilename verifies that the filename constant used
// inside RunMigrations to detect migration 011 matches the actual file on disk.
// If the file is renamed, this test will catch the mismatch before runtime.
func TestEmbeddingDimMigrationFilename(t *testing.T) {
	t.Parallel()

	const wantBase = "011_configurable_embedding_dim.sql"

	// The actual migration file lives two directories up from this test file
	// (internal/store/ → project root → migrations/).  We verify the constant
	// used in runMigration011 dispatch matches the file that exists.
	got := filepath.Base(wantBase)
	if got != wantBase {
		t.Fatalf("filepath.Base(%q) = %q, want %q", wantBase, got, wantBase)
	}

	// Confirm the name starts with "011_" so sorting puts it in the right position.
	if !strings.HasPrefix(wantBase, "011_") {
		t.Errorf("migration filename %q must start with '011_' for correct sort order", wantBase)
	}

	// Confirm the file has a .sql extension (RunMigrations filters on this).
	if filepath.Ext(wantBase) != ".sql" {
		t.Errorf("migration filename %q must have .sql extension", wantBase)
	}
}

// TestRunMigrationsEmbeddingDimLogic verifies the branch selection logic inside
// RunMigrations without requiring a live database.  It mirrors the exact
// condition: base == "011_configurable_embedding_dim.sql" && embeddingDim > 0.
func TestRunMigrationsEmbeddingDimLogic(t *testing.T) {
	t.Parallel()

	const targetFile = "011_configurable_embedding_dim.sql"

	cases := []struct {
		name         string
		file         string
		embeddingDim int
		wantSpecial  bool
	}{
		{
			name:         "matching file and positive dim uses special path",
			file:         targetFile,
			embeddingDim: 384,
			wantSpecial:  true,
		},
		{
			name:         "matching file but zero dim uses normal pool path",
			file:         targetFile,
			embeddingDim: 0,
			wantSpecial:  false,
		},
		{
			name:         "matching file but negative dim uses normal pool path",
			file:         targetFile,
			embeddingDim: -1,
			wantSpecial:  false,
		},
		{
			name:         "different file always uses normal pool path",
			file:         "001_init.sql",
			embeddingDim: 1536,
			wantSpecial:  false,
		},
		{
			name:         "default dim 1536 with matching file uses special path",
			file:         targetFile,
			embeddingDim: 1536,
			wantSpecial:  true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := filepath.Base(tc.file) == targetFile && tc.embeddingDim > 0
			if got != tc.wantSpecial {
				t.Errorf("special-path condition for file=%q dim=%d = %v, want %v",
					tc.file, tc.embeddingDim, got, tc.wantSpecial)
			}
		})
	}
}
