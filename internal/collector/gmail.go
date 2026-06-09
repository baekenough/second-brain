package collector

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/model"
)

const (
	gmailScope   = "https://www.googleapis.com/auth/gmail.readonly"
	gmailBaseURL = "https://gmail.googleapis.com"
)

// GmailCollector collects emails from a Gmail account via the Gmail REST API.
// It is disabled when credentials or token are not configured.
type GmailCollector struct {
	cfg *config.Config

	// httpClient and baseURL are overridable for testing.
	httpClient *http.Client
	baseURL    string

	// maxMessages caps the total IDs fetched per Collect call (0 = unlimited).
	// Sourced from cfg.GmailMaxMessages; stored here so tests can override easily.
	maxMessages int

	// tokenMu guards tokenSource and cachedToken.
	tokenMu     sync.Mutex
	tokenSource oauth2.TokenSource
	cachedToken *oauth2.Token
}

// NewGmailCollector returns a GmailCollector configured from cfg.
// When GmailCredentialsJSON or GmailTokenJSON is empty, Enabled() returns false
// and the scheduler will not call Collect.
func NewGmailCollector(cfg *config.Config) *GmailCollector {
	return &GmailCollector{
		cfg:         cfg,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
		baseURL:     gmailBaseURL,
		maxMessages: cfg.GmailMaxMessages,
	}
}

func (c *GmailCollector) Name() string             { return "gmail" }
func (c *GmailCollector) Source() model.SourceType { return model.SourceGmail }
func (c *GmailCollector) Enabled() bool {
	return c.cfg.GmailCredentialsJSON != "" && c.cfg.GmailTokenJSON != ""
}

// Collect fetches Gmail messages matching cfg.GmailQuery that were received
// after since. It returns one Document per message (thread-level dedup is left
// to the store's Upsert logic via SourceID).
func (c *GmailCollector) Collect(ctx context.Context, since time.Time) ([]model.Document, error) {
	token, err := c.getAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("gmail: get access token: %w", err)
	}

	ids, err := c.listMessageIDs(ctx, token, since)
	if err != nil {
		return nil, fmt.Errorf("gmail: list message IDs: %w", err)
	}

	var docs []model.Document
	for _, id := range ids {
		doc, err := c.fetchMessage(ctx, token, id)
		if err != nil {
			slog.Warn("gmail: failed to fetch message", "id", id, "error", err)
			continue
		}
		docs = append(docs, doc)
	}

	slog.Info("gmail: collected documents", "count", len(docs))
	return docs, nil
}

// --- Gmail API helpers ---

// gmailMessage is the minimal shape of a Gmail API message resource.
type gmailMessage struct {
	ID           string              `json:"id"`
	ThreadID     string              `json:"threadId"`
	LabelIDs     []string            `json:"labelIds"`
	InternalDate string              `json:"internalDate"` // millis since epoch (string)
	Payload      *gmailMessagePart   `json:"payload"`
}

type gmailMessagePart struct {
	MimeType string              `json:"mimeType"`
	Headers  []gmailHeader       `json:"headers"`
	Body     gmailPartBody       `json:"body"`
	Parts    []gmailMessagePart  `json:"parts"`
}

type gmailHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type gmailPartBody struct {
	Data string `json:"data"` // base64url-encoded content
}

// listMessageIDs retrieves message IDs matching the configured query,
// applying an "after:<unix seconds>" filter when since is non-zero.
func (c *GmailCollector) listMessageIDs(ctx context.Context, token string, since time.Time) ([]string, error) {
	q := c.cfg.GmailQuery
	if !since.IsZero() && since.Unix() > 0 {
		q = strings.TrimSpace(q) + fmt.Sprintf(" after:%d", since.Unix())
	}

	var ids []string
	pageToken := ""

	for {
		if c.maxMessages > 0 && len(ids) >= c.maxMessages {
			slog.Warn("gmail: reached message cap, truncating results", "cap", c.maxMessages)
			break
		}

		params := url.Values{
			"q":          {q},
			"maxResults": {"500"},
		}
		if pageToken != "" {
			params.Set("pageToken", pageToken)
		}

		u := c.baseURL + "/gmail/v1/users/me/messages?" + params.Encode()
		var resp struct {
			Messages          []struct{ ID string `json:"id"` } `json:"messages"`
			NextPageToken     string                             `json:"nextPageToken"`
		}
		if err := c.doRequest(ctx, token, u, &resp); err != nil {
			return nil, err
		}

		for _, m := range resp.Messages {
			ids = append(ids, m.ID)
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}

	return ids, nil
}

// fetchMessage retrieves a single Gmail message and converts it to a Document.
func (c *GmailCollector) fetchMessage(ctx context.Context, token, id string) (model.Document, error) {
	u := fmt.Sprintf("%s/gmail/v1/users/me/messages/%s?format=full", c.baseURL, url.PathEscape(id))

	var msg gmailMessage
	if err := c.doRequest(ctx, token, u, &msg); err != nil {
		return model.Document{}, err
	}

	// MEDIUM: nil Payload guard — the Gmail API can return messages without
	// a payload (e.g. draft stubs, deleted messages returned by accident).
	// Dereferencing a nil pointer would panic; skip with a warning instead.
	if msg.Payload == nil {
		slog.Warn("gmail: message has nil payload, skipping", "id", id)
		return model.Document{}, fmt.Errorf("gmail: message %q has nil payload", id)
	}

	subject := gmailHeaders(msg.Payload.Headers).get("Subject")
	from := gmailHeaders(msg.Payload.Headers).get("From")
	to := gmailHeaders(msg.Payload.Headers).get("To")
	dateStr := gmailHeaders(msg.Payload.Headers).get("Date")

	bodyText := extractGmailBody(msg.Payload)

	var occurredAt *time.Time
	if t, err := mail.ParseDate(dateStr); err == nil {
		t = t.UTC()
		occurredAt = &t
	}

	now := time.Now().UTC()
	return model.Document{
		ID:         uuid.New(),
		SourceType: model.SourceGmail,
		SourceID:   "gmail:" + msg.ID,
		Title:      subject,
		Content:    bodyText,
		Metadata: map[string]any{
			"thread_id": msg.ThreadID,
			"from":      from,
			"to":        to,
			"label_ids": msg.LabelIDs,
		},
		OccurredAt:  occurredAt,
		CollectedAt: now,
	}, nil
}

// gmailHeaders is a named type so we can attach the helper get method.
type gmailHeaders []gmailHeader

func (hs gmailHeaders) get(name string) string {
	nameLower := strings.ToLower(name)
	for _, h := range hs {
		if strings.ToLower(h.Name) == nameLower {
			return h.Value
		}
	}
	return ""
}

// extractGmailBody walks the MIME part tree and returns the best-effort plain
// text body. It prefers text/plain over text/html; for HTML it strips tags.
func extractGmailBody(part *gmailMessagePart) string {
	if part == nil {
		return ""
	}

	// Prefer text/plain.
	if strings.EqualFold(part.MimeType, "text/plain") && part.Body.Data != "" {
		return decodeBase64URL(part.Body.Data)
	}

	// Recurse into multipart.
	if strings.HasPrefix(strings.ToLower(part.MimeType), "multipart/") {
		// Two-pass: first look for text/plain recursively.
		for i := range part.Parts {
			if text := extractGmailBodyPreferPlain(&part.Parts[i]); text != "" {
				return text
			}
		}
	}

	// Fall back to HTML body (strip tags).
	if strings.EqualFold(part.MimeType, "text/html") && part.Body.Data != "" {
		return stripHTMLTags(decodeBase64URL(part.Body.Data))
	}

	return ""
}

// extractGmailBodyPreferPlain recursively searches for a text/plain part.
func extractGmailBodyPreferPlain(part *gmailMessagePart) string {
	if part == nil {
		return ""
	}
	if strings.EqualFold(part.MimeType, "text/plain") && part.Body.Data != "" {
		return decodeBase64URL(part.Body.Data)
	}
	if strings.HasPrefix(strings.ToLower(part.MimeType), "multipart/") {
		for i := range part.Parts {
			if text := extractGmailBodyPreferPlain(&part.Parts[i]); text != "" {
				return text
			}
		}
	}
	return ""
}

// decodeBase64URL decodes a base64url-encoded string (Gmail uses URL-safe base64).
func decodeBase64URL(s string) string {
	b, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		// Gmail may omit padding; try RawURLEncoding as fallback.
		b, err = base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			return ""
		}
	}
	return string(b)
}

// stripHTMLTags removes HTML tags from s using a simple state machine.
// For production-quality HTML parsing golang.org/x/net/html is recommended,
// but importing it solely for tag stripping adds an unnecessary dependency
// since x/net is already available in go.mod.
func stripHTMLTags(s string) string {
	var sb strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// --- OAuth2 token management ---

// gmailCredentials is the shape of a Google OAuth2 installed-app credentials.json.
type gmailCredentials struct {
	Installed struct {
		ClientID     string   `json:"client_id"`
		ClientSecret string   `json:"client_secret"`
		RedirectURIs []string `json:"redirect_uris"`
		AuthURI      string   `json:"auth_uri"`
		TokenURI     string   `json:"token_uri"`
	} `json:"installed"`
	Web struct {
		ClientID     string   `json:"client_id"`
		ClientSecret string   `json:"client_secret"`
		RedirectURIs []string `json:"redirect_uris"`
		AuthURI      string   `json:"auth_uri"`
		TokenURI     string   `json:"token_uri"`
	} `json:"web"`
}

// gmailTokenFile is the shape of a token.json saved by the OAuth2 flow.
type gmailTokenFile struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	Expiry       time.Time `json:"expiry"`
	TokenURI     string    `json:"token_uri"`
	ClientID     string    `json:"client_id"`
	ClientSecret string    `json:"client_secret"`
}

// getAccessToken returns a valid OAuth2 Bearer token, refreshing if necessary.
func (c *GmailCollector) getAccessToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	if c.cachedToken != nil && c.cachedToken.Valid() {
		return c.cachedToken.AccessToken, nil
	}

	if c.tokenSource == nil {
		ts, err := c.buildTokenSource(context.Background())
		if err != nil {
			return "", err
		}
		c.tokenSource = ts
	}

	tok, err := c.tokenSource.Token()
	if err != nil {
		if strings.Contains(err.Error(), "invalid_grant") {
			slog.Error("gmail: OAuth2 refresh token is invalid or revoked — re-authentication required",
				"credentials_path", c.cfg.GmailCredentialsJSON,
				"token_path", c.cfg.GmailTokenJSON,
			)
		}
		return "", fmt.Errorf("gmail: fetch access token: %w", err)
	}
	c.cachedToken = tok
	return tok.AccessToken, nil
}

// buildTokenSource constructs an oauth2.TokenSource from the credentials and
// token files specified in the config. Both fields are treated as file paths.
func (c *GmailCollector) buildTokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	credJSON, err := os.ReadFile(c.cfg.GmailCredentialsJSON)
	if err != nil {
		return nil, fmt.Errorf("gmail: read credentials file %q: %w", c.cfg.GmailCredentialsJSON, err)
	}

	tokenJSON, err := os.ReadFile(c.cfg.GmailTokenJSON)
	if err != nil {
		return nil, fmt.Errorf("gmail: read token file %q: %w", c.cfg.GmailTokenJSON, err)
	}

	// Parse credentials to extract client_id / client_secret / token_uri.
	var creds gmailCredentials
	if err := json.Unmarshal(credJSON, &creds); err != nil {
		return nil, fmt.Errorf("gmail: parse credentials JSON: %w", err)
	}

	// Support both "installed" and "web" credential types.
	clientID := creds.Installed.ClientID
	clientSecret := creds.Installed.ClientSecret
	tokenURI := creds.Installed.TokenURI
	if clientID == "" {
		clientID = creds.Web.ClientID
		clientSecret = creds.Web.ClientSecret
		tokenURI = creds.Web.TokenURI
	}
	if clientID == "" {
		return nil, fmt.Errorf("gmail: credentials JSON has neither 'installed' nor 'web' client_id")
	}
	if tokenURI == "" {
		tokenURI = "https://oauth2.googleapis.com/token"
	}

	// Parse the saved token.
	var tok gmailTokenFile
	if err := json.Unmarshal(tokenJSON, &tok); err != nil {
		return nil, fmt.Errorf("gmail: parse token JSON: %w", err)
	}

	oauthCfg := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       []string{gmailScope},
	}
	if tokenURI != "" {
		oauthCfg.Endpoint.TokenURL = tokenURI
	}

	oauthToken := &oauth2.Token{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		TokenType:    tok.TokenType,
		Expiry:       tok.Expiry,
	}

	// oauth2.ReuseTokenSource wraps the StaticTokenSource so it auto-refreshes
	// when the access token expires, using the refresh token.
	ts := oauthCfg.TokenSource(ctx, oauthToken)
	return oauth2.ReuseTokenSource(oauthToken, ts), nil
}

// doRequest performs a GET request to url, attaches the Bearer token, reads the
// response body, and JSON-decodes it into dest.
func (c *GmailCollector) doRequest(ctx context.Context, token, u string, dest interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	res, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode >= 400 {
		b, _ := io.ReadAll(res.Body)
		return fmt.Errorf("gmail API %s: status %d: %s", u, res.StatusCode, b)
	}

	b, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dest)
}
