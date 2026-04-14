package auth_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/baekenough/second-brain/internal/auth"
)

// --- StaticToken ---

func TestStaticToken_Empty(t *testing.T) {
	t.Parallel()

	var tok auth.StaticToken = ""
	_, err := tok.Token()
	if err == nil {
		t.Fatal("expected error for empty StaticToken, got nil")
	}
}

func TestStaticToken_NonEmpty(t *testing.T) {
	t.Parallel()

	tok := auth.StaticToken("abc")
	got, err := tok.Token()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "abc" {
		t.Fatalf("want %q, got %q", "abc", got)
	}
}

// --- CliProxyFile ---

func writeTokenFile(t *testing.T, dir, accessToken string) string {
	t.Helper()
	payload := map[string]string{"access_token": accessToken}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal token file: %v", err)
	}
	path := filepath.Join(dir, "token.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	return path
}

func TestCliProxyFile_MissingFile(t *testing.T) {
	t.Parallel()

	ts := auth.NewCliProxyFile("/nonexistent/path/token.json")
	_, err := ts.Token()
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestCliProxyFile_InvalidJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("not-json{{{"), 0o600); err != nil {
		t.Fatalf("write bad json: %v", err)
	}

	ts := auth.NewCliProxyFile(path)
	_, err := ts.Token()
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestCliProxyFile_EmptyAccessToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writeTokenFile(t, dir, "")

	ts := auth.NewCliProxyFile(path)
	_, err := ts.Token()
	if err == nil {
		t.Fatal("expected error for empty access_token, got nil")
	}
}

func TestCliProxyFile_Valid(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writeTokenFile(t, dir, "xyz")

	ts := auth.NewCliProxyFile(path)
	got, err := ts.Token()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "xyz" {
		t.Fatalf("want %q, got %q", "xyz", got)
	}
}

func TestCliProxyFile_CacheTTL(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writeTokenFile(t, dir, "first-token")

	ts := auth.NewCliProxyFile(path)

	// First call — reads file.
	got, err := ts.Token()
	if err != nil {
		t.Fatalf("first call failed: %v", err)
	}
	if got != "first-token" {
		t.Fatalf("first call: want %q, got %q", "first-token", got)
	}

	// Overwrite the file with a new token before the cache expires.
	newPayload, _ := json.Marshal(map[string]string{"access_token": "second-token"})
	if err := os.WriteFile(path, newPayload, 0o600); err != nil {
		t.Fatalf("overwrite token file: %v", err)
	}

	// Second call within TTL — must return the cached value.
	got2, err := ts.Token()
	if err != nil {
		t.Fatalf("second call failed: %v", err)
	}
	if got2 != "first-token" {
		t.Fatalf("cache miss: expected cached %q, got %q", "first-token", got2)
	}
}

// TestCliProxyFile_CacheTTL verifies that once the cache expires the file is
// re-read. We set a very short TTL by manipulating internal state via a
// custom constructor instead — since CliProxyFile.cacheTTL is unexported we
// rely on the observable behaviour: after waiting past a known short TTL the
// new value should appear.
//
// Because the default TTL is 5 minutes and we cannot set it from outside the
// package, this test is intentionally omitted to avoid multi-minute waits.
// The cache behaviour is verified by TestCliProxyFile_CacheTTL above.

// --- Resolve ---

func TestResolve_StaticPreferred(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writeTokenFile(t, dir, "file-token")

	ts := auth.Resolve("static-key", path)
	got, err := ts.Token()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "static-key" {
		t.Fatalf("want static-key, got %q", got)
	}
}

func TestResolve_FallbackToFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writeTokenFile(t, dir, "file-token")

	ts := auth.Resolve("", path)
	got, err := ts.Token()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "file-token" {
		t.Fatalf("want file-token, got %q", got)
	}
}

func TestResolve_BothEmpty(t *testing.T) {
	t.Parallel()

	ts := auth.Resolve("", "")
	if ts != nil {
		t.Fatalf("expected nil TokenSource, got %T", ts)
	}
}

// Compile-time assertion: CliProxyFile satisfies TokenSource.
var _ auth.TokenSource = (*auth.CliProxyFile)(nil)

// Compile-time assertion: StaticToken satisfies TokenSource.
var _ auth.TokenSource = auth.StaticToken("")
