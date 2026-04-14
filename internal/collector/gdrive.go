package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/model"
)

// GDriveCollector collects documents from Google Drive using a service account.
//
// It uses the Drive REST API v3 with a service-account access token obtained
// via the Google OAuth2 token endpoint (JWT grant). For a PoC this is kept
// simple — production use should use the official google.golang.org/api client.
type GDriveCollector struct {
	credentialsJSON string
	client          *http.Client
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

// getAccessToken exchanges the service account JSON credentials for a Bearer token.
// This is a simplified JWT flow; production code should use google.golang.org/api/google.
func (c *GDriveCollector) getAccessToken(ctx context.Context) (string, error) {
	var creds struct {
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
	}
	if err := json.Unmarshal([]byte(c.credentialsJSON), &creds); err != nil {
		return "", fmt.Errorf("parse credentials JSON: %w", err)
	}
	// NOTE: A production implementation would sign a JWT with creds.PrivateKey
	// and POST it to creds.TokenURI (https://oauth2.googleapis.com/token).
	// For PoC, callers should pre-set GOOGLE_APPLICATION_CREDENTIALS and use
	// the Application Default Credentials flow from google.golang.org/api.
	return "", fmt.Errorf("gdrive: full OAuth2 JWT flow not implemented in PoC — set GOOGLE_APPLICATION_CREDENTIALS and use ADC")
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
