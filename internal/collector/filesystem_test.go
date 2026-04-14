package collector

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestIsFilenameTooLong verifies the byte-length guard across ASCII, Korean,
// and boundary values.
func TestIsFilenameTooLong(t *testing.T) {
	t.Parallel()

	// Korean characters are 3 bytes each in UTF-8.
	korean3  := "가나다"                // 9 bytes  — short
	korean85 := strings.Repeat("가", 85) // 255 bytes — exactly at limit
	korean86 := strings.Repeat("가", 86) // 258 bytes — one char over limit

	cases := []struct {
		name     string
		filename string
		want     bool
	}{
		{"empty", "", false},
		{"short ASCII", "hello.md", false},
		{"ASCII exactly 255", strings.Repeat("a", 255), false},
		{"ASCII exactly 256", strings.Repeat("a", 256), true},
		{"Korean 3 chars (9 bytes)", korean3, false},
		{"Korean 85 chars (255 bytes, at limit)", korean85, false},
		{"Korean 86 chars (258 bytes, over limit)", korean86, true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isFilenameTooLong(tc.filename)
			if got != tc.want {
				t.Errorf("isFilenameTooLong(%q) = %v, want %v (bytes: %d)",
					tc.filename, got, tc.want, len(tc.filename))
			}
		})
	}
}

// TestFilesystemCollect_SkipsLongFilenames verifies that Collect skips files
// whose names exceed maxFilenameBytes without aborting the walk, and still
// returns documents for valid files.
//
// Creating a filename longer than the OS limit (255 bytes on macOS/Linux) is
// not possible via the real filesystem, so we verify the guard logic using a
// controlled directory where the long-named file cannot be created. Instead we
// confirm:
//   1. isFilenameTooLong correctly classifies the boundary values (covered by
//      TestIsFilenameTooLong above).
//   2. Collect completes without error and returns documents for normal files
//      even when a walk error occurs for a valid entry (simulated via a
//      permission-denied directory on non-root Unix processes).
//
// For the "too-long name" skip path specifically, we exercise it via a real
// filesystem on macOS/Linux by relying on the fact that the OS itself returns
// an error when we try to stat a component > 255 bytes — the walkErr branch in
// WalkDir fires before our guard. The guard matters primarily when Go's
// directory iterator returns a DirEntry whose Name() is already > 255 bytes
// (possible on some virtual/network filesystems). We test that branch via a
// direct unit test of isFilenameTooLong and confirm the early-return path is
// reachable by code inspection.
func TestFilesystemCollect_SkipsLongFilenames(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	// Create a normal file that should be collected.
	normalFile := filepath.Join(root, "note.md")
	if err := os.WriteFile(normalFile, []byte("# hello"), 0o644); err != nil {
		t.Fatalf("write normal file: %v", err)
	}

	// Attempt to create a file whose name exceeds 255 bytes. On most OS/FS
	// combinations this will fail — that's expected. We use it only when the
	// OS allows it (e.g. some virtual FS layers). The test is meaningful
	// either way:
	//   - If creation fails: we verify collect still works on the normal file.
	//   - If creation succeeds: we verify collect skips the long-named file.
	longName := strings.Repeat("가", 90) + ".md" // 272 bytes — over limit
	longFile := filepath.Join(root, longName)
	longFileCreated := false
	if err := os.WriteFile(longFile, []byte("should be skipped"), 0o644); err == nil {
		longFileCreated = true
		t.Logf("long filename created on this OS (%d bytes) — testing skip path", len(longName))
	} else {
		t.Logf("OS rejected long filename (%d bytes): %v — testing walk error path", len(longName), err)
	}

	c := NewFilesystemCollector(root)
	docs, err := c.Collect(t.Context(), time.Time{})
	if err != nil {
		t.Fatalf("Collect returned error: %v", err)
	}

	// Find collected source IDs.
	sourceIDs := make(map[string]bool, len(docs))
	for _, d := range docs {
		sourceIDs[d.SourceID] = true
	}

	// The normal file must always be collected.
	if !sourceIDs["note.md"] {
		t.Errorf("expected note.md in collected docs, got source IDs: %v", sourceIDs)
	}

	// If the long file was created, it must NOT appear in the results.
	if longFileCreated && sourceIDs[longName] {
		t.Errorf("long-named file %q should have been skipped but was collected", longName)
	}
}

// TestFilesystemListActiveSourceIDs_SkipsLongFilenames mirrors the Collect
// test for the ListActiveSourceIDs path.
func TestFilesystemListActiveSourceIDs_SkipsLongFilenames(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	normalFile := filepath.Join(root, "doc.md")
	if err := os.WriteFile(normalFile, []byte("# doc"), 0o644); err != nil {
		t.Fatalf("write normal file: %v", err)
	}

	longName := strings.Repeat("가", 90) + ".md"
	longFile := filepath.Join(root, longName)
	longFileCreated := false
	if err := os.WriteFile(longFile, []byte("skip me"), 0o644); err == nil {
		longFileCreated = true
	}

	c := NewFilesystemCollector(root)
	ids, err := c.ListActiveSourceIDs(t.Context())
	if err != nil {
		t.Fatalf("ListActiveSourceIDs returned error: %v", err)
	}

	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}

	if !idSet["doc.md"] {
		t.Errorf("expected doc.md in active IDs, got: %v", ids)
	}

	if longFileCreated && idSet[longName] {
		t.Errorf("long-named file %q should have been skipped but appeared in active IDs", longName)
	}
}

// TestFilesystemCollect_WalkErrorContinues verifies that a walk error on a
// single entry (e.g. permission denied sub-directory) does not abort the
// overall walk — other files are still collected.
func TestFilesystemCollect_WalkErrorContinues(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod permission test not applicable on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("running as root — permission bits are not enforced")
	}

	t.Parallel()

	root := t.TempDir()

	// Normal file in root.
	if err := os.WriteFile(filepath.Join(root, "visible.md"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("write visible file: %v", err)
	}

	// Sub-directory with no read/exec permission.
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatalf("mkdir locked: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	c := NewFilesystemCollector(root)
	docs, err := c.Collect(t.Context(), time.Time{})
	if err != nil {
		t.Fatalf("Collect must not return error on walk errors: %v", err)
	}

	found := false
	for _, d := range docs {
		if d.SourceID == "visible.md" {
			found = true
		}
	}
	if !found {
		t.Error("expected visible.md to be collected even when a sibling directory is unreadable")
	}
}
