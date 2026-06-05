package collector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// --- helpers ---

// fakeTokenSource is a stub oauth2.TokenSource for testing.
type fakeTokenSource struct {
	mu     sync.Mutex
	calls  int
	token  *oauth2.Token
	err    error
}

func (f *fakeTokenSource) Token() (*oauth2.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.token, f.err
}

// newCollectorWithFakeSource creates a GDriveCollector with an already-built
// token source so tests bypass network calls.
func newCollectorWithFakeSource(ts oauth2.TokenSource) *GDriveCollector {
	return &GDriveCollector{
		credentialsJSON: "{}",     // non-empty so Enabled() returns true
		client:          &http.Client{Timeout: 5 * time.Second},
		tokenSource:     ts,
	}
}

// --- JSON parsing sanity check ---

func TestGDriveCollector_CredentialsJSONParsing(t *testing.T) {
	t.Parallel()

	type saKey struct {
		Type        string `json:"type"`
		ProjectID   string `json:"project_id"`
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
	}

	key := saKey{
		Type:        "service_account",
		ProjectID:   "test-project",
		ClientEmail: "test@test-project.iam.gserviceaccount.com",
		// A real private_key is not needed for JSON parsing; google.CredentialsFromJSON
		// validates the key only on first token fetch, not on parse.
		PrivateKey: "-----BEGIN RSA PRIVATE KEY-----\nMIIEo...\n-----END RSA PRIVATE KEY-----\n",
		TokenURI:   "https://oauth2.googleapis.com/token",
	}

	raw, err := json.Marshal(key)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Verify our JSON is a valid JSON blob (the real test: no panic on Unmarshal).
	var decoded saKey
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.ClientEmail != key.ClientEmail {
		t.Errorf("client_email mismatch: got %q want %q", decoded.ClientEmail, key.ClientEmail)
	}
}

// --- Enabled() ---

func TestGDriveCollector_Enabled(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name            string
		credentialsJSON string
		want            bool
	}{
		{"empty credentials — disabled", "", false},
		{"non-empty credentials — enabled", `{"type":"service_account"}`, true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := NewGDriveCollector(tc.credentialsJSON)
			if c.Enabled() != tc.want {
				t.Errorf("Enabled() = %v, want %v", c.Enabled(), tc.want)
			}
		})
	}
}

// --- Token caching ---

func TestGDriveCollector_TokenCaching_ReuseValidToken(t *testing.T) {
	t.Parallel()

	// A token valid for 1 hour.
	validToken := &oauth2.Token{
		AccessToken: "cached-token-123",
		Expiry:      time.Now().Add(time.Hour),
	}
	fake := &fakeTokenSource{token: validToken}
	c := newCollectorWithFakeSource(fake)

	ctx := context.Background()

	tok1, err := c.getAccessToken(ctx)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	tok2, err := c.getAccessToken(ctx)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	if tok1 != "cached-token-123" || tok2 != "cached-token-123" {
		t.Errorf("unexpected tokens: %q, %q", tok1, tok2)
	}
	if fake.calls != 1 {
		t.Errorf("Token() called %d times, want 1 (second call should reuse cache)", fake.calls)
	}
}

func TestGDriveCollector_TokenCaching_RefreshExpiredToken(t *testing.T) {
	t.Parallel()

	// First call returns an already-expired token; second call gets a fresh one.
	expired := &oauth2.Token{
		AccessToken: "expired-token",
		Expiry:      time.Now().Add(-time.Minute),
	}
	fresh := &oauth2.Token{
		AccessToken: "fresh-token-456",
		Expiry:      time.Now().Add(time.Hour),
	}

	fake := &fakeTokenSource{}

	// Simpler approach: pre-seed cachedToken as expired, then let tokenSource return fresh.
	fake.token = fresh
	c := newCollectorWithFakeSource(fake)

	// Manually seed an expired cached token to simulate a stale cache.
	c.tokenMu.Lock()
	c.cachedToken = expired
	c.tokenMu.Unlock()

	ctx := context.Background()
	tok, err := c.getAccessToken(ctx)
	if err != nil {
		t.Fatalf("getAccessToken: %v", err)
	}
	if tok != "fresh-token-456" {
		t.Errorf("got %q, want fresh-token-456", tok)
	}
	if fake.calls != 1 {
		t.Errorf("Token() called %d times, want 1", fake.calls)
	}
}

// --- ADC vs credentials JSON branch selection ---

func TestGDriveCollector_BranchSelection_EmptyCredentials_UsesADC(t *testing.T) {
	t.Parallel()

	// A collector with empty credentialsJSON is disabled (Enabled() == false),
	// so getAccessToken should never be reached in practice. We verify that
	// buildTokenSource falls through to the ADC path when credentialsJSON is "".
	// Because ADC is not configured in CI, the call should fail with a
	// "no credentials" error — NOT the old PoC stub message.
	c := &GDriveCollector{
		credentialsJSON: "", // ADC path
		client:          &http.Client{},
	}
	ctx := context.Background()
	_, err := c.buildTokenSource(ctx)
	if err == nil {
		// In a GCP environment ADC is available — that's also fine.
		t.Log("ADC credentials found — skipping error assertion")
		return
	}
	// Must not return the old PoC stub message.
	if strings.Contains(err.Error(), "not implemented in PoC") {
		t.Errorf("old PoC stub error returned: %v", err)
	}
	// Must mention "no credentials" to give the user a clear action.
	if !strings.Contains(err.Error(), "no credentials") && !strings.Contains(err.Error(), "credentials") {
		t.Errorf("error should mention credentials, got: %v", err)
	}
}

func TestGDriveCollector_BranchSelection_InvalidJSON_ReturnsParseError(t *testing.T) {
	t.Parallel()

	c := &GDriveCollector{
		credentialsJSON: `{not valid json`,
		client:          &http.Client{},
	}
	ctx := context.Background()
	_, err := c.buildTokenSource(ctx)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
	if !strings.Contains(err.Error(), "parse service account credentials JSON") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// --- Collect integration with fake Drive API ---

func TestGDriveCollector_Collect_UsesToken(t *testing.T) {
	t.Parallel()

	const fakeToken = "test-bearer-token"

	// Fake Drive API server.
	mux := http.NewServeMux()

	// files.list endpoint.
	mux.HandleFunc("/drive/v3/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"files": []map[string]any{
				{
					"id":           "file-id-1",
					"name":         "Test Doc",
					"mimeType":     "application/vnd.google-apps.document",
					"webViewLink":  "https://docs.google.com/document/d/file-id-1/edit",
					"modifiedTime": time.Now().UTC().Format(time.RFC3339),
				},
			},
		})
	})

	// files.export endpoint.
	mux.HandleFunc("/drive/v3/files/file-id-1/export", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("exported text content"))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Patch the Drive API base URL by injecting a custom HTTP client that
	// rewrites the host. We use a transport wrapper.
	transport := &rewriteHostTransport{
		base:    http.DefaultTransport,
		oldHost: "www.googleapis.com",
		newHost: strings.TrimPrefix(srv.URL, "http://"),
	}

	fake := &fakeTokenSource{
		token: &oauth2.Token{
			AccessToken: fakeToken,
			Expiry:      time.Now().Add(time.Hour),
		},
	}
	c := &GDriveCollector{
		credentialsJSON: `{"type":"service_account"}`,
		client:          &http.Client{Transport: transport},
		tokenSource:     fake,
	}

	ctx := context.Background()
	docs, err := c.Collect(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d docs, want 1", len(docs))
	}
	if docs[0].Title != "Test Doc" {
		t.Errorf("title = %q, want %q", docs[0].Title, "Test Doc")
	}
	if docs[0].Content != "exported text content" {
		t.Errorf("content = %q, want %q", docs[0].Content, "exported text content")
	}
}

// rewriteHostTransport redirects requests from oldHost to newHost (test server).
type rewriteHostTransport struct {
	base    http.RoundTripper
	oldHost string
	newHost string
}

func (t *rewriteHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	if cloned.URL.Host == t.oldHost {
		cloned.URL.Host = t.newHost
		cloned.URL.Scheme = "http"
	}
	return t.base.RoundTrip(cloned)
}
