// Package scheduler periodically triggers collectors and persists the
// resulting documents.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
	"github.com/baekenough/second-brain/internal/chunker"
	"github.com/baekenough/second-brain/internal/collector"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/baekenough/second-brain/internal/worker"
)

// DocumentUpserter is the subset of the document store used by the scheduler.
type DocumentUpserter interface {
	Upsert(ctx context.Context, doc *model.Document) error
	LastCollectedAt(ctx context.Context, instanceID string, src model.SourceType, fallback time.Time) time.Time
	UpdateCollectorState(ctx context.Context, instanceID string, src model.SourceType, lastCollectedAt time.Time) error
	RecordCollectionLog(ctx context.Context, src model.SourceType, started time.Time, count int, err error) error
	MarkDeleted(ctx context.Context, sourceType model.SourceType, activeIDs []string) (int, error)
	// ListUnembedded returns up to limit active documents with a NULL embedding.
	ListUnembedded(ctx context.Context, limit int) ([]*model.Document, error)
	// UpdateEmbedding persists the embedding vector for a single document.
	UpdateEmbedding(ctx context.Context, doc *model.Document) error
	// ActiveSourceIDSet returns the set of source_ids currently active in the
	// store for the given source type. Used by the filesystem collector to detect
	// files that are new (not yet indexed) regardless of their mtime.
	ActiveSourceIDSet(ctx context.Context, sourceType model.SourceType) (map[string]struct{}, error)
}

// ActiveDocumentCounter is an optional extension of DocumentUpserter. When the
// store implements it, the scheduler uses it to perform a deletion-ratio
// sanity check before calling MarkDeleted. If the ratio of documents that
// would be deleted exceeds deletionRatioThreshold the deletion is skipped and
// a warning is logged — this guards against bulk data-loss caused by a
// partially unmounted or glitching filesystem.
type ActiveDocumentCounter interface {
	CountActiveDocuments(ctx context.Context, sourceType model.SourceType) (int, error)
}

// deletionRatioThreshold is the maximum fraction of currently active documents
// that may be deleted in a single MarkDeleted pass. Deletions exceeding this
// fraction are considered suspicious (e.g. partial unmount) and are skipped
// with a warning log. The value 0.50 means "block if more than 50% would be
// deleted". Tests and callers must not hardcode this constant.
const deletionRatioThreshold = 0.50

// deletionRatioWouldExceed reports whether deleting all DB-active documents
// that are absent from the filesystem walk (activeInDB - activeOnFS) would
// exceed deletionRatioThreshold.
//
// When activeInDB is zero (fresh source, no prior documents), the ratio is
// zero by definition and the function always returns false so that the very
// first sync can proceed normally.
func deletionRatioWouldExceed(activeInDB, activeOnFS int) bool {
	if activeInDB <= 0 {
		return false
	}
	wouldDelete := activeInDB - activeOnFS
	if wouldDelete <= 0 {
		return false
	}
	ratio := float64(wouldDelete) / float64(activeInDB)
	return ratio > deletionRatioThreshold
}

// EntityExtractor is the subset of the entity store used by the scheduler to
// trigger best-effort entity extraction immediately after document ingestion.
// It is satisfied by *store.EntityStore.
type EntityExtractor interface {
	UpsertAndLinkEntities(ctx context.Context, documentID uuid.UUID, entities []model.Entity) error
}

// Scheduler wraps robfig/cron and manages periodic collection runs.
type Scheduler struct {
	cron        *cron.Cron
	collectors  []collector.Collector
	store       DocumentUpserter
	embed       search.EmbeddingEngine
	chunkStore  *store.ChunkStore  // nil when chunk storage is disabled
	entities    EntityExtractor    // nil when entity extraction is disabled
	llmClient   llm.Completer      // nil when entity extraction is disabled
	instanceID  string             // per-instance watermark key (e.g., "laptop", "host1", "host2")
	cutover     time.Time          // zero = floor disabled; propagated to CutoverAwareCollectors

	// running is a global guard used by runAll / TriggerAll to prevent a
	// "run all collectors" operation from overlapping with another one.
	// Individual per-collector ticks use runningPerCollector instead, so that
	// distinct collectors can proceed concurrently.
	running atomic.Bool

	// runningPerCollector holds one try-lock per collector, keyed by Name().
	// Each cron tick for a specific collector performs a CompareAndSwap on its
	// own flag only, so a slow collector (e.g. a gmail backfill) cannot block
	// an unrelated collector (calendar, whisper, sms) from running.
	//
	// Pre-populated in New() and never mutated after construction — safe for
	// concurrent reads without additional locking.
	runningPerCollector map[string]*atomic.Bool
}

// New returns a Scheduler with the given collectors and storage backend.
// Use WithChunkStore to enable chunk-based FTS indexing (issue #9).
func New(store DocumentUpserter, embed search.EmbeddingEngine, collectors ...collector.Collector) *Scheduler {
	c := cron.New(cron.WithSeconds())

	// Pre-allocate one atomic.Bool per collector, keyed by Name().
	// The map itself is never mutated after this point, so concurrent access
	// to the map is safe (only the atomic.Bool values are written to later).
	rpc := make(map[string]*atomic.Bool, len(collectors))
	for _, col := range collectors {
		var b atomic.Bool
		rpc[col.Name()] = &b
	}

	return &Scheduler{
		cron:                c,
		collectors:          collectors,
		store:               store,
		embed:               embed,
		instanceID:          "default",
		runningPerCollector: rpc,
	}
}

// WithInstance sets the collector instance identifier used to key per-instance
// watermark state. Defaults to "default" when not called.
func (s *Scheduler) WithInstance(id string) *Scheduler {
	if id == "" {
		id = "default"
	}
	s.instanceID = id
	return s
}

// WithChunkStore attaches a ChunkStore so that each collected document is split
// into overlapping text chunks and stored in the chunks table for FTS indexing.
// When not called, chunk storage is disabled and the scheduler behaves as
// before (full-document FTS via documents.tsv only).
func (s *Scheduler) WithChunkStore(cs *store.ChunkStore) *Scheduler {
	s.chunkStore = cs
	return s
}

// WithEntityExtraction attaches the entity store and LLM client so that entity
// extraction is attempted inline after each document is persisted.
//
// This method should only be called when entity extraction is explicitly
// enabled (ENTITY_EXTRACTION_ENABLED=true in cmd/collector/main.go). When not
// called, s.entities and s.llmClient remain nil and the extractEntities call
// in processBatch is skipped entirely — zero LLM calls are made.
//
// Extraction is BEST-EFFORT: failures are logged as warnings and never block
// document ingestion. Safe to call with nil arguments — extraction is silently
// disabled in that case.
func (s *Scheduler) WithEntityExtraction(entities EntityExtractor, client llm.Completer) *Scheduler {
	s.entities = entities
	s.llmClient = client
	return s
}

// WithCutover sets the cutover floor time that is propagated to every
// CutoverAwareCollector (SMS, Whisper) registered with this scheduler.
// When non-zero, those collectors will not emit records whose event time
// (OccurredAt for SMS/call-log, mtime for Whisper) is before t — even when
// the record was never indexed (IndexAware path).
//
// Zero t (the default) disables the floor entirely (no behaviour change).
func (s *Scheduler) WithCutover(t time.Time) *Scheduler {
	s.cutover = t
	return s
}

// Register adds ONE cron job PER enabled collector (each "@every interval").
//
// Cron runs each job in its own goroutine, so distinct collectors execute
// concurrently. A given collector still skips its own overlapping tick via a
// per-collector try-lock (runningPerCollector). This eliminates the
// head-of-line blocking that occurred when a single global job ran all
// collectors sequentially: a slow gmail backfill (~60 min) no longer prevents
// calendar, whisper, or SMS collectors from running on their scheduled ticks.
//
// The old single-job / single-lock design that fixed the starvation bug is
// preserved in runAll, which is still used by TriggerAll.
func (s *Scheduler) Register(interval time.Duration) error {
	spec := fmt.Sprintf("@every %s", interval)

	for _, col := range s.collectors {
		col := col // capture loop variable
		if !col.Enabled() {
			slog.Info("scheduler: collector disabled, skipping", "name", col.Name())
			continue
		}
		slog.Info("scheduler: registered collector", "name", col.Name(), "interval", interval)
		if _, err := s.cron.AddFunc(spec, func() {
			s.run(context.Background(), col)
		}); err != nil {
			return fmt.Errorf("register collector %q: %w", col.Name(), err)
		}
	}
	return nil
}

// runAll acquires the GLOBAL running lock and executes all enabled collectors
// sequentially. It is used by TriggerAll (manual API trigger) and kept for
// test coverage of the "trigger all" semantic.
//
// cron already runs each job in its own goroutine, so runAll does NOT spawn
// an additional goroutine — doing so would release the outer cron goroutine
// immediately and break the overlap-skip semantics.
func (s *Scheduler) runAll(ctx context.Context) {
	if !s.running.CompareAndSwap(false, true) {
		slog.Warn("scheduler: collection already running, skipping tick")
		return
	}
	defer s.running.Store(false)

	for _, col := range s.collectors {
		if !col.Enabled() {
			continue
		}
		s.runCollector(ctx, col)
	}
}

// Start begins the cron scheduler. It is non-blocking.
func (s *Scheduler) Start() { s.cron.Start() }

// Stop gracefully halts the scheduler and waits for running jobs to finish.
func (s *Scheduler) Stop() { s.cron.Stop() }

// TriggerAll runs all enabled collectors immediately in the background,
// each in its own goroutine under its own per-collector lock.
// It is intended for manual /collect/trigger API calls.
// Collectors already running on their scheduled tick are skipped for the
// duration of that overlap; others proceed immediately.
func (s *Scheduler) TriggerAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, col := range s.collectors {
		col := col // capture loop variable
		if !col.Enabled() {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.run(ctx, col)
		}()
	}
	// Fire-and-forget at the call site; goroutines clean up via per-collector
	// locks. We do not block the caller.
	go wg.Wait()
}

// run is the cron-tick and manual-trigger entry point for a single collector.
// It acquires the PER-COLLECTOR running flag (CompareAndSwap 0→1) so that
// only one concurrent execution of this collector is allowed at a time, while
// other collectors proceed unimpeded.
func (s *Scheduler) run(ctx context.Context, col collector.Collector) {
	flag := s.runningPerCollector[col.Name()]
	if flag == nil {
		// Safety valve: collector was not in the original set (should not happen).
		slog.Warn("scheduler: no running flag for collector, skipping",
			"collector", col.Name())
		return
	}
	if !flag.CompareAndSwap(false, true) {
		slog.Warn("scheduler: collector still running, skipping tick",
			"collector", col.Name())
		return
	}
	defer flag.Store(false)
	s.runCollector(ctx, col)
}

// runningFor reports whether the named collector's per-collector flag is
// currently held. Intended for tests only.
func (s *Scheduler) runningFor(name string) bool {
	if flag, ok := s.runningPerCollector[name]; ok {
		return flag.Load()
	}
	return false
}

// runCollector executes a single collection cycle for one collector.
// It must only be called while the caller holds the collector's per-collector
// flag (via runningPerCollector[col.Name()]) or the global running flag
// (runAll path). It is safe to call from concurrent goroutines as long as
// each goroutine holds a distinct flag.
func (s *Scheduler) runCollector(ctx context.Context, col collector.Collector) {
	started := time.Now()
	defaultSince := time.Time{} // zero time = collect all files on first run
	since := s.store.LastCollectedAt(ctx, s.instanceID, col.Source(), defaultSince)

	// Cutover floor for date-watermark collectors (gmail, calendar, secretary,
	// llm-memory): if since is before the cutover, advance it to the cutover so
	// that these collectors only fetch data from after the cutover date.
	if !s.cutover.IsZero() && since.Before(s.cutover) {
		since = s.cutover
	}

	slog.Info("scheduler: starting collection",
		"collector", col.Name(),
		"instance", s.instanceID,
		"since", since.Format(time.RFC3339),
	)

	// For collectors that implement IndexAwareCollector (filesystem, SMS, whisper),
	// pre-load the set of already-indexed source_ids so the collector can detect
	// records that are new (never indexed) even when their event time predates the
	// cursor. This fixes two classes of data-loss bugs:
	//
	//  HIGH#1: late-arriving records (OneDrive sync lag) have OccurredAt/mtime
	//          before the watermark → pure event-time filter drops them forever.
	//  HIGH#2: after XML truncation the SourceID mechanism guarantees eventual
	//          re-collection of post-truncation records on the next successful run.
	//
	// We load the set once per run (not per-record) so the per-record cost is an
	// O(1) map lookup rather than a round-trip to the database.
	if iac, ok := col.(collector.IndexAwareCollector); ok && !since.IsZero() {
		indexedIDs, err := s.store.ActiveSourceIDSet(ctx, col.Source())
		if err != nil {
			// Non-fatal: fall back to event-time-only behaviour for this run.
			// Explicitly clear any stale set so the collector does not silently
			// carry over stale data from a previous successful run.
			iac.WithIndexedIDs(nil)
			slog.Warn("scheduler: could not load indexed source IDs, falling back to event-time-only",
				"collector", col.Name(), "error", err)
		} else {
			iac.WithIndexedIDs(indexedIDs)
			slog.Debug("scheduler: loaded indexed source IDs",
				"collector", col.Name(), "count", len(indexedIDs))
		}
	}

	// Propagate the cutover floor to collectors that support it (SMS, Whisper).
	// This is done after WithIndexedIDs so the collector has both the indexed
	// set and the cutover floor for its per-record emit decision.
	if cac, ok := col.(collector.CutoverAwareCollector); ok {
		cac.WithCutover(s.cutover)
	}

	var (
		count    int
		totalSeen int
	)

	processBatch := func(batch []model.Document) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		totalSeen += len(batch)

		// Optionally enrich documents with embeddings before upserting.
		if s.embed.Enabled() && len(batch) > 0 {
			s.embedDocuments(ctx, batch)
		}

		for i := range batch {
			if err := s.store.Upsert(ctx, &batch[i]); err != nil {
				if errors.Is(err, store.ErrDuplicateTranscript) {
					slog.Debug("scheduler: skipped duplicate call-transcript",
						"collector", col.Name(),
						"source_id", batch[i].SourceID)
					continue
				}
				slog.Warn("scheduler: upsert failed",
					"collector", col.Name(),
					"source_id", batch[i].SourceID,
					"error", err)
				continue
			}
			count++

			// Persist text chunks for FTS indexing (issue #9).
			// This replaces the previous 8 KB hard truncation (issue #3):
			// the full document content is now split into overlapping chunks and
			// stored in the chunks table. The documents.content column is unchanged.
			if s.chunkStore != nil {
				s.persistChunks(ctx, &batch[i])
			}

			// Best-effort entity extraction (issue #77).
			// Only runs when WithEntityExtraction was called (opt-in via
			// ENTITY_EXTRACTION_ENABLED=true). When disabled, both fields are
			// nil and this block is skipped — zero LLM calls are made.
			// Never blocks or fails document ingestion — errors are logged only.
			if s.entities != nil && s.llmClient != nil {
				s.extractEntities(ctx, &batch[i])
			}
		}
		return nil
	}

	var collectErr error
	if sc, ok := col.(collector.StreamingCollector); ok {
		collectErr = sc.CollectStream(ctx, since, processBatch)
	} else {
		docs, err := col.Collect(ctx, since)
		if err != nil {
			collectErr = err
		} else if len(docs) > 0 {
			collectErr = processBatch(docs)
		}
	}
	if collectErr != nil {
		slog.Error("scheduler: collection failed",
			"collector", col.Name(), "error", collectErr)
		_ = s.store.RecordCollectionLog(ctx, col.Source(), started, count, collectErr)
		return
	}

	_ = s.store.RecordCollectionLog(ctx, col.Source(), started, count, nil)

	// Persist the per-instance watermark so the next tick on this host picks up
	// incremental changes only. Using the run start time (rather than per-doc
	// max) is simpler and race-free: any document written during the run has
	// collected_at <= started, so the next scan with since=started is correct.
	if err := s.store.UpdateCollectorState(ctx, s.instanceID, col.Source(), started); err != nil {
		slog.Warn("scheduler: update collector state failed",
			"collector", col.Name(), "instance", s.instanceID, "error", err)
	}

	slog.Info("scheduler: collection complete",
		"collector", col.Name(),
		"upserted", count,
		"total", totalSeen,
		"elapsed", time.Since(started).Round(time.Millisecond),
	)

	// Soft-delete detection: if the collector can enumerate all current source IDs,
	// mark any DB-active documents whose source IDs are no longer present.
	//
	// Three-layer defence (issue #135):
	//   Layer 1 — collector.ListActiveSourceIDs returns error on missing/unmounted root.
	//   Layer 2 — scheduler skips MarkDeleted on error OR when deletion ratio exceeds threshold.
	//   Layer 3 — store.MarkDeleted no-ops on empty slice (belt-and-suspenders).
	if dd, ok := col.(collector.DeletionDetector); ok {
		allIDs, err := dd.ListActiveSourceIDs(ctx)
		if err != nil {
			// Layer 2a: error from collector (e.g. root not accessible / unmounted).
			// Do NOT call MarkDeleted — an empty/error result must never be treated
			// as "all files deleted".
			slog.Warn("scheduler: deletion detection skipped — ID listing failed (root may be unmounted)",
				"collector", col.Name(), "error", err)
			return
		}

		// Layer 2b: deletion-ratio sanity check.
		// If the store supports CountActiveDocuments, verify that the fraction of
		// documents that would be deleted is within the safe threshold.
		if counter, ok := s.store.(ActiveDocumentCounter); ok {
			activeInDB, countErr := counter.CountActiveDocuments(ctx, col.Source())
			if countErr != nil {
				slog.Warn("scheduler: deletion detection skipped — could not count active documents",
					"collector", col.Name(), "error", countErr)
				return
			}
			if deletionRatioWouldExceed(activeInDB, len(allIDs)) {
				wouldDelete := activeInDB - len(allIDs)
				slog.Warn("scheduler: deletion detection skipped — ratio exceeds safety threshold",
					"collector", col.Name(),
					"active_in_db", activeInDB,
					"active_on_source", len(allIDs),
					"would_delete", wouldDelete,
					"threshold_pct", int(deletionRatioThreshold*100))
				return
			}
		}

		deleted, err := s.store.MarkDeleted(ctx, col.Source(), allIDs)
		if err != nil {
			slog.Warn("scheduler: deletion detection failed",
				"collector", col.Name(), "error", err)
			return
		}
		if deleted > 0 {
			slog.Info("scheduler: marked deleted",
				"collector", col.Name(), "count", deleted)
		}
	}

	// Backfill embeddings for documents that were previously skipped due to
	// rate-limit errors. This runs once per collection cycle so that each
	// scheduler tick makes incremental progress through the NULL-embedding
	// backlog without requiring a full re-collection.
	s.backfillEmbeddings(ctx)
}

// backfillBatchSize is the number of unembedded documents processed per
// backfill cycle. Kept small enough to fit within OpenAI's rate limits:
// text-embedding-3-small allows 3,000 RPM; one batch of 200 documents costs
// one API call, so a single backfill pass is far below that ceiling.
// Raise this value only after verifying that the embedding endpoint can sustain
// the resulting request rate without triggering 429 errors.
const backfillBatchSize = 200

// backfillEmbeddings queries for active documents with a NULL embedding and
// embeds them in batches of backfillBatchSize. It is called at the end of
// every collection cycle so that documents that were skipped earlier (e.g.
// because of an OpenAI 429 rate-limit) are retried on subsequent ticks.
//
// On any EmbedBatch failure (including 429) the entire cycle is aborted and
// retried on the next scheduler tick.  Documents that already have an
// embedding are never returned by ListUnembedded and are therefore not
// re-processed.
func (s *Scheduler) backfillEmbeddings(ctx context.Context) {
	if !s.embed.Enabled() {
		return
	}

	docs, err := s.store.ListUnembedded(ctx, backfillBatchSize)
	if err != nil {
		slog.Warn("scheduler: backfill list unembedded failed", "error", err)
		return
	}
	if len(docs) == 0 {
		return
	}

	texts := make([]string, len(docs))
	for i, d := range docs {
		texts[i] = d.Title + "\n\n" + d.Content
	}

	vecs, err := s.embed.EmbedBatch(ctx, texts)
	if err != nil {
		// Rate-limit or transient API error: skip this cycle and retry next tick.
		slog.Warn("scheduler: backfill embedding failed, will retry next cycle",
			"error", err, "count", len(docs))
		return
	}

	succeeded := 0
	for i, doc := range docs {
		if i >= len(vecs) || len(vecs[i]) == 0 {
			continue
		}
		doc.Embedding = vecs[i]
		if err := s.store.UpdateEmbedding(ctx, doc); err != nil {
			slog.Warn("scheduler: backfill update embedding failed",
				"doc_id", doc.ID, "error", err)
			continue
		}
		succeeded++
	}

	slog.Info("scheduler: backfill embeddings complete",
		"processed", len(docs),
		"succeeded", succeeded,
	)
}

// embedDocuments fills the Embedding field of each document by calling the
// embedding API in batches to avoid timeout and payload-too-large errors.
//
// Full-document embeddings are kept alongside per-chunk embeddings so that the
// existing document-level vector search path (RRF fusion in search.Service)
// continues to work without regression. Per-chunk embedding is handled by
// embedChunks (called from persistChunks when both chunkStore and embed are
// configured). Both paths are additive.
func (s *Scheduler) embedDocuments(ctx context.Context, docs []model.Document) {
	const batchSize = 20

	for start := 0; start < len(docs); start += batchSize {
		end := start + batchSize
		if end > len(docs) {
			end = len(docs)
		}

		batch := docs[start:end]
		texts := make([]string, len(batch))
		for i, d := range batch {
			// Use title + content for a richer embedding context.
			// No truncation: full text is sent to the embedding API.
			// If the API returns a token-limit error, the error is logged and
			// the batch is skipped (FTS fallback remains active).
			texts[i] = d.Title + "\n\n" + d.Content
		}

		vecs, err := s.embed.EmbedBatch(ctx, texts)
		if err != nil {
			slog.Warn("scheduler: batch embedding failed, skipping batch",
				"error", err, "start", start, "end", end)
			continue
		}
		for i := range batch {
			if i < len(vecs) {
				docs[start+i].Embedding = vecs[i]
			}
		}

		slog.Info("scheduler: embedded batch", "start", start, "end", end, "total", len(docs))
	}
}

// persistChunks splits doc.Content into overlapping text chunks and stores
// them in the chunks table via ChunkStore.ReplaceDocument. A failure here is
// non-fatal: the document itself is already persisted in documents; only the
// chunk-based FTS and vector index are affected.
//
// Chunking strategy is selected per document by chunker.SelectOptions (issue #60).
// Long-form structured sources (filesystem, notion, github, gdrive) continue to
// use the heading-aware defaults (Target 2000 / Max 4000 / Overlap 100), so
// there is no behavioural regression for those sources.
//
// When the embedding client is configured, per-chunk embeddings are generated
// and persisted immediately after the chunks are stored (issue #71).
func (s *Scheduler) persistChunks(ctx context.Context, doc *model.Document) {
	texts := chunker.Split(doc.Content, chunker.SelectOptions(*doc))
	if len(texts) == 0 {
		return
	}

	chunks := make([]store.Chunk, 0, len(texts))
	for i, t := range texts {
		chunks = append(chunks, store.Chunk{
			DocumentID: doc.ID,
			ChunkIndex: i,
			Content:    t,
			ByteSize:   len(t),
		})
	}

	if err := s.chunkStore.ReplaceDocument(ctx, doc.ID, chunks); err != nil {
		slog.Error("scheduler: chunk persist failed",
			"err", err,
			"doc_id", doc.ID,
			"source_id", doc.SourceID,
			"chunk_count", len(chunks),
		)
		return // no point embedding if we could not store chunks
	}

	// Per-chunk embedding: generate and persist vectors for each chunk.
	// This is a best-effort operation: failure is non-fatal and only affects
	// vector search quality. FTS-based chunk search remains available.
	if s.embed.Enabled() {
		s.embedChunks(ctx, doc.ID, chunks)
	}
}

// embedChunks generates embedding vectors for chunks belonging to a single
// document and persists them via ChunkStore.UpdateChunkEmbeddings.
//
// Because ReplaceDocument uses CopyFrom which does not return inserted IDs,
// we query the database IDs back by (document_id, chunk_index) after
// insertion via ListByDocument.
//
// Failures are non-fatal: a warning is logged and the document remains
// searchable via FTS.
func (s *Scheduler) embedChunks(ctx context.Context, docID uuid.UUID, chunks []store.Chunk) {
	if len(chunks) == 0 {
		return
	}

	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Content
	}

	vecs, err := s.embed.EmbedBatch(ctx, texts)
	if err != nil {
		slog.Warn("scheduler: chunk embedding failed",
			"doc_id", docID,
			"chunk_count", len(chunks),
			"error", err,
		)
		return
	}

	// Fetch the database IDs for the chunks we just stored.
	// ReplaceDocument uses CopyFrom which doesn't return IDs, so we must
	// query them back. We do a single SELECT ordered by chunk_index.
	storedChunks, err := s.chunkStore.ListByDocument(ctx, docID)
	if err != nil {
		slog.Warn("scheduler: list chunks for embedding failed",
			"doc_id", docID,
			"error", err,
		)
		return
	}

	// Build a chunk_index → DB id map from the stored chunks.
	idxToID := make(map[int]int64, len(storedChunks))
	for _, sc := range storedChunks {
		idxToID[sc.ChunkIndex] = sc.ID
	}

	embeddings := make([]store.ChunkEmbedding, 0, len(chunks))
	for i, c := range chunks {
		if i >= len(vecs) || len(vecs[i]) == 0 {
			continue
		}
		id, ok := idxToID[c.ChunkIndex]
		if !ok {
			continue
		}
		embeddings = append(embeddings, store.ChunkEmbedding{
			ChunkID:   id,
			Embedding: vecs[i],
		})
	}

	if err := s.chunkStore.UpdateChunkEmbeddings(ctx, embeddings); err != nil {
		slog.Warn("scheduler: persist chunk embeddings failed",
			"doc_id", docID,
			"error", err,
		)
		return
	}

	slog.Debug("scheduler: chunk embeddings stored",
		"doc_id", docID,
		"count", len(embeddings),
	)
}

// extractEntities calls the LLM to extract named entities from doc and
// persists them via the entity store. All errors are logged as warnings;
// this method never returns an error so document ingestion is never blocked.
//
// The LLM call uses the request context so it respects scheduler shutdown.
// It runs synchronously in the collection goroutine to keep the architecture
// simple for the MVP; a dedicated background EntityWorker (internal/worker) is
// available for backfill of documents that were ingested before this feature
// was deployed.
func (s *Scheduler) extractEntities(ctx context.Context, doc *model.Document) {
	entities, err := worker.ExtractEntities(ctx, s.llmClient, doc)
	if err != nil {
		slog.Warn("scheduler: entity extraction failed (non-fatal)",
			"doc_id", doc.ID, "source_id", doc.SourceID, "error", err)
		return
	}
	if len(entities) == 0 {
		return
	}
	if linkErr := s.entities.UpsertAndLinkEntities(ctx, doc.ID, entities); linkErr != nil {
		slog.Warn("scheduler: entity link failed (non-fatal)",
			"doc_id", doc.ID, "count", len(entities), "error", linkErr)
	}
}

// Collectors returns the list of registered collectors (for status reporting).
func (s *Scheduler) Collectors() []collector.Collector { return s.collectors }

// slackCollector returns the first *collector.SlackCollector in the registry,
// or nil if none is registered.
func (s *Scheduler) slackCollector() *collector.SlackCollector {
	for _, col := range s.collectors {
		if sc, ok := col.(*collector.SlackCollector); ok {
			return sc
		}
	}
	return nil
}

// LookupSlackChannel resolves a channel name (case-insensitive, "#" stripped)
// to its Slack channel ID by querying the channels the bot is a member of.
// Returns ErrSlackCollectorNotFound when no Slack collector is configured,
// ErrSlackChannelNotFound when the name does not match any member channel.
func (s *Scheduler) LookupSlackChannel(ctx context.Context, name string) (id, channelName string, err error) {
	sc := s.slackCollector()
	if sc == nil {
		return "", "", ErrSlackCollectorNotFound
	}
	id, channelName, found, err := sc.FindMemberChannelByName(ctx, name)
	if err != nil {
		return "", "", fmt.Errorf("lookup slack channel %q: %w", name, err)
	}
	if !found {
		return "", "", ErrSlackChannelNotFound
	}
	return id, channelName, nil
}

// ForceCollectSlackChannel runs a full-history collection (since = zero time)
// for a single Slack channel and persists the resulting documents.
// It bypasses the source-level LastCollectedAt and is intended for manual
// POST /api/v1/collect/slack/channel calls.
//
// If channelID is empty, channelName is used to resolve the ID via the Slack
// API (the bot must be a member of the channel).
//
// Returns the number of upserted documents and any error.
func (s *Scheduler) ForceCollectSlackChannel(ctx context.Context, channelID, channelName string) (int, error) {
	sc := s.slackCollector()
	if sc == nil {
		return 0, ErrSlackCollectorNotFound
	}
	if !sc.Enabled() {
		return 0, fmt.Errorf("slack collector is disabled")
	}

	// Resolve channel ID from name when not provided.
	if channelID == "" {
		id, resolvedName, found, err := sc.FindMemberChannelByName(ctx, channelName)
		if err != nil {
			return 0, fmt.Errorf("resolve channel name %q: %w", channelName, err)
		}
		if !found {
			return 0, ErrSlackChannelNotFound
		}
		channelID = id
		channelName = resolvedName
	}

	started := time.Now()
	slog.Info("scheduler: force-collecting slack channel",
		"channel_id", channelID,
		"channel_name", channelName,
	)

	docs, err := sc.CollectChannel(ctx, channelID, channelName, time.Time{})
	if err != nil {
		slog.Error("scheduler: force-collect failed",
			"channel_id", channelID, "channel_name", channelName, "error", err)
		_ = s.store.RecordCollectionLog(ctx, sc.Source(), started, 0, err)
		return 0, fmt.Errorf("collect channel %s: %w", channelID, err)
	}

	if s.embed.Enabled() && len(docs) > 0 {
		s.embedDocuments(ctx, docs)
	}

	count := 0
	for i := range docs {
		if err := s.store.Upsert(ctx, &docs[i]); err != nil {
			if errors.Is(err, store.ErrDuplicateTranscript) {
				slog.Debug("scheduler: skipped duplicate call-transcript",
					"source_id", docs[i].SourceID)
				continue
			}
			slog.Warn("scheduler: force-collect upsert failed",
				"channel_id", channelID,
				"source_id", docs[i].SourceID,
				"error", err)
			continue
		}
		count++

		if s.chunkStore != nil {
			s.persistChunks(ctx, &docs[i])
		}
	}

	_ = s.store.RecordCollectionLog(ctx, sc.Source(), started, count, nil)
	slog.Info("scheduler: force-collect complete",
		"channel_id", channelID,
		"channel_name", channelName,
		"upserted", count,
		"total", len(docs),
		"elapsed", time.Since(started).Round(time.Millisecond),
	)
	return count, nil
}

// Sentinel errors for Slack channel operations.
var (
	ErrSlackCollectorNotFound = fmt.Errorf("slack collector not configured")
	ErrSlackChannelNotFound   = fmt.Errorf("channel not found in bot member list")
)
