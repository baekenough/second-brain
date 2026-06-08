package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
)

// EntityDocumentLister is the read side of the document store required by
// EntityWorker. It returns documents that have not yet had entity extraction run.
type EntityDocumentLister interface {
	// ListWithoutEntities returns up to limit active documents that have no
	// entries in document_entities. Documents are ordered by collected_at ASC
	// so backfill progresses forward in time.
	ListWithoutEntities(ctx context.Context, limit int) ([]*model.Document, error)
}

// EntityLinker is the write side of the entity store required by EntityWorker.
type EntityLinker interface {
	// UpsertAndLinkEntities persists entities and links them to the document.
	UpsertAndLinkEntities(ctx context.Context, documentID uuid.UUID, entities []model.Entity) error
}

// EntityWorkerConfig holds configuration for EntityWorker.
type EntityWorkerConfig struct {
	// Store provides both document listing and entity linking.
	// Typically a *store.DocumentEntityStore (see adapters.go).
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
// Extraction is BEST-EFFORT: failures are logged as warnings and the document
// is not re-queued (it will remain eligible for extraction on the next tick
// because ListWithoutEntities only excludes documents that already have at
// least one entity linked).
type EntityWorker struct {
	store     EntityDocumentLister
	entities  EntityLinker
	llm       llm.Completer
	interval  time.Duration
	batchSize int
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
		entities, err := ExtractEntities(ctx, w.llm, doc)
		if err != nil {
			slog.Warn("entity worker: extraction failed",
				"doc_id", doc.ID, "source_id", doc.SourceID, "error", err)
			// Insert a sentinel empty-link so that this document is not re-queued
			// indefinitely when the LLM consistently fails on it.
			// We skip this and rely on the caller to mark it processed externally.
			// For the MVP, the document simply remains in the queue and will be
			// retried on the next tick — this is acceptable for best-effort extraction.
			continue
		}
		if len(entities) == 0 {
			// No entities found — still a valid result; document will not be
			// re-processed unless the table has a mechanism for it. For the MVP
			// we accept that documents with zero entities will be re-queued each
			// tick. Callers can add a processed_at column in a follow-up.
			succeeded++
			continue
		}
		if linkErr := w.entities.UpsertAndLinkEntities(ctx, doc.ID, entities); linkErr != nil {
			slog.Warn("entity worker: link entities failed",
				"doc_id", doc.ID, "count", len(entities), "error", linkErr)
			continue
		}
		succeeded++
		slog.Debug("entity worker: entities linked",
			"doc_id", doc.ID, "count", len(entities))
	}

	slog.Info("entity worker: batch complete",
		"processed", len(docs), "succeeded", succeeded)
}
