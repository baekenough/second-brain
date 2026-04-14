package collector

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"regexp"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

// driveFileIDRegexp matches a Google file ID embedded in a Docs/Sheets/Slides URL.
// Example: https://docs.google.com/document/d/1BxiMVs0XRA5nFMdKvBdBZjgmUUqptlbs74OgVE2upms/edit
var driveFileIDRegexp = regexp.MustCompile(`https://docs\.google\.com/[^/]+/d/([a-zA-Z0-9_-]+)`)

// DriveExporter exports text content from Google Workspace stub files via the
// Drive API using Application Default Credentials (ADC). When ADC is not
// available, Enabled() returns false and all export calls are no-ops.
type DriveExporter struct {
	svc *drive.Service
}

// NewDriveExporter creates a DriveExporter backed by ADC. It returns nil (not
// an error) when credentials are unavailable so callers can treat it as optional.
func NewDriveExporter(ctx context.Context) (*DriveExporter, error) {
	creds, err := google.FindDefaultCredentials(ctx, drive.DriveReadonlyScope)
	if err != nil {
		// ADC not configured — exporter is disabled. Not an error.
		slog.Info("gdrive_export: ADC not configured, Drive export disabled", "reason", err)
		return nil, nil //nolint:nilerr
	}

	svc, err := drive.NewService(ctx, option.WithCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("gdrive_export: create drive service: %w", err)
	}

	slog.Info("gdrive_export: Drive API export enabled via ADC")
	return &DriveExporter{svc: svc}, nil
}

// Enabled reports whether the exporter has valid credentials.
func (e *DriveExporter) Enabled() bool {
	return e != nil && e.svc != nil
}

// Export fetches the text content of the Google Workspace file identified by
// fileID. The MIME type used for export is chosen based on ext:
//   - .gdoc, .gscript, .gform → text/plain
//   - .gsheet                 → text/csv
//   - .gslides                → text/plain (falls back to raw export on error)
func (e *DriveExporter) Export(ctx context.Context, fileID, ext string) (string, error) {
	if !e.Enabled() {
		return "", fmt.Errorf("gdrive_export: exporter not enabled")
	}

	mimeType := exportMIME(ext)
	body, err := e.exportAs(ctx, fileID, mimeType)
	if err != nil && ext == ".gslides" {
		// Slides may not support text/plain — try without a mime type override.
		body, err = e.exportAs(ctx, fileID, "text/plain")
	}
	if err != nil {
		return "", err
	}
	return body, nil
}

// ExtractFileID parses a Google Workspace stub file's URL and returns the
// embedded Drive file ID. Returns "" when the URL cannot be parsed.
func ExtractFileID(stubContent []byte) string {
	m := driveFileIDRegexp.FindSubmatch(stubContent)
	if len(m) < 2 {
		return ""
	}
	return string(m[1])
}

// exportAs calls the Drive files.export endpoint and returns the response body.
func (e *DriveExporter) exportAs(ctx context.Context, fileID, mimeType string) (string, error) {
	resp, err := e.svc.Files.Export(fileID, mimeType).Context(ctx).Download()
	if err != nil {
		return "", fmt.Errorf("gdrive_export: export %q as %q: %w", fileID, mimeType, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("gdrive_export: export %q: status %d", fileID, resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("gdrive_export: read export body for %q: %w", fileID, err)
	}
	return string(data), nil
}

// exportMIME returns the appropriate export MIME type for a workspace extension.
func exportMIME(ext string) string {
	switch ext {
	case ".gsheet":
		return "text/csv"
	default:
		// .gdoc, .gscript, .gform, .gslides
		return "text/plain"
	}
}
