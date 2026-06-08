// Package worker provides background goroutine workers for the second-brain
// service. Workers are long-running and must be started via [Run] with a
// context that controls their lifetime.
package worker

import (
	"context"
	"errors"
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
	// Refetcher is optional: when non-nil it is used to re-download remote
	// binaries (e.g. Discord/Slack attachments) before extraction.
	// When nil (or when the Refetcher returns ErrRefetchNotSupported), remote
	// paths are skipped with a debug log — identical to the pre-Refetcher
	// behaviour.
	Refetcher Refetcher
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
// Remote-source files (Slack attachments, Discord attachments, etc.) are
// handled as follows:
//   - When Config.Refetcher is nil or returns [ErrRefetchNotSupported], the
//     record is skipped with a debug log (same as pre-Refetcher behaviour).
//   - When Config.Refetcher successfully downloads the binary, extraction runs
//     on the temp file; success resolves the record, failure increments attempts.
type ExtractionRetryWorker struct {
	failureStore FailureStore
	docStore     DocStore
	extractor    Extractor
	refetcher    Refetcher // optional; nil disables remote-file retry
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
		refetcher:    cfg.Refetcher, // nil is valid; remote retry is disabled
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
	if !looksLikeLocalPath(f.FilePath) {
		// Remote-source path: attempt a re-download when a Refetcher is available.
		w.processRemote(ctx, f)
		return
	}

	w.extractAndResolve(ctx, f, f.FilePath, nil)
}

// processRemote handles a failure record whose FilePath is a remote URL or
// opaque identifier (not a local absolute path).
//
// When w.refetcher is nil or returns ErrRefetchNotSupported the record is
// skipped with a debug log — identical to pre-Refetcher behaviour so that
// callers without a Refetcher see no regression.
func (w *ExtractionRetryWorker) processRemote(ctx context.Context, f store.ExtractionFailure) {
	if w.refetcher == nil {
		slog.Debug("extraction retry: skipping non-local path (no refetcher configured)",
			"source_type", f.SourceType,
			"source_id", f.SourceID,
		)
		return
	}

	result, err := w.refetcher.Refetch(ctx, f)
	if err != nil {
		if errors.Is(err, ErrRefetchNotSupported) {
			slog.Debug("extraction retry: skipping non-local path (refetch not supported)",
				"source_type", f.SourceType,
				"source_id", f.SourceID,
			)
			return
		}
		// Refetch error (e.g. network failure, expired URL): increment attempts.
		w.recordFailure(ctx, f, err)
		slog.Warn("extraction retry: refetch failed",
			"source_type", f.SourceType,
			"source_id", f.SourceID,
			"attempt", f.Attempts+1,
			"err", err,
		)
		return
	}

	w.extractAndResolve(ctx, f, result.LocalPath, result.Cleanup)
}

// extractAndResolve runs extraction on localPath, upserts the document on
// success, and resolves the failure record. On extraction failure it increments
// the attempt counter. cleanup is called (if non-nil) after extraction
// regardless of outcome (always defer-called so temp files are removed).
func (w *ExtractionRetryWorker) extractAndResolve(
	ctx context.Context,
	f store.ExtractionFailure,
	localPath string,
	cleanup func(),
) {
	if cleanup != nil {
		defer cleanup()
	}

	content, err := w.extractor.ExtractFromPath(ctx, localPath)
	if err != nil {
		w.recordFailure(ctx, f, err)
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
		Title:       filepath.Base(urlPath(f.FilePath)),
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

// recordFailure increments the attempt counter for f using the provided error
// message. Errors from the store are logged and suppressed so that a store
// hiccup does not mask the original extraction failure.
func (w *ExtractionRetryWorker) recordFailure(ctx context.Context, f store.ExtractionFailure, cause error) {
	recordErr := w.failureStore.Record(ctx, store.ExtractionFailure{
		SourceType:   f.SourceType,
		SourceID:     f.SourceID,
		FilePath:     f.FilePath,
		ErrorMessage: cause.Error(),
		// Pass the current attempt count so that Record() can compute the
		// correct exponential back-off delay via durable.ExtractionBackoff.
		// f.Attempts was read from the DB by DueForRetry; the ON CONFLICT
		// branch in Record() uses this value to schedule the next retry.
		Attempts: f.Attempts,
	})
	if recordErr != nil {
		slog.Warn("extraction retry: failed to record attempt",
			"source_type", f.SourceType,
			"source_id", f.SourceID,
			"err", recordErr,
		)
	}
}

// looksLikeLocalPath returns true when p is an absolute POSIX path that does
// not start with "//" (which would indicate a UNC or network path on some
// systems).
func looksLikeLocalPath(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//")
}
