package askeval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRepoRoot_ResolvesThisRepository asserts repoRoot's compile-time-path
// walk actually lands on THIS repository (a go.mod declaring repoModule,
// with an eval/ask/fixtures directory directly beneath it) — the anchor
// every requireFixturesUnderRepo test below depends on being correct.
func TestRepoRoot_ResolvesThisRepository(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("repoRoot: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("reading go.mod under resolved root %q: %v", root, err)
	}
	if !goModDeclaresModule(b, repoModule) {
		t.Fatalf("go.mod under resolved root %q does not declare module %q", root, repoModule)
	}
	if info, err := os.Stat(filepath.Join(root, "eval", "ask", "fixtures")); err != nil || !info.IsDir() {
		t.Fatalf("resolved root %q has no eval/ask/fixtures directory (err=%v)", root, err)
	}
}

// TestRequireFixturesUnderRepo_AllowsRealFixturesDir is the positive case:
// the repository's own committed fixtures directory must always be
// accepted — every RefusesWithoutGuards test in judge_test.go that expects
// NewRemoteClaimJudge to proceed PAST this specific guard depends on it.
func TestRequireFixturesUnderRepo_AllowsRealFixturesDir(t *testing.T) {
	if err := requireFixturesUnderRepo(fixturesDir(t)); err != nil {
		t.Errorf("want the real eval/ask/fixtures dir to be allowed, got error: %v", err)
	}
}

// TestRequireFixturesUnderRepo_RejectsOutsidePathWithMatchingSuffix is
// deep-verify #273's HIGH finding, attack shape 1: a plain (non-symlink)
// directory OUTSIDE this repository whose trailing path components merely
// happen to spell "eval/ask/fixtures" — the pre-fix guard's bare
// strings.Contains(abs, "/eval/ask/fixtures") check passed this; the
// repo-root-anchored filepath.Rel check must not.
func TestRequireFixturesUnderRepo_RejectsOutsidePathWithMatchingSuffix(t *testing.T) {
	fakeRepo := t.TempDir()
	suffixed := filepath.Join(fakeRepo, "eval", "ask", "fixtures")
	if err := os.MkdirAll(suffixed, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	err := requireFixturesUnderRepo(suffixed)
	if err == nil {
		t.Fatalf("want an error for %q (outside the repo, matching suffix only), got nil", suffixed)
	}
	if !strings.Contains(err.Error(), "outside") {
		t.Errorf("want the error to explain the path is outside the repo's fixtures tree, got: %v", err)
	}
}

// TestRequireFixturesUnderRepo_RejectsSymlinkedDirResolvingOutside is
// deep-verify #273's HIGH finding, attack shape 2: a SYMLINK whose own
// (unresolved) path spells "eval/ask/fixtures" — passing the pre-fix
// guard's substring check exactly like a real fixtures dir would — but
// whose target resolves entirely outside this repository. Only
// filepath.EvalSymlinks before the containment check catches this; the
// pre-fix code never called it.
func TestRequireFixturesUnderRepo_RejectsSymlinkedDirResolvingOutside(t *testing.T) {
	fakeRepo := t.TempDir()
	evalAskDir := filepath.Join(fakeRepo, "eval", "ask")
	if err := os.MkdirAll(evalAskDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	realOutsideData := t.TempDir()
	symlinkPath := filepath.Join(evalAskDir, "fixtures")
	if err := os.Symlink(realOutsideData, symlinkPath); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}
	err := requireFixturesUnderRepo(symlinkPath)
	if err == nil {
		t.Fatalf("want an error for %q (a symlink resolving outside the repo despite its own path spelling eval/ask/fixtures), got nil", symlinkPath)
	}
	if !strings.Contains(err.Error(), "outside") {
		t.Errorf("want the error to explain the resolved target is outside the repo's fixtures tree, got: %v", err)
	}
}

// TestRequireFixturesUnderRepo_RejectsEmptyDir pins the pre-existing empty-
// FixturesDir guard survives this rewrite unchanged (judge_test.go's
// "empty fixtures dir" subtest exercises this indirectly through
// NewRemoteClaimJudge; this test exercises requireFixturesUnderRepo
// itself, directly).
func TestRequireFixturesUnderRepo_RejectsEmptyDir(t *testing.T) {
	if err := requireFixturesUnderRepo(""); err == nil {
		t.Fatal("want an error for an empty fixtures dir, got nil")
	}
}

// TestRejectSymlinkedJSONOutside_RejectsSymlinkedFileResolvingOutside is
// deep-verify #273's HIGH finding, third attack shape: a directory that
// itself resolves cleanly INSIDE the allowed fixtures root (so the
// directory-level containment check alone would accept it), but one of its
// *.json entries is individually a symlink pointing at real data outside
// that root. requireFixturesUnderRepo's own doc comment requires this to
// be caught too — exercised here at rejectSymlinkedJSONOutside's own level
// (the component requireFixturesUnderRepo calls after the directory-level
// check passes) so it does not depend on writing into this repository's
// actual eval/ask/fixtures tree to set up.
func TestRejectSymlinkedJSONOutside_RejectsSymlinkedFileResolvingOutside(t *testing.T) {
	allowedRoot := t.TempDir()
	realOutsideFile := filepath.Join(t.TempDir(), "real-personal-data.json")
	if err := os.WriteFile(realOutsideFile, []byte(`{"secret":true}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	symlinkedJSON := filepath.Join(allowedRoot, "innocuous-looking.json")
	if err := os.Symlink(realOutsideFile, symlinkedJSON); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}
	err := rejectSymlinkedJSONOutside(allowedRoot, allowedRoot)
	if err == nil {
		t.Fatalf("want an error for a *.json entry (%q) that is a symlink resolving outside %q, got nil", symlinkedJSON, allowedRoot)
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("want the error to identify the offending entry as a symlink, got: %v", err)
	}
}

// TestRejectSymlinkedJSONOutside_AllowsRegularFiles is the negative
// control for the test above: a directory containing only ordinary
// (non-symlink) *.json files, and non-.json entries of any kind, must
// never be rejected.
func TestRejectSymlinkedJSONOutside_AllowsRegularFiles(t *testing.T) {
	allowedRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(allowedRoot, "ok.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(allowedRoot, "README.md"), []byte("n/a"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Mkdir(filepath.Join(allowedRoot, "subdir"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := rejectSymlinkedJSONOutside(allowedRoot, allowedRoot); err != nil {
		t.Errorf("want no error for a directory of only regular files, got: %v", err)
	}
}

// TestRequireWithin exercises requireWithin's containment logic directly,
// including the exact edge case a naive strings.HasPrefix(target, root)
// check (without a separator) would get wrong: a sibling directory whose
// name happens to start with root's own name as a string
// (".../fixtures-evil" starting with ".../fixtures") must NOT be treated
// as "within" ".../fixtures".
func TestRequireWithin(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "repo", "eval", "ask", "fixtures")
	cases := []struct {
		name    string
		target  string
		wantErr bool
	}{
		{"root itself", root, false},
		{"direct child", filepath.Join(root, "a.json"), false},
		{"nested descendant", filepath.Join(root, "sub", "a.json"), false},
		{"parent directory", filepath.Dir(root), true},
		{"unrelated sibling", filepath.Join(filepath.Dir(root), "other"), true},
		{"name-prefix-only sibling (not a real descendant)", root + "-evil", true},
		{"completely different tree", filepath.Join(string(filepath.Separator), "etc", "passwd"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := requireWithin(root, c.target)
			if c.wantErr && err == nil {
				t.Errorf("requireWithin(%q, %q): want an error, got nil", root, c.target)
			}
			if !c.wantErr && err != nil {
				t.Errorf("requireWithin(%q, %q): want no error, got %v", root, c.target, err)
			}
		})
	}
}
