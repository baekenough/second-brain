package collector

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestListActiveSourceIDs_NonexistentRoot verifies that ListActiveSourceIDs
// returns a non-nil error when the configured root directory does not exist.
//
// Before the fix, a missing root caused WalkDir to invoke the callback with a
// root-level walkErr that the callback silently swallowed (return nil), resulting
// in an empty slice and a nil error — indistinguishable from a legitimately
// empty directory. MarkDeleted would then vacuously delete all active docs.
func TestListActiveSourceIDs_NonexistentRoot(t *testing.T) {
	t.Parallel()

	c := NewFilesystemCollector("/nonexistent/path/that/does/not/exist")
	ids, err := c.ListActiveSourceIDs(context.Background())

	if err == nil {
		t.Errorf("expected error when root does not exist, got nil (ids=%v)", ids)
	}
	if len(ids) != 0 {
		t.Errorf("expected empty slice on error, got %d IDs: %v", len(ids), ids)
	}
}

// TestListActiveSourceIDs_UnmountedRoot verifies that ListActiveSourceIDs
// returns an error when the root exists as a path but is not accessible
// (simulated via chmod 000 on the root directory).
func TestListActiveSourceIDs_UnmountedRoot(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root — permission bits are not enforced")
	}
	t.Parallel()

	root := t.TempDir()
	// Remove all permissions so WalkDir cannot enter the root at all.
	if err := os.Chmod(root, 0o000); err != nil {
		t.Fatalf("chmod root: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	c := NewFilesystemCollector(root)
	ids, err := c.ListActiveSourceIDs(context.Background())

	if err == nil {
		t.Errorf("expected error when root is inaccessible, got nil (ids=%v)", ids)
	}
	if len(ids) != 0 {
		t.Errorf("expected empty slice on error, got %d IDs: %v", len(ids), ids)
	}
}

// TestListActiveSourceIDs_EmptyRoot verifies that a root that exists but
// contains no indexable files returns ([], nil) — not an error.
// This is the "legitimately empty source" case that must NOT trigger deletion.
func TestListActiveSourceIDs_EmptyRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	c := NewFilesystemCollector(root)
	ids, err := c.ListActiveSourceIDs(context.Background())

	if err != nil {
		t.Errorf("expected nil error for empty-but-existing root, got: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected empty ID slice for empty root, got: %v", ids)
	}
}

// TestListActiveSourceIDs_NormalRoot verifies that a root with files returns
// those files and nil error.
func TestListActiveSourceIDs_NormalRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.md"), []byte("# hi"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "todo.txt"), []byte("items"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := NewFilesystemCollector(root)
	ids, err := c.ListActiveSourceIDs(context.Background())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := make(map[string]bool, len(ids))
	for _, id := range ids {
		found[id] = true
	}
	if !found["note.md"] {
		t.Errorf("expected note.md in IDs, got: %v", ids)
	}
	if !found["todo.txt"] {
		t.Errorf("expected todo.txt in IDs, got: %v", ids)
	}
}
