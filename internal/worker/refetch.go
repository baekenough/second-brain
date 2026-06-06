package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/baekenough/second-brain/internal/store"
)

// ErrRefetchNotSupported is returned by a Refetcher when it cannot handle the
// given failure record. The worker treats this as a permanent skip (same
// behaviour as the pre-Refetcher debug-log skip) so that sources without a
// re-fetch capability do not regress.
var ErrRefetchNotSupported = errors.New("refetch not supported for this source")

// RefetchResult is returned by a successful Refetcher.Refetch call.
// The caller is responsible for invoking Cleanup after the temp file is no
// longer needed.
type RefetchResult struct {
	// LocalPath is the absolute path to the downloaded temp file.
	LocalPath string
	// Cleanup removes the temp file. It is always non-nil. Callers must call
	// Cleanup exactly once (typically via defer) after extraction.
	Cleanup func()
}

// Refetcher can re-download a remote binary given an ExtractionFailure record
// and return a temporary local file path for the extractor to process.
//
// Implementations must:
//   - Return ErrRefetchNotSupported when they cannot handle the given record.
//   - Return a non-nil RefetchResult.Cleanup function on success.
//   - Respect ctx cancellation.
//   - Not panic.
type Refetcher interface {
	Refetch(ctx context.Context, f store.ExtractionFailure) (*RefetchResult, error)
}

// maxRefetchBytes is the hard cap for a single remote download via the
// URL-based refetcher. Oversized responses are rejected without buffering.
const maxRefetchBytes = 25 * 1024 * 1024 // 25 MB

// allowedRefetchHosts maps source type (lower-case) to the set of hostnames
// that are permitted as download targets. This allowlist prevents SSRF to
// internal/metadata endpoints and ensures credentials are only sent to known
// CDN hosts.
var allowedRefetchHosts = map[string]map[string]bool{
	"discord": {
		"cdn.discordapp.com":         true,
		"media.discordapp.net":       true,
		"attachments.discordapp.net": true,
	},
	"slack": {
		"files.slack.com": true,
		"slack-files.com": true,
	},
}

// isAllowedRefetchHost reports whether rawURL's hostname is in the allowlist
// for the given sourceType. Both rawURL's hostname and sourceType are compared
// case-insensitively. Returns false for unknown sourceTypes.
func isAllowedRefetchHost(rawURL, sourceType string) bool {
	allowed, ok := allowedRefetchHosts[strings.ToLower(sourceType)]
	if !ok {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return allowed[strings.ToLower(u.Hostname())]
}

// URLRefetcher re-downloads remote attachments whose FilePath is an HTTP(S)
// URL. It handles two source types:
//
//   - "discord": Discord CDN URLs (no authentication required; URLs are
//     pre-signed and time-limited by Discord). On expiry the download will
//     return an HTTP error and the worker will increment the attempt counter.
//
//   - "slack": Slack file URLs that require a Bearer token. Provide the token
//     via SlackBotToken; when empty, Slack records fall back to
//     ErrRefetchNotSupported (no regression).
//
// Any failure record whose FilePath does not start with "http://" or
// "https://" returns ErrRefetchNotSupported (preserving the existing skip
// behaviour for non-URL paths).
type URLRefetcher struct {
	// SlackBotToken is the Slack OAuth token used to authenticate file
	// downloads. Optional: when empty, Slack records are skipped.
	SlackBotToken string

	// client is the HTTP client used for downloads. When nil, http.DefaultClient
	// is used. Injected for testing.
	client *http.Client

	// hostChecker overrides isAllowedRefetchHost for testing. When nil the
	// package-level isAllowedRefetchHost is used.
	hostChecker func(rawURL, sourceType string) bool
}

// NewURLRefetcher returns a URLRefetcher. slackBotToken may be empty — Slack
// records will return ErrRefetchNotSupported when it is.
func NewURLRefetcher(slackBotToken string) *URLRefetcher {
	return &URLRefetcher{
		SlackBotToken: slackBotToken,
		client: &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				ResponseHeaderTimeout: 10 * time.Second,
			},
		},
	}
}

// checkHost invokes the injected hostChecker when present, falling back to the
// package-level allowlist function.
func (r *URLRefetcher) checkHost(rawURL, sourceType string) bool {
	if r.hostChecker != nil {
		return r.hostChecker(rawURL, sourceType)
	}
	return isAllowedRefetchHost(rawURL, sourceType)
}

// Refetch downloads the file at f.FilePath (which must be an HTTP/HTTPS URL)
// to a temporary local file and returns the path + a cleanup func.
//
// Returns ErrRefetchNotSupported when:
//   - f.FilePath is not an HTTP/HTTPS URL
//   - f.SourceType is "slack" and SlackBotToken is empty
//   - f.FilePath hostname is not in the allowlist for f.SourceType (SSRF guard)
func (r *URLRefetcher) Refetch(ctx context.Context, f store.ExtractionFailure) (*RefetchResult, error) {
	if !isHTTPURL(f.FilePath) {
		return nil, ErrRefetchNotSupported
	}

	// Hostname allowlist: reject SSRF attempts and unknown CDN hosts.
	// Treated as ErrRefetchNotSupported (permanent skip, no attempt-counter bump)
	// because the record is structurally invalid rather than transiently failing.
	if !r.checkHost(f.FilePath, f.SourceType) {
		return nil, ErrRefetchNotSupported
	}

	// Slack files require authentication; skip when the token is not available.
	if strings.EqualFold(f.SourceType, "slack") && r.SlackBotToken == "" {
		return nil, ErrRefetchNotSupported
	}

	data, err := r.download(ctx, f.FilePath, f.SourceType)
	if err != nil {
		return nil, fmt.Errorf("refetch download %q: %w", f.FilePath, err)
	}

	// Infer extension from the URL path for the temp file so extractors can
	// identify the format from the file extension.
	ext := filepath.Ext(urlPath(f.FilePath))

	tmpFile, err := os.CreateTemp("", "refetch-*"+ext)
	if err != nil {
		return nil, fmt.Errorf("refetch create temp: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("refetch write temp: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("refetch close temp: %w", err)
	}

	return &RefetchResult{
		LocalPath: tmpPath,
		Cleanup:   func() { os.Remove(tmpPath) },
	}, nil
}

// download fetches the URL and returns the body bytes, enforcing the size cap
// and injecting authentication headers for Slack.
func (r *URLRefetcher) download(ctx context.Context, rawURL, sourceType string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}

	if strings.EqualFold(sourceType, "slack") && r.SlackBotToken != "" {
		req.Header.Set("Authorization", "Bearer "+r.SlackBotToken)
	}

	client := r.client
	if client == nil {
		client = http.DefaultClient
	}

	// Build a copy of the client with a redirect policy that:
	//   (a) limits to 3 hops,
	//   (b) rejects redirects to hosts outside the allowlist (SSRF),
	//   (c) strips Authorization when the redirect host differs from the
	//       original (belt-and-suspenders token-leak guard).
	//
	// We shadow the client variable rather than mutating the shared struct so
	// the injected test client is never modified.
	originalHost := strings.ToLower(req.URL.Hostname())
	redirectingClient := *client
	redirectingClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return fmt.Errorf("stopped after 3 redirects")
		}
		redirectHost := strings.ToLower(req.URL.Hostname())
		if !r.checkHost(req.URL.String(), sourceType) {
			return fmt.Errorf("redirect to disallowed host %q", redirectHost)
		}
		if redirectHost != originalHost {
			req.Header.Del("Authorization")
		}
		return nil
	}

	resp, err := redirectingClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP status %d for %q", resp.StatusCode, rawURL)
	}

	// Read up to maxRefetchBytes+1 to detect oversized responses without fully
	// buffering them in memory (mirrors discord.go downloadAttachment).
	buf := &bytes.Buffer{}
	if _, err := io.CopyN(buf, resp.Body, int64(maxRefetchBytes)+1); err != nil && err != io.EOF {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if buf.Len() > maxRefetchBytes {
		return nil, fmt.Errorf("response body exceeds %d bytes", maxRefetchBytes)
	}
	return buf.Bytes(), nil
}

// isHTTPURL returns true when s begins with "http://" or "https://".
func isHTTPURL(s string) bool {
	l := strings.ToLower(s)
	return strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://")
}

// urlPath returns the path component of a URL string.
// Falls back to the raw string on parse error (best-effort extension inference).
func urlPath(rawURL string) string {
	// Simple extraction: find the path after the host without importing net/url
	// to keep the function cheap and allocation-free on the hot path.
	s := rawURL
	// Strip scheme.
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	// Strip query/fragment.
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	// Strip host (first path component before /).
	if i := strings.Index(s, "/"); i >= 0 {
		return s[i:]
	}
	return "/" + s
}
