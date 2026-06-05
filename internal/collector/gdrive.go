package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/baekenough/second-brain/internal/model"
)

const gDriveReadonlyScope = "https://www.googleapis.com/auth/drive.readonly"

// GDriveCollector collects documents from Google Drive using a service account.
//
// It uses the Drive REST API v3 with a service-account access token obtained
// via golang.org/x/oauth2/google. Two credential paths are supported:
//   - GDRIVE_CREDENTIALS_JSON: service account JSON inline (takes priority).
//   - ADC (GOOGLE_APPLICATION_CREDENTIALS or GCP metadata): Application Default Credentials.
type GDriveCollector struct {
	credentialsJSON string
	client          *http.Client

	// tokenMu guards cachedToken.
	tokenMu     sync.Mutex
	cachedToken *oauth2.Token
	tokenSource oauth2.TokenSource
}

// NewGDriveCollector returns a GDriveCollector. When credentialsJSON is empty
// the collector is disabled.
func NewGDriveCollector(credentialsJSON string) *GDriveCollector {
	return &GDriveCollector{
		credentialsJSON: credentialsJSON,
		client:          &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *GDriveCollector) Name() string             { return "gdrive" }
func (c *GDriveCollector) Source() model.SourceType { return model.SourceGDrive }
func (c *GDriveCollector) Enabled() bool            { return c.credentialsJSON != "" }

// Collect lists Google Docs modified after since and exports their text content.
func (c *GDriveCollector) Collect(ctx context.Context, since time.Time) ([]model.Document, error) {
	token, err := c.getAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("gdrive get access token: %w", err)
	}

	files, err := c.listFiles(ctx, token, since)
	if err != nil {
		return nil, fmt.Errorf("gdrive list files: %w", err)
	}

	var docs []model.Document
	for _, f := range files {
		text, err := c.exportText(ctx, token, f.ID)
		if err != nil {
			slog.Warn("gdrive: failed to export file", "id", f.ID, "name", f.Name, "error", err)
			continue
		}
		docs = append(docs, model.Document{
			ID:         uuid.New(),
			SourceType: model.SourceGDrive,
			SourceID:   f.ID,
			Title:      f.Name,
			Content:    text,
			Metadata: map[string]any{
				"mime_type":   f.MimeType,
				"web_link":    f.WebViewLink,
				"modified_at": f.ModifiedTime,
			},
			CollectedAt: time.Now().UTC(),
		})
	}

	slog.Info("gdrive: collected documents", "count", len(docs))
	return docs, nil
}

// --- Drive API helpers ---

type driveFile struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	MimeType     string `json:"mimeType"`
	WebViewLink  string `json:"webViewLink"`
	ModifiedTime string `json:"modifiedTime"`
}

func (c *GDriveCollector) listFiles(ctx context.Context, token string, since time.Time) ([]driveFile, error) {
	var all []driveFile
	pageToken := ""
	q := fmt.Sprintf(
		"mimeType='application/vnd.google-apps.document' and modifiedTime>'%s' and trashed=false",
		since.UTC().Format(time.RFC3339),
	)

	for {
		params := url.Values{
			"q":        {q},
			"fields":   {"nextPageToken,files(id,name,mimeType,webViewLink,modifiedTime)"},
			"pageSize": {"100"},
		}
		if pageToken != "" {
			params.Set("pageToken", pageToken)
		}

		var resp struct {
			NextPageToken string      `json:"nextPageToken"`
			Files         []driveFile `json:"files"`
		}
		if err := c.doRequest(ctx, token, "GET",
			"https://www.googleapis.com/drive/v3/files?"+params.Encode(),
			nil, &resp); err != nil {
			return nil, err
		}

		all = append(all, resp.Files...)
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return all, nil
}

func (c *GDriveCollector) exportText(ctx context.Context, token, fileID string) (string, error) {
	u := fmt.Sprintf(
		"https://www.googleapis.com/drive/v3/files/%s/export?mimeType=text/plain",
		url.PathEscape(fileID),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	res, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	if res.StatusCode >= 400 {
		return "", fmt.Errorf("export %s: status %d", fileID, res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	return string(body), err
}

// getAccessToken returns a valid OAuth2 Bearer token for the Drive API.
//
// Priority:
//  1. GDRIVE_CREDENTIALS_JSON (service account JSON) — parsed via google.CredentialsFromJSON.
//  2. ADC (GOOGLE_APPLICATION_CREDENTIALS env var or GCP metadata server).
//
// Tokens are cached and reused until they expire; a new token is fetched
// automatically when the cached one is within 10 seconds of expiry.
func (c *GDriveCollector) getAccessToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	// Reuse a cached token that is still valid (with 10 s buffer).
	if c.cachedToken != nil && c.cachedToken.Valid() {
		return c.cachedToken.AccessToken, nil
	}

	// Build a token source once; reuse it across calls.
	// Use context.Background() so the token source's lifetime is tied to the
	// collector instance, not to the first caller's (potentially short-lived)
	// request context. The JWT source captures the context it receives and
	// reuses it for every future token refresh — a cancelled request context
	// would silently break all subsequent refreshes.
	if c.tokenSource == nil {
		ts, err := c.buildTokenSource(context.Background())
		if err != nil {
			return "", err
		}
		c.tokenSource = ts
	}

	tok, err := c.tokenSource.Token()
	if err != nil {
		return "", fmt.Errorf("gdrive: fetch access token: %w", err)
	}
	c.cachedToken = tok
	return tok.AccessToken, nil
}

// buildTokenSource constructs an oauth2.TokenSource from the available credentials.
// It is called at most once per GDriveCollector instance.
func (c *GDriveCollector) buildTokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	if c.credentialsJSON != "" {
		creds, err := google.CredentialsFromJSON(
			ctx,
			[]byte(c.credentialsJSON),
			gDriveReadonlyScope,
		)
		if err != nil {
			return nil, fmt.Errorf("gdrive: parse service account credentials JSON: %w", err)
		}
		slog.Info("gdrive: using service account credentials from GDRIVE_CREDENTIALS_JSON")
		return creds.TokenSource, nil
	}

	// Fall back to Application Default Credentials.
	creds, err := google.FindDefaultCredentials(ctx, gDriveReadonlyScope)
	if err != nil {
		return nil, fmt.Errorf("gdrive: no credentials available (set GDRIVE_CREDENTIALS_JSON or GOOGLE_APPLICATION_CREDENTIALS): %w", err)
	}
	slog.Info("gdrive: using Application Default Credentials")
	return creds.TokenSource, nil
}

func (c *GDriveCollector) doRequest(ctx context.Context, token, method, u string, body io.Reader, dest interface{}) error {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	res, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode >= 400 {
		b, _ := io.ReadAll(res.Body)
		return fmt.Errorf("gdrive API %s: status %d: %s", u, res.StatusCode, b)
	}

	b, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dest)
}
