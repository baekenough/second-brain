// Package worker provides background goroutine workers for the second-brain
// service. Workers are long-running and must be started via [Run] with a
// context that controls their lifetime.
package worker

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// Extractor re-extracts plain text from a file at the given absolute path.
// Implementations must not panic and must respect context cancellation.
//
// The concrete adapter ([registryExtractor]) bridges this interface to the
// [extractor.Registry] type in internal/collector/extractor.
type Extractor interface {
	ExtractFromPath(ctx context.Context, path string) (string, error)
}

// DocStore persists documents produced by successful re-extractions.
type DocStore interface {
	Upsert(ctx context.Context, doc *model.Document) error
}

// FailureStore manages extraction_failures rows.
type FailureStore interface {
	DueForRetry(ctx context.Context, limit int) ([]store.ExtractionFailure, error)
	Record(ctx context.Context, f store.ExtractionFailure) error
	Resolve(ctx context.Context, sourceType, sourceID string) error
}

// Config holds all configuration for [ExtractionRetryWorker].
// Zero values for Interval and BatchSize are replaced with safe defaults.
type Config struct {
	// FailureStore is required: the persistence layer for extraction failures.
	FailureStore FailureStore
	// DocStore is required: used to persist successfully re-extracted content.
	DocStore DocStore
	// Extractor is required: performs the actual text extraction from a file path.
	Extractor Extractor
	// Interval controls how often the worker polls for due failures.
	// Defaults to 1 minute when zero or negative.
	Interval time.Duration
	// BatchSize limits the number of failures processed per tick.
	// Defaults to 20 when zero or negative.
	BatchSize int
}

// ExtractionRetryWorker periodically reads due failures from the
// extraction_failures table and re-runs extraction for each entry.
//
// On success the failure record is deleted (via [FailureStore.Resolve]) and
// the extracted content is upserted into the document store.
//
// On failure the failure record's attempt counter is incremented (via
// [FailureStore.Record]), backing off exponentially until the row is
// dead-lettered at 10 attempts.
//
// Remote-source files (Slack attachments, Discord attachments, etc.) cannot be
// retried by this worker because the original binary is not retained locally.
// Those paths are skipped with a debug log.
//
// TODO(issue#8-followup): add remote-file retry via re-download from the
// originating collector (Slack/Discord) once a download cache or URL
// re-fetch mechanism is available.
type ExtractionRetryWorker struct {
	failureStore FailureStore
	docStore     DocStore
	extractor    Extractor
	interval     time.Duration
	batchSize    int
}

// New returns a configured [ExtractionRetryWorker]. It panics if any required
// dependency (FailureStore, DocStore, Extractor) is nil.
func New(cfg Config) *ExtractionRetryWorker {
	if cfg.FailureStore == nil {
		panic("worker.New: FailureStore is required")
	}
	if cfg.DocStore == nil {
		panic("worker.New: DocStore is required")
	}
	if cfg.Extractor == nil {
		panic("worker.New: Extractor is required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 20
	}
	return &ExtractionRetryWorker{
		failureStore: cfg.FailureStore,
		docStore:     cfg.DocStore,
		extractor:    cfg.Extractor,
		interval:     cfg.Interval,
		batchSize:    cfg.BatchSize,
	}
}

// Run blocks until ctx is cancelled, processing retry batches at each tick.
// An initial batch is processed immediately on entry so that short-lived test
// contexts see at least one attempt.
func (w *ExtractionRetryWorker) Run(ctx context.Context) {
	w.processBatch(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("extraction retry worker: stopped")
			return
		case <-ticker.C:
			w.processBatch(ctx)
		}
	}
}

// processBatch fetches up to batchSize due failures and attempts to re-extract
// each one. It returns without error; all errors are logged internally.
func (w *ExtractionRetryWorker) processBatch(ctx context.Context) {
	failures, err := w.failureStore.DueForRetry(ctx, w.batchSize)
	if err != nil {
		slog.Warn("extraction retry: DueForRetry failed", "err", err)
		return
	}
	if len(failures) == 0 {
		return
	}

	slog.Info("extraction retry: batch started", "size", len(failures))

	for _, f := range failures {
		if ctx.Err() != nil {
			return
		}
		w.processOne(ctx, f)
	}
}

// processOne attempts to re-extract a single failure record.
func (w *ExtractionRetryWorker) processOne(ctx context.Context, f store.ExtractionFailure) {
	// Remote-source files (Slack/Discord attachments) are stored as URLs or
	// opaque identifiers — the binary is not available locally. Skip them.
	if !looksLikeLocalPath(f.FilePath) {
		slog.Debug("extraction retry: skipping non-local path",
			"source_type", f.SourceType,
			"source_id", f.SourceID,
		)
		return
	}

	content, err := w.extractor.ExtractFromPath(ctx, f.FilePath)
	if err != nil {
		// Increment attempt counter; the store applies exponential back-off.
		recordErr := w.failureStore.Record(ctx, store.ExtractionFailure{
			SourceType:   f.SourceType,
			SourceID:     f.SourceID,
			FilePath:     f.FilePath,
			ErrorMessage: err.Error(),
		})
		if recordErr != nil {
			slog.Warn("extraction retry: failed to record attempt",
				"source_type", f.SourceType,
				"source_id", f.SourceID,
				"err", recordErr,
			)
		}
		slog.Warn("extraction retry: extraction failed",
			"source_type", f.SourceType,
			"source_id", f.SourceID,
			"attempt", f.Attempts+1,
		)
		return
	}

	doc := &model.Document{
		SourceType:  model.SourceType(f.SourceType),
		SourceID:    f.SourceID,
		Title:       filepath.Base(f.FilePath),
		Content:     content,
		CollectedAt: time.Now(),
	}
	if err := w.docStore.Upsert(ctx, doc); err != nil {
		slog.Warn("extraction retry: upsert failed",
			"source_type", f.SourceType,
			"source_id", f.SourceID,
			"err", err,
		)
		return
	}

	if err := w.failureStore.Resolve(ctx, f.SourceType, f.SourceID); err != nil {
		slog.Warn("extraction retry: resolve failed",
			"source_type", f.SourceType,
			"source_id", f.SourceID,
			"err", err,
		)
		return
	}

	slog.Info("extraction retry: resolved",
		"source_type", f.SourceType,
		"source_id", f.SourceID,
	)
}

// looksLikeLocalPath returns true when p is an absolute POSIX path that does
// not start with "//" (which would indicate a UNC or network path on some
// systems).
func looksLikeLocalPath(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//")
}
