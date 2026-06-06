package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/store"
)

// ---------------------------------------------------------------------------
// isAllowedRefetchHost
// ---------------------------------------------------------------------------

func TestIsAllowedRefetchHost(t *testing.T) {
	t.Parallel()

	cases := []struct {
		rawURL     string
		sourceType string
		want       bool
	}{
		// Discord allowed hosts.
		{"https://cdn.discordapp.com/attachments/123/456/file.pdf", "discord", true},
		{"https://cdn.discordapp.com/attachments/123/456/file.pdf", "DISCORD", true}, // case-insensitive sourceType
		{"https://media.discordapp.net/attachments/123/456/img.png", "discord", true},
		{"https://attachments.discordapp.net/abc/file.docx", "discord", true},
		// Discord disallowed (wrong CDN / internal).
		{"https://files.slack.com/file.pdf", "discord", false},
		{"http://169.254.169.254/latest/meta-data/", "discord", false},
		{"https://evil.example.com/file.pdf", "discord", false},
		// Slack allowed hosts.
		{"https://files.slack.com/files-pri/T00/F00/report.pdf", "slack", true},
		{"https://slack-files.com/abc/file.pdf", "slack", true},
		// Slack disallowed.
		{"https://cdn.discordapp.com/att.pdf", "slack", false},
		{"https://evil.example.com/file.pdf", "slack", false},
		// Unknown source type — always denied.
		{"https://cdn.discordapp.com/att.pdf", "googledrive", false},
		{"https://cdn.discordapp.com/att.pdf", "", false},
		// Malformed URL — denied.
		{"://bad-url", "discord", false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.sourceType+"|"+tc.rawURL, func(t *testing.T) {
			t.Parallel()
			if got := isAllowedRefetchHost(tc.rawURL, tc.sourceType); got != tc.want {
				t.Errorf("isAllowedRefetchHost(%q, %q) = %v, want %v", tc.rawURL, tc.sourceType, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// URLRefetcher.Refetch: disallowed host → ErrRefetchNotSupported (no download)
// ---------------------------------------------------------------------------

func TestURLRefetcher_DisallowedHost_NotSupported(t *testing.T) {
	t.Parallel()

	// httptest server that should never be called.
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := NewURLRefetcher("")
	r.client = srv.Client()

	f := store.ExtractionFailure{
		// sourceType "discord" but host points to the test server (not an
		// allowed Discord CDN host) — simulates an SSRF attempt.
		SourceType: "discord",
		SourceID:   "att-ssrf",
		FilePath:   srv.URL + "/internal/secret",
	}

	result, err := r.Refetch(context.Background(), f)
	if result != nil {
		result.Cleanup()
		t.Errorf("expected nil result for disallowed host, got %+v", result)
	}
	if err != ErrRefetchNotSupported {
		t.Errorf("expected ErrRefetchNotSupported for disallowed host, got %v", err)
	}
	if called {
		t.Error("HTTP request was made despite disallowed host — SSRF guard failed")
	}
}

// ---------------------------------------------------------------------------
// isHTTPURL
// ---------------------------------------------------------------------------

func TestIsHTTPURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  bool
	}{
		{"https://cdn.discordapp.com/att.pdf", true},
		{"http://files.example.com/f.docx", true},
		{"HTTPS://cdn.example.com/FILE.PDF", true},
		{"/tmp/local.pdf", false},
		{"relative/path.pdf", false},
		{"slack://files/abc", false},
		{"ftp://host/file", false},
		{"", false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			if got := isHTTPURL(tc.input); got != tc.want {
				t.Errorf("isHTTPURL(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// urlPath
// ---------------------------------------------------------------------------

func TestURLPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  string
	}{
		{"https://cdn.discordapp.com/attach/123/report.pdf", "/attach/123/report.pdf"},
		{"https://cdn.example.com/file.pdf?ex=abc&is=def", "/file.pdf"},
		{"https://host/", "/"},
		{"https://host/path#fragment", "/path"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			if got := urlPath(tc.input); got != tc.want {
				t.Errorf("urlPath(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// URLRefetcher.Refetch: non-HTTP path → ErrRefetchNotSupported
// ---------------------------------------------------------------------------

func TestURLRefetcher_NonHTTPPath_NotSupported(t *testing.T) {
	t.Parallel()

	r := NewURLRefetcher("")
	f := store.ExtractionFailure{
		SourceType: "filesystem",
		SourceID:   "local",
		FilePath:   "/tmp/file.pdf",
	}

	result, err := r.Refetch(context.Background(), f)
	if result != nil {
		t.Errorf("expected nil result, got %+v", result)
	}
	if err != ErrRefetchNotSupported {
		t.Errorf("expected ErrRefetchNotSupported, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// URLRefetcher.Refetch: Slack URL without token → ErrRefetchNotSupported
// ---------------------------------------------------------------------------

func TestURLRefetcher_SlackWithoutToken_NotSupported(t *testing.T) {
	t.Parallel()

	r := NewURLRefetcher("") // empty token
	f := store.ExtractionFailure{
		SourceType: "slack",
		SourceID:   "file-1",
		FilePath:   "https://files.slack.com/files-pri/T00/F00/report.pdf",
	}

	result, err := r.Refetch(context.Background(), f)
	if result != nil {
		t.Errorf("expected nil result, got %+v", result)
	}
	if err != ErrRefetchNotSupported {
		t.Errorf("expected ErrRefetchNotSupported, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// URLRefetcher.Refetch: successful download → temp file written + cleaned up
// ---------------------------------------------------------------------------

func TestURLRefetcher_SuccessfulDownload(t *testing.T) {
	t.Parallel()

	const payload = "binary content of attachment"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(payload)) //nolint:errcheck
	}))
	defer srv.Close()

	r := NewURLRefetcher("")
	// Inject test HTTP client that points at the test server.
	r.client = srv.Client()
	// Bypass allowlist: httptest uses 127.0.0.1 which is not a real CDN host.
	r.hostChecker = func(_, _ string) bool { return true }

	f := store.ExtractionFailure{
		SourceType: "discord",
		SourceID:   "att-1",
		FilePath:   srv.URL + "/attach/123/report.pdf",
	}

	result, err := r.Refetch(context.Background(), f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Cleanup == nil {
		t.Fatal("expected non-nil Cleanup func")
	}

	// Verify temp file has correct content.
	data, readErr := os.ReadFile(result.LocalPath)
	if readErr != nil {
		t.Fatalf("read temp file: %v", readErr)
	}
	if string(data) != payload {
		t.Errorf("temp file content = %q, want %q", string(data), payload)
	}

	// Verify extension is derived from URL path (.pdf).
	if !strings.HasSuffix(result.LocalPath, ".pdf") {
		t.Errorf("expected temp file to end with .pdf, got %q", result.LocalPath)
	}

	// Call Cleanup and verify temp file is removed.
	result.Cleanup()
	if _, err := os.Stat(result.LocalPath); !os.IsNotExist(err) {
		t.Errorf("temp file still exists after Cleanup: %s", result.LocalPath)
	}
}

// ---------------------------------------------------------------------------
// URLRefetcher.Refetch: Slack — token injected as Authorization header
// ---------------------------------------------------------------------------

func TestURLRefetcher_SlackBearerHeader(t *testing.T) {
	t.Parallel()

	const token = "xoxb-test-token"
	var receivedAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("slack content")) //nolint:errcheck
	}))
	defer srv.Close()

	r := NewURLRefetcher(token)
	r.client = srv.Client()
	// Bypass allowlist: httptest uses 127.0.0.1 which is not a real CDN host.
	r.hostChecker = func(_, _ string) bool { return true }

	f := store.ExtractionFailure{
		SourceType: "slack",
		SourceID:   "F001",
		FilePath:   srv.URL + "/files/report.pdf",
	}

	result, err := r.Refetch(context.Background(), f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	result.Cleanup()

	if receivedAuth != "Bearer "+token {
		t.Errorf("Authorization header = %q, want %q", receivedAuth, "Bearer "+token)
	}
}

// ---------------------------------------------------------------------------
// URLRefetcher.Refetch: HTTP 403 → error returned (not ErrRefetchNotSupported)
// ---------------------------------------------------------------------------

func TestURLRefetcher_HTTP403_ReturnsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	r := NewURLRefetcher("")
	r.client = srv.Client()
	// Bypass allowlist: httptest uses 127.0.0.1 which is not a real CDN host.
	r.hostChecker = func(_, _ string) bool { return true }

	f := store.ExtractionFailure{
		SourceType: "discord",
		SourceID:   "att-expired",
		FilePath:   srv.URL + "/expired.pdf",
	}

	result, err := r.Refetch(context.Background(), f)
	if result != nil {
		result.Cleanup()
		t.Errorf("expected nil result on HTTP error, got %+v", result)
	}
	if err == nil {
		t.Fatal("expected error for HTTP 403, got nil")
	}
	if err == ErrRefetchNotSupported {
		t.Error("error should NOT be ErrRefetchNotSupported for HTTP 403 — attempt counter must be incremented")
	}
}
