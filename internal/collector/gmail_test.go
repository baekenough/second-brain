package collector

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/model"
)

// --- helpers ---

// newGmailCollectorWithFakeSource creates a GmailCollector with an already-built
// token source so tests bypass file I/O and network calls.
func newGmailCollectorWithFakeSource(cfg *config.Config, srv *httptest.Server, ts oauth2.TokenSource) *GmailCollector {
	return &GmailCollector{
		cfg:         cfg,
		httpClient:  srv.Client(),
		baseURL:     srv.URL,
		tokenSource: ts,
		cachedToken: &oauth2.Token{
			AccessToken: "test-token",
			Expiry:      time.Now().Add(time.Hour),
		},
	}
}

// validGmailConfig returns a minimal config with non-empty credential paths.
func validGmailConfig() *config.Config {
	return &config.Config{
		GmailCredentialsJSON: "/fake/credentials.json",
		GmailTokenJSON:       "/fake/token.json",
		GmailQuery:           "-in:spam -in:trash",
	}
}

// gmailMessageJSON builds a minimal Gmail API message resource for tests.
func gmailMessageJSON(id, threadID, subject, from, to, dateStr, bodyText string) map[string]any {
	encoded := base64.URLEncoding.EncodeToString([]byte(bodyText))
	return map[string]any{
		"id":           id,
		"threadId":     threadID,
		"labelIds":     []string{"INBOX"},
		"internalDate": "1686000000000",
		"payload": map[string]any{
			"mimeType": "text/plain",
			"headers": []map[string]any{
				{"name": "Subject", "value": subject},
				{"name": "From", "value": from},
				{"name": "To", "value": to},
				{"name": "Date", "value": dateStr},
			},
			"body": map[string]any{"data": encoded},
		},
	}
}

// --- Enabled() ---

func TestGmailCollector_Enabled(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		cfg   *config.Config
		want  bool
	}{
		{
			name: "both paths set — enabled",
			cfg:  &config.Config{GmailCredentialsJSON: "/creds.json", GmailTokenJSON: "/token.json"},
			want: true,
		},
		{
			name: "credentials missing — disabled",
			cfg:  &config.Config{GmailCredentialsJSON: "", GmailTokenJSON: "/token.json"},
			want: false,
		},
		{
			name: "token missing — disabled",
			cfg:  &config.Config{GmailCredentialsJSON: "/creds.json", GmailTokenJSON: ""},
			want: false,
		},
		{
			name: "both empty — disabled",
			cfg:  &config.Config{},
			want: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := NewGmailCollector(tc.cfg)
			if c.Enabled() != tc.want {
				t.Errorf("Enabled() = %v, want %v", c.Enabled(), tc.want)
			}
		})
	}
}

// --- decodeBase64URL ---

func TestDecodeBase64URL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "standard padded base64url",
			input: base64.URLEncoding.EncodeToString([]byte("Hello, World!")),
			want:  "Hello, World!",
		},
		{
			name:  "raw (no-padding) base64url",
			input: base64.RawURLEncoding.EncodeToString([]byte("Hello!")),
			want:  "Hello!",
		},
		{
			name:  "invalid base64 returns empty",
			input: "not-valid!!!",
			want:  "",
		},
		{
			name:  "empty string returns empty",
			input: "",
			want:  "",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := decodeBase64URL(tc.input)
			if got != tc.want {
				t.Errorf("decodeBase64URL(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// --- extractGmailBody ---

func TestExtractGmailBody_PlainText(t *testing.T) {
	t.Parallel()

	body := "This is the plain text body."
	encoded := base64.URLEncoding.EncodeToString([]byte(body))

	part := &gmailMessagePart{
		MimeType: "text/plain",
		Body:     gmailPartBody{Data: encoded},
	}
	got := extractGmailBody(part)
	if got != body {
		t.Errorf("got %q, want %q", got, body)
	}
}

func TestExtractGmailBody_HTMLFallback(t *testing.T) {
	t.Parallel()

	htmlBody := "<html><body><p>Hello <b>World</b></p></body></html>"
	encoded := base64.URLEncoding.EncodeToString([]byte(htmlBody))

	part := &gmailMessagePart{
		MimeType: "text/html",
		Body:     gmailPartBody{Data: encoded},
	}
	got := extractGmailBody(part)
	// Tags should be stripped.
	if strings.Contains(got, "<") || strings.Contains(got, ">") {
		t.Errorf("HTML tags not stripped: %q", got)
	}
	if !strings.Contains(got, "Hello") || !strings.Contains(got, "World") {
		t.Errorf("expected text content, got: %q", got)
	}
}

func TestExtractGmailBody_MultipartPrefersPlain(t *testing.T) {
	t.Parallel()

	plainBody := "Plain text version"
	htmlBody := "<html><body>HTML version</body></html>"

	plainEncoded := base64.URLEncoding.EncodeToString([]byte(plainBody))
	htmlEncoded := base64.URLEncoding.EncodeToString([]byte(htmlBody))

	part := &gmailMessagePart{
		MimeType: "multipart/alternative",
		Parts: []gmailMessagePart{
			{
				MimeType: "text/html",
				Body:     gmailPartBody{Data: htmlEncoded},
			},
			{
				MimeType: "text/plain",
				Body:     gmailPartBody{Data: plainEncoded},
			},
		},
	}
	got := extractGmailBody(part)
	if got != plainBody {
		t.Errorf("expected plain text, got %q", got)
	}
}

func TestExtractGmailBody_Nil(t *testing.T) {
	t.Parallel()

	got := extractGmailBody(nil)
	if got != "" {
		t.Errorf("expected empty string for nil part, got %q", got)
	}
}

// --- stripHTMLTags ---

func TestStripHTMLTags(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "simple tag",
			input: "<b>bold</b>",
			want:  "bold",
		},
		{
			name:  "nested tags",
			input: "<html><body><p>Hello</p></body></html>",
			want:  "Hello",
		},
		{
			name:  "no tags",
			input: "plain text",
			want:  "plain text",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := stripHTMLTags(tc.input)
			if got != tc.want {
				t.Errorf("stripHTMLTags(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// --- buildTokenSource (file path parsing) ---

func TestGmailCollector_BuildTokenSource_MissingFiles(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		GmailCredentialsJSON: "/nonexistent/credentials.json",
		GmailTokenJSON:       "/nonexistent/token.json",
	}
	c := NewGmailCollector(cfg)
	_, err := c.buildTokenSource(context.Background())
	if err == nil {
		t.Fatal("expected error for missing credentials file, got nil")
	}
	if !strings.Contains(err.Error(), "credentials") {
		t.Errorf("expected error to mention 'credentials', got: %v", err)
	}
}

func TestGmailCollector_BuildTokenSource_InvalidCredentialsJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	credPath := filepath.Join(dir, "credentials.json")
	tokenPath := filepath.Join(dir, "token.json")

	// Write invalid JSON.
	if err := os.WriteFile(credPath, []byte("{invalid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		GmailCredentialsJSON: credPath,
		GmailTokenJSON:       tokenPath,
	}
	c := NewGmailCollector(cfg)
	_, err := c.buildTokenSource(context.Background())
	if err == nil {
		t.Fatal("expected error for invalid credentials JSON, got nil")
	}
	if !strings.Contains(err.Error(), "parse credentials") {
		t.Errorf("expected parse error, got: %v", err)
	}
}

func TestGmailCollector_BuildTokenSource_NoClientID(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	credPath := filepath.Join(dir, "credentials.json")
	tokenPath := filepath.Join(dir, "token.json")

	// Credentials JSON without installed or web key.
	if err := os.WriteFile(credPath, []byte(`{"other":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tokenJSON := gmailTestTokenJSON("refresh-token-abc", "access-token-xyz")
	if err := os.WriteFile(tokenPath, tokenJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		GmailCredentialsJSON: credPath,
		GmailTokenJSON:       tokenPath,
	}
	c := NewGmailCollector(cfg)
	_, err := c.buildTokenSource(context.Background())
	if err == nil {
		t.Fatal("expected error when client_id is missing")
	}
	if !strings.Contains(err.Error(), "client_id") {
		t.Errorf("expected error about client_id, got: %v", err)
	}
}

// gmailTestTokenJSON returns a minimal token.json bytes.
func gmailTestTokenJSON(refreshToken, accessToken string) []byte {
	tok := map[string]any{
		"access_token":  accessToken,
		"refresh_token": refreshToken,
		"token_type":    "Bearer",
		"expiry":        time.Now().Add(time.Hour).Format(time.RFC3339),
	}
	b, _ := json.Marshal(tok)
	return b
}

// gmailTestCredentialsJSON returns a minimal credentials.json bytes (installed app).
func gmailTestCredentialsJSON(tokenServerURL string) []byte {
	creds := map[string]any{
		"installed": map[string]any{
			"client_id":     "test-client-id",
			"client_secret": "test-client-secret",
			"token_uri":     tokenServerURL,
			"redirect_uris": []string{"urn:ietf:wg:oauth:2.0:oob"},
		},
	}
	b, _ := json.Marshal(creds)
	return b
}

// --- Collect with httptest server ---

func TestGmailCollector_Collect_SingleMessage(t *testing.T) {
	t.Parallel()

	const (
		msgID     = "msg-001"
		threadID  = "thread-001"
		subject   = "Test Subject"
		fromAddr  = "sender@example.com"
		toAddr    = "receiver@example.com"
		dateStr   = "Mon, 06 Jun 2022 10:00:00 +0000"
		bodyText  = "Hello from Gmail test."
	)

	mux := http.NewServeMux()

	// messages.list
	mux.HandleFunc("/gmail/v1/users/me/messages", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if !strings.Contains(q, "after:") {
			// since is non-zero in the call below; verify the after filter is applied.
			t.Errorf("expected 'after:' in query parameter, got %q", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]any{{"id": msgID}},
		})
	})

	// messages.get
	mux.HandleFunc("/gmail/v1/users/me/messages/"+msgID, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(gmailMessageJSON(msgID, threadID, subject, fromAddr, toAddr, dateStr, bodyText))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := validGmailConfig()
	c := newGmailCollectorWithFakeSource(cfg, srv, nil)

	since := time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)
	docs, err := c.Collect(context.Background(), since)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d docs, want 1", len(docs))
	}

	doc := docs[0]
	if doc.SourceType != model.SourceGmail {
		t.Errorf("SourceType = %q, want %q", doc.SourceType, model.SourceGmail)
	}
	if doc.SourceID != "gmail:"+msgID {
		t.Errorf("SourceID = %q, want %q", doc.SourceID, "gmail:"+msgID)
	}
	if doc.Title != subject {
		t.Errorf("Title = %q, want %q", doc.Title, subject)
	}
	if doc.Content != bodyText {
		t.Errorf("Content = %q, want %q", doc.Content, bodyText)
	}
	if doc.OccurredAt == nil {
		t.Error("OccurredAt is nil, want non-nil")
	}
	if doc.Metadata["thread_id"] != threadID {
		t.Errorf("Metadata.thread_id = %v, want %q", doc.Metadata["thread_id"], threadID)
	}
	if doc.Metadata["from"] != fromAddr {
		t.Errorf("Metadata.from = %v, want %q", doc.Metadata["from"], fromAddr)
	}
}

func TestGmailCollector_Collect_Pagination(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	callCount := 0

	mux.HandleFunc("/gmail/v1/users/me/messages", func(w http.ResponseWriter, r *http.Request) {
		callCount++
		pt := r.URL.Query().Get("pageToken")
		w.Header().Set("Content-Type", "application/json")
		if pt == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"messages":      []map[string]any{{"id": "msg-page1"}},
				"nextPageToken": "page2",
			})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"messages": []map[string]any{{"id": "msg-page2"}},
			})
		}
	})

	for _, id := range []string{"msg-page1", "msg-page2"} {
		id := id
		mux.HandleFunc("/gmail/v1/users/me/messages/"+id, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(gmailMessageJSON(
				id, "t-"+id, "Subj "+id, "from@x.com", "to@x.com",
				"Mon, 06 Jun 2022 10:00:00 +0000", "body "+id,
			))
		})
	}

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := newGmailCollectorWithFakeSource(validGmailConfig(), srv, nil)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 2 {
		t.Errorf("got %d docs, want 2", len(docs))
	}
	if callCount != 2 {
		t.Errorf("list called %d times, want 2 (pagination)", callCount)
	}
}

func TestGmailCollector_Collect_MessageFetchError_Skipped(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()

	mux.HandleFunc("/gmail/v1/users/me/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]any{
				{"id": "good-msg"},
				{"id": "bad-msg"},
			},
		})
	})

	mux.HandleFunc("/gmail/v1/users/me/messages/good-msg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(gmailMessageJSON(
			"good-msg", "thread-1", "Good", "a@a.com", "b@b.com",
			"Mon, 06 Jun 2022 10:00:00 +0000", "good body",
		))
	})

	mux.HandleFunc("/gmail/v1/users/me/messages/bad-msg", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := newGmailCollectorWithFakeSource(validGmailConfig(), srv, nil)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect returned unexpected error: %v", err)
	}
	// bad-msg is skipped; only good-msg collected.
	if len(docs) != 1 {
		t.Errorf("got %d docs, want 1 (bad message skipped)", len(docs))
	}
	if docs[0].SourceID != "gmail:good-msg" {
		t.Errorf("unexpected SourceID: %q", docs[0].SourceID)
	}
}

func TestGmailCollector_Collect_SinceZero_NoAfterFilter(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()

	mux.HandleFunc("/gmail/v1/users/me/messages", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if strings.Contains(q, "after:") {
			t.Errorf("zero since should not produce 'after:' filter, got q=%q", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": nil})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := newGmailCollectorWithFakeSource(validGmailConfig(), srv, nil)
	_, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
}

func TestGmailCollector_Collect_MultipartBody(t *testing.T) {
	t.Parallel()

	const plainText = "This is the plain text part."
	const htmlText = "<p>This is HTML part.</p>"
	plainEncoded := base64.URLEncoding.EncodeToString([]byte(plainText))
	htmlEncoded := base64.URLEncoding.EncodeToString([]byte(htmlText))

	msg := map[string]any{
		"id":           "multipart-001",
		"threadId":     "thread-multi",
		"labelIds":     []string{"INBOX"},
		"internalDate": "1686000000000",
		"payload": map[string]any{
			"mimeType": "multipart/alternative",
			"headers": []map[string]any{
				{"name": "Subject", "value": "Multipart Email"},
				{"name": "From", "value": "multi@example.com"},
				{"name": "To", "value": "me@example.com"},
				{"name": "Date", "value": "Mon, 06 Jun 2022 10:00:00 +0000"},
			},
			"body": map[string]any{"data": ""},
			"parts": []map[string]any{
				{
					"mimeType": "text/html",
					"body":     map[string]any{"data": htmlEncoded},
				},
				{
					"mimeType": "text/plain",
					"body":     map[string]any{"data": plainEncoded},
				},
			},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/gmail/v1/users/me/messages", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]any{{"id": "multipart-001"}},
		})
	})
	mux.HandleFunc("/gmail/v1/users/me/messages/multipart-001", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(msg)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := newGmailCollectorWithFakeSource(validGmailConfig(), srv, nil)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d docs, want 1", len(docs))
	}
	if docs[0].Content != plainText {
		t.Errorf("expected plain text body, got %q", docs[0].Content)
	}
}
