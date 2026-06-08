package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
)

// EntityDocumentLister is the read/write side of the document store required
// by EntityWorker.
type EntityDocumentLister interface {
	// ListWithoutEntities returns up to limit active documents whose
	// entities_processed_at column is NULL. Documents are ordered by
	// collected_at ASC so backfill progresses forward in time.
	ListWithoutEntities(ctx context.Context, limit int) ([]*model.Document, error)

	// MarkEntitiesProcessed sets entities_processed_at to now() for the
	// given document so it is not re-queued by ListWithoutEntities on the
	// next tick (issue #86).
	MarkEntitiesProcessed(ctx context.Context, documentID uuid.UUID) error
}

// EntityLinker is the write side of the entity store required by EntityWorker.
type EntityLinker interface {
	// UpsertAndLinkEntities persists entities and links them to the document.
	UpsertAndLinkEntities(ctx context.Context, documentID uuid.UUID, entities []model.Entity) error
}

// EntityWorkerConfig holds configuration for EntityWorker.
type EntityWorkerConfig struct {
	// Store provides both document listing and marking-processed.
	// Typically *store.DocumentStore satisfies this interface.
	Store EntityDocumentLister
	// Entities is the entity store (write side).
	Entities EntityLinker
	// LLM is the completion client used for extraction.
	LLM llm.Completer
	// Interval controls how often the worker polls. Defaults to 10 minutes.
	Interval time.Duration
	// BatchSize is the number of documents processed per tick. Defaults to 5.
	BatchSize int
}

// EntityWorker is a background worker that extracts named entities from
// documents that have not yet been processed and persists them via EntityStore.
//
// Extraction is BEST-EFFORT: LLM failures are logged as warnings and the
// document is left eligible for retry on the next tick. For documents where
// extraction succeeds (even with zero entities) or where entity linking fails,
// MarkEntitiesProcessed is called so the document is not re-queued indefinitely
// (issue #86).
type EntityWorker struct {
	store     EntityDocumentLister
	entities  EntityLinker
	llm       llm.Completer
	interval  time.Duration
	batchSize int

	// extractFn is the entity-extraction function. It defaults to
	// ExtractEntities and can be replaced in tests to avoid live LLM calls.
	extractFn func(ctx context.Context, c llm.Completer, doc *model.Document) ([]model.Entity, error)
}

// NewEntityWorker constructs an EntityWorker from cfg.
// Panics when required fields are nil (programming error).
func NewEntityWorker(cfg EntityWorkerConfig) *EntityWorker {
	if cfg.Store == nil {
		panic("EntityWorkerConfig.Store must not be nil")
	}
	if cfg.Entities == nil {
		panic("EntityWorkerConfig.Entities must not be nil")
	}
	if cfg.LLM == nil {
		panic("EntityWorkerConfig.LLM must not be nil")
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 5
	}
	return &EntityWorker{
		store:     cfg.Store,
		entities:  cfg.Entities,
		llm:       cfg.LLM,
		interval:  interval,
		batchSize: batchSize,
		extractFn: ExtractEntities,
	}
}

// Run starts the entity extraction loop. It blocks until ctx is cancelled.
// When the LLM is not configured a single diagnostic log is emitted and the
// loop idles without processing any documents.
func (w *EntityWorker) Run(ctx context.Context) {
	if !w.llm.Enabled() {
		slog.Info("entity worker disabled — LLM not configured (set LLM_API_KEY)")
		<-ctx.Done()
		return
	}

	slog.Info("entity worker started", "interval", w.interval, "batch_size", w.batchSize)
	w.tick(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("entity worker stopped")
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

// tick processes one batch of documents that are missing entity annotations.
func (w *EntityWorker) tick(ctx context.Context) {
	docs, err := w.store.ListWithoutEntities(ctx, w.batchSize)
	if err != nil {
		slog.Warn("entity worker: list documents failed", "error", err)
		return
	}
	if len(docs) == 0 {
		return
	}

	slog.Info("entity worker: processing batch", "count", len(docs))
	succeeded := 0
	for _, doc := range docs {
		entities, err := w.extractFn(ctx, w.llm, doc)
		if err != nil {
			// LLM failure is considered transient; leave entities_processed_at
			// NULL so the document is retried on the next tick.
			slog.Warn("entity worker: extraction failed",
				"doc_id", doc.ID, "source_id", doc.SourceID, "error", err)
			continue
		}

		if len(entities) == 0 {
			// No entities found — valid result (e.g. binary or very short doc).
			// Mark processed to prevent re-queuing on every tick (issue #86).
			w.markProcessed(ctx, doc.ID)
			succeeded++
			continue
		}

		if linkErr := w.entities.UpsertAndLinkEntities(ctx, doc.ID, entities); linkErr != nil {
			// Link failure may be transient, but to avoid infinite re-queuing we
			// still mark the document processed. The entities can be re-extracted
			// via a manual trigger if needed.
			slog.Warn("entity worker: link entities failed",
				"doc_id", doc.ID, "count", len(entities), "error", linkErr)
			w.markProcessed(ctx, doc.ID)
			continue
		}

		w.markProcessed(ctx, doc.ID)
		succeeded++
		slog.Debug("entity worker: entities linked",
			"doc_id", doc.ID, "count", len(entities))
	}

	slog.Info("entity worker: batch complete",
		"processed", len(docs), "succeeded", succeeded)
}

// markProcessed calls MarkEntitiesProcessed and logs a warning on failure.
// It is a convenience wrapper to keep tick() readable.
func (w *EntityWorker) markProcessed(ctx context.Context, documentID uuid.UUID) {
	if err := w.store.MarkEntitiesProcessed(ctx, documentID); err != nil {
		slog.Warn("entity worker: mark processed failed",
			"doc_id", documentID, "error", err)
	}
}
