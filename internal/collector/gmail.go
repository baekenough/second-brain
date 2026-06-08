package collector

// TODO(#gmail): implement Gmail collector using the Gmail REST API.
// This stub satisfies the collector.Collector interface so that the binary
// compiles and the scheduler can be registered without a real implementation.
//
// Implementation notes:
//   - Use GmailCredentialsJSON + GmailTokenJSON (OAuth2) from config.Config.
//   - Query all threads matching cfg.GmailQuery since the watermark (since).
//   - Convert each thread to a model.Document with SourceType=SourceGmail.
//   - SourceID format: "gmail:<thread-id>"
//   - OccurredAt: use the internalDate of the first/latest message in the thread.
//   - Consider implementing StreamingCollector for large mailboxes.

import (
	"context"
	"time"

	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/model"
)

// GmailCollector collects emails from a Gmail account via the Gmail REST API.
// It is disabled when credentials or token are not configured.
type GmailCollector struct {
	cfg *config.Config
}

// NewGmailCollector returns a GmailCollector configured from cfg.
// When GmailCredentialsJSON or GmailTokenJSON is empty, Enabled() returns false
// and the scheduler will not call Collect.
func NewGmailCollector(cfg *config.Config) *GmailCollector {
	return &GmailCollector{cfg: cfg}
}

func (c *GmailCollector) Name() string             { return "gmail" }
func (c *GmailCollector) Source() model.SourceType { return model.SourceGmail }
func (c *GmailCollector) Enabled() bool {
	return c.cfg.GmailCredentialsJSON != "" && c.cfg.GmailTokenJSON != ""
}

// Collect is not yet implemented.
// TODO(#gmail): implement incremental Gmail thread collection.
func (c *GmailCollector) Collect(_ context.Context, _ time.Time) ([]model.Document, error) {
	return nil, nil
}
