// Package collector defines the Collector interface and common types used by
// all source-specific collectors.
package collector

import (
	"context"
	"time"

	"github.com/baekenough/second-brain/internal/model"
)

// Collector collects documents from an external source.
type Collector interface {
	// Name returns a human-readable identifier (e.g. "slack", "github").
	Name() string

	// Source returns the SourceType that this collector produces.
	Source() model.SourceType

	// Enabled reports whether this collector is configured and should run.
	Enabled() bool

	// Collect fetches documents created or updated since the given time.
	Collect(ctx context.Context, since time.Time) ([]model.Document, error)
}

// DeletionDetector is an optional interface that collectors may implement to
// support soft-delete detection. Collectors that can enumerate all currently
// existing source IDs (regardless of modification time) should implement this.
type DeletionDetector interface {
	// ListActiveSourceIDs returns all source IDs that currently exist in the
	// source. The scheduler uses this to detect files removed since last run.
	ListActiveSourceIDs(ctx context.Context) ([]string, error)
}
