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

	"github.com/baekenough/second-brain/internal/chunker"
	"github.com/baekenough/second-brain/internal/collector"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/logsafe"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/baekenough/second-brain/internal/worker"
	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
)

// DocumentUpserter is the subset of the document store used by the scheduler.
type DocumentUpserter interface {
	// Upsert 는 내용이 바뀌었는지 알려 주지 않는 upsert 다. 사용자가 명시적으로
	// 요청한 전량 재수집(ForceCollectSlackChannel)만 쓴다 — 거기서는 청크를
	// 무조건 다시 만드는 것이 의도다.
	Upsert(ctx context.Context, doc *model.Document) error
	// UpsertTracked 는 정기 수집(processBatch)의 비병합 경로가 쓴다. 저장된
	// content 가 바뀌었는지 돌려주고, 스케줄러는 바뀌지 않았으면 청크·청크
	// 임베딩·엔티티 추출을 건너뛴다(#292). 통화 전사 보호로 들어온 요약 대신
	// 기존 전사가 남은 경우도 "바뀌지 않음" 이다 — store.callTranscriptKeptSQL.
	UpsertTracked(ctx context.Context, doc *model.Document) (contentChanged bool, err error)
	LastCollectedAt(ctx context.Context, instanceID string, src model.SourceType, fallback time.Time) time.Time
	UpdateCollectorState(ctx context.Context, instanceID string, src model.SourceType, lastCollectedAt time.Time) error
	RecordCollectionLog(ctx context.Context, src model.SourceType, started time.Time, count int, err error) error
	MarkDeleted(ctx context.Context, sourceType model.SourceType, activeIDs []string) (int, error)
	// CountActiveDocuments returns the number of active (non-deleted) documents for
	// the given source type. Required for the deletion-ratio sanity check (#148):
	// promoting this from an optional type-assertion to a required interface method
	// ensures the guard is always enforced at compile time — no store can silently
	// bypass the ratio check by omitting the method.
	CountActiveDocuments(ctx context.Context, sourceType model.SourceType) (int, error)
	// ListUnembedded returns up to limit active documents with a NULL embedding.
	ListUnembedded(ctx context.Context, limit int) ([]*model.Document, error)
	// ListDocumentsNeedingEmbedding 은 임베딩이 없거나 currentVersion 과 다른
	// 버전으로 만들어진 활성 문서를 돌려준다. currentVersion 이 빈 문자열이면
	// ListUnembedded 와 같다. 선택 메서드가 아니라 필수 메서드로 둔 이유는
	// CountActiveDocuments(#148) 와 같다 — 타입 어서션으로 두면 어떤 구현체가
	// 조용히 재임베딩을 건너뛰어도 컴파일 단계에서 드러나지 않는다.
	ListDocumentsNeedingEmbedding(ctx context.Context, limit int, currentVersion string) ([]*model.Document, error)
	// UpdateEmbedding persists the embedding vector for a single document.
	UpdateEmbedding(ctx context.Context, doc *model.Document) error
	// ActiveSourceIDSet returns the set of source_ids currently active in the
	// store for the given source type. Used by the filesystem collector to detect
	// files that are new (not yet indexed) regardless of their mtime.
	ActiveSourceIDSet(ctx context.Context, sourceType model.SourceType) (map[string]struct{}, error)
	// TranscribedSourceIDSet returns the set of source_ids the whisper pipeline
	// has already transcribed (transcription_ledger), regardless of whether the
	// resulting document was stored or rejected as a duplicate. Unioned with
	// ActiveSourceIDSet to build the authoritative "do NOT re-transcribe" set —
	// this fixes the infinite re-transcription loop for immutable audio.
	TranscribedSourceIDSet(ctx context.Context, sourceType model.SourceType) (map[string]struct{}, error)
	// RecordTranscribed durably records that the given source_ids were
	// transcribed for the source type. Idempotent (ON CONFLICT DO NOTHING),
	// no-op on empty input. Called per batch so duplicate-rejected files are
	// ledgered and never re-transcribed.
	RecordTranscribed(ctx context.Context, sourceType model.SourceType, sourceIDs []string) error
	// AttachTranscript merges a whisper transcript into an existing call
	// document (see model.SourceCall / store.AttachTranscript doc comments):
	// content/embedding are replaced, metadata is merged (not overwritten),
	// title/occurred_at are preserved when already set. Used instead of
	// Upsert whenever a batch document carries
	// metadata["transcript_source_id"] — WhisperCollector's signal that this
	// document should merge into the call-log document rather than become a
	// second, unlinked one. Returns contentChanged like UpsertTracked.
	AttachTranscript(ctx context.Context, doc *model.Document) (contentChanged bool, err error)
}

// ActiveDocumentCounter is retained for backward compatibility and for use by
// external callers that only need the count method (e.g. collection_status store
// query). It is now also satisfied by any DocumentUpserter implementation since
// CountActiveDocuments is a required method on that interface (#148).
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
	cron       *cron.Cron
	collectors []collector.Collector
	store      DocumentUpserter
	embed      search.EmbeddingEngine
	chunkStore *store.ChunkStore // nil when chunk storage is disabled
	entities   EntityExtractor   // nil when entity extraction is disabled
	llmClient  llm.Completer     // nil when entity extraction is disabled
	instanceID string            // per-instance watermark key (e.g., "laptop", "host1", "host2")
	cutover    time.Time         // zero = floor disabled; propagated to CutoverAwareCollectors
	// deletionRatioOverride, when true, bypasses the 50% deletion-ratio guard for
	// a single MarkDeleted pass. This is an escape hatch for legitimate large-scale
	// deletions (e.g. a user genuinely deletes >50% of a source's files) that would
	// otherwise be permanently blocked by the guard (#147).
	//
	// Trade-off: bypassing the guard removes protection against false-positives caused
	// by partial unmounts or transient filesystem errors. Use only when you are certain
	// the source root is fully mounted and the large deletion is intentional.
	// Set DELETION_RATIO_OVERRIDE=true in the environment; the scheduler reads it at
	// construction time via WithDeletionRatioOverride.
	deletionRatioOverride bool

	// documentEmbedVersion / chunkEmbedVersion 은 이번 프로세스가 만드는
	// 벡터에 찍을 버전 식별자다(internal/search.EmbeddingVersion). 빈 문자열
	// 이면 버전을 기록하지 않는다 — 배선 전 동작과 동일.
	documentEmbedVersion string
	chunkEmbedVersion    string

	// reembedStale 이 true 면 백필이 "버전이 다른" 행까지 집어 간다
	// (EMBEDDING_REEMBED_ENABLED). false 면 embedding IS NULL 인 행만 본다.
	reembedStale bool

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

// WithEmbeddingVersion 은 이 스케줄러가 만드는 벡터에 찍을 버전 식별자를
// 설정한다. model/dimensions 는 설정값(EMBEDDING_MODEL / EMBEDDING_DIMENSIONS)
// 을 그대로 넘기면 된다.
//
// 설정하지 않으면(빈 model) 버전을 기록하지 않으므로 재임베딩 선별도 동작하지
// 않는다 — 배선 누락이 조용한 전량 재임베딩으로 번지지 않도록 한 안전
// 기본값이다.
func (s *Scheduler) WithEmbeddingVersion(modelName string, dimensions int) *Scheduler {
	s.documentEmbedVersion = search.EmbeddingVersion(modelName, dimensions, search.RecipeDocumentV1)
	s.chunkEmbedVersion = search.EmbeddingVersion(modelName, dimensions, search.RecipeChunkContextV1)
	return s
}

// WithStaleReembedding 은 "현재 버전과 다른 벡터를 다시 만들지" 여부를 정한다
// (EMBEDDING_REEMBED_ENABLED). 기본은 false.
//
// 켜면 백필이 매 수집 사이클마다 구버전 문서·청크를 배치 단위로 다시 임베딩
// 한다. 전체 코퍼스를 한 번 훑는 동안은 새 벡터와 옛 벡터가 같은 인덱스에
// 섞여 있으므로(= 거리 비교가 부분적으로만 의미 있는 상태) 그 사실을 사이클
// 마다 경고 로그로 남긴다. 검색을 막지는 않는다 — 막으면 재임베딩이 끝날
// 때까지 서비스가 통째로 멈춘다.
func (s *Scheduler) WithStaleReembedding(enabled bool) *Scheduler {
	s.reembedStale = enabled
	return s
}

// embeddingSelectorVersion 은 백필 선별 쿼리에 넘길 버전 문자열이다.
// 재임베딩이 꺼져 있으면 빈 문자열을 돌려주어 "embedding IS NULL 인 행만"
// 이라는 기존 동작을 그대로 유지한다.
func (s *Scheduler) embeddingSelectorVersion(version string) string {
	if !s.reembedStale {
		return ""
	}
	return version
}

// WithDeletionRatioOverride enables the escape hatch for the deletion-ratio
// guard. When enabled=true, the 50% ratio check is skipped for the current
// scheduler instance — MarkDeleted will run regardless of the deletion fraction.
//
// Use this ONLY when you are certain that a legitimate large-scale deletion
// (>50% of a source's documents) needs to proceed. The guard exists to prevent
// accidental bulk data-loss caused by partially unmounted or glitching
// filesystems; bypassing it removes that protection.
//
// A warning is always logged when the override is active so that the bypass is
// visible in production logs. This is an intentional audit trail.
//
// Source: DELETION_RATIO_OVERRIDE=true in the environment (parsed in cmd/collector).
func (s *Scheduler) WithDeletionRatioOverride(enabled bool) *Scheduler {
	s.deletionRatioOverride = enabled
	return s
}

// IntervalPreferrer is an optional interface that a Collector may implement to
// advertise its preferred collection interval. When implemented, the scheduler
// uses this interval instead of the global COLLECT_INTERVAL for that collector.
//
// This enables per-source freshness SLAs without changing the base Collector
// interface (#143): push-based sources (SMS, recording ingest) can use a longer
// interval, while high-frequency sources (Whisper) can use a shorter one.
// Collectors that do not implement this interface default to the global interval.
type IntervalPreferrer interface {
	PreferredInterval() time.Duration
}

// Register adds ONE cron job PER enabled collector.
//
// The interval for each collector is resolved in this order:
//  1. Per-source env var: COLLECT_INTERVAL_<NAME> (upper-cased collector name,
//     hyphens and spaces replaced with underscores). E.g. COLLECT_INTERVAL_WHISPER.
//  2. Collector implements IntervalPreferrer → uses PreferredInterval().
//  3. Global interval (the interval argument).
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
func (s *Scheduler) Register(interval time.Duration, perSource ...map[string]time.Duration) error {
	// Merge optional per-source override map (e.g. from config).
	sourceIntervals := map[string]time.Duration{}
	for _, m := range perSource {
		for k, v := range m {
			sourceIntervals[k] = v
		}
	}

	for _, col := range s.collectors {
		col := col // capture loop variable
		if !col.Enabled() {
			slog.Info("scheduler: collector disabled, skipping", "name", col.Name())
			continue
		}

		// Resolve the effective interval for this collector (priority order above).
		eff := resolveCollectorInterval(col, interval, sourceIntervals)
		spec := fmt.Sprintf("@every %s", eff)

		slog.Info("scheduler: registered collector", "name", col.Name(), "interval", eff)
		if _, err := s.cron.AddFunc(spec, func() {
			s.run(context.Background(), col)
		}); err != nil {
			return fmt.Errorf("register collector %q: %w", col.Name(), err)
		}
	}
	return nil
}

// resolveCollectorInterval returns the effective cron interval for col.
// Priority: perSource map > IntervalPreferrer > global default.
func resolveCollectorInterval(col collector.Collector, global time.Duration, perSource map[string]time.Duration) time.Duration {
	if d, ok := perSource[col.Name()]; ok && d > 0 {
		return d
	}
	if ip, ok := col.(IntervalPreferrer); ok {
		if d := ip.PreferredInterval(); d > 0 {
			return d
		}
	}
	return global
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

// transcriptLedgerID returns the audio-file identity that should be recorded
// in the transcription ledger for doc: metadata["transcript_source_id"] when
// present — set by WhisperCollector's buildDocument when it merges a
// transcript into an existing call-log document (see
// internal/collector/whisper.go's callLogMergeSourceID) — falling back to
// doc.SourceID for a standalone transcript document whose SourceID already
// IS its own raw identity ("transcript:{relPath}"). See
// store.TranscribedSourceIDSet's doc comment for why this distinction matters.
func transcriptLedgerID(doc *model.Document) string {
	if v, ok := doc.Metadata["transcript_source_id"].(string); ok && v != "" {
		return v
	}
	return doc.SourceID
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
	//
	// The "&& !since.IsZero()" guard was REMOVED (infinite re-transcription loop
	// fix): when the watermark was zero (first run / after any reset) the index
	// set was not loaded at all, which fully disabled whisper dedup and made every
	// audio file look new every cycle. The set must always load.
	//
	// For whisper, the authoritative "do NOT re-transcribe" set is the UNION of
	//   active = ActiveSourceIDSet     (source_ids active in the search index)
	//   ledger = TranscribedSourceIDSet (source_ids already transcribed, including
	//            those rejected as duplicates or whose document was never stored)
	// Audio is immutable, so a source_id in this union must never be transcribed
	// again. If EITHER query errors we fall back to WithIndexedIDs(nil)
	// (mtime-only) so the collector never carries stale data and never blocks.
	if iac, ok := col.(collector.IndexAwareCollector); ok {
		active, activeErr := s.store.ActiveSourceIDSet(ctx, col.Source())
		ledger, ledgerErr := s.store.TranscribedSourceIDSet(ctx, col.Source())
		if activeErr != nil || ledgerErr != nil {
			// Non-fatal: fall back to event-time-only behaviour for this run.
			// Explicitly clear any stale set so the collector does not silently
			// carry over stale data from a previous successful run.
			iac.WithIndexedIDs(nil)
			slog.Warn("scheduler: could not load indexed/ledger source IDs, falling back to event-time-only",
				"collector", col.Name(),
				"active_error", activeErr,
				"ledger_error", ledgerErr)
		} else {
			// Union active ∪ ledger into a single set.
			union := make(map[string]struct{}, len(active)+len(ledger))
			for id := range active {
				union[id] = struct{}{}
			}
			for id := range ledger {
				union[id] = struct{}{}
			}
			iac.WithIndexedIDs(union)
			slog.Debug("scheduler: loaded indexed source IDs",
				"collector", col.Name(),
				"active", len(active),
				"ledger", len(ledger),
				"union", len(union))
		}
	}

	// Propagate the cutover floor to collectors that support it (SMS, Whisper).
	// This is done after WithIndexedIDs so the collector has both the indexed
	// set and the cutover floor for its per-record emit decision.
	if cac, ok := col.(collector.CutoverAwareCollector); ok {
		cac.WithCutover(s.cutover)
	}

	var (
		count     int
		totalSeen int
	)

	processBatch := func(batch []model.Document) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		totalSeen += len(batch)

		// Transcription ledger (infinite re-transcription loop fix):
		// The collector only returns SUCCESSFULLY-transcribed documents, so every
		// source_id in this batch has already been transcribed. Record them in the
		// ledger BEFORE the upsert loop so that even documents rejected as
		// duplicates (ErrDuplicateTranscript) — whose source_id never enters the
		// active index — are durably marked as "already transcribed" and are never
		// re-submitted to the (expensive) whisper API on a later cycle.
		//
		// col.Name() == "whisper" replaces the old col.Source() ==
		// model.SourceCallTranscript gate: since migration 033 unified
		// call-log/call-transcript into model.SourceCall, Source() is shared
		// with the SMS collector's call-log documents (which never go through
		// this codepath) and can no longer identify "this is whisper" by
		// itself — see WhisperCollector.Source's doc comment.
		//
		// ledgerID uses metadata["transcript_source_id"] when present — the
		// AUDIO FILE's own raw identity ("transcript:{relPath}"), preserved by
		// buildDocument even when the document itself is merge-keyed under
		// the call-log-formula SourceID — falling back to SourceID for
		// standalone transcripts (no sidecar / no call-log document to merge
		// into). Ledgering under any other id would let the raw identity
		// re-enter WalkDir's filter as "never transcribed" on the next cycle,
		// reintroducing the infinite re-transcription loop through the merge
		// path (see store.TranscribedSourceIDSet's doc comment).
		//
		// This is a single batched round-trip and is best-effort: a failure is
		// logged but never blocks ingestion (worst case is a redundant
		// re-transcription, which the active-index union still mostly prevents).
		if col.Name() == "whisper" && len(batch) > 0 {
			ids := make([]string, len(batch))
			for i := range batch {
				ids[i] = transcriptLedgerID(&batch[i])
			}
			if err := s.store.RecordTranscribed(ctx, col.Source(), ids); err != nil {
				slog.Warn("scheduler: record transcribed ledger failed (non-fatal)",
					"collector", col.Name(), "count", len(ids), "error", err)
			}
		}

		// Optionally enrich documents with embeddings before upserting.
		if s.embed.Enabled() && len(batch) > 0 {
			s.embedDocuments(ctx, batch)
		}

		for i := range batch {
			// A document carrying metadata["transcript_source_id"] is
			// WhisperCollector's signal (buildDocument, callLogMergeSourceID)
			// that it must MERGE into the existing call-log document at
			// (SourceType, SourceID) rather than overwrite it wholesale — see
			// store.AttachTranscript's doc comment for exactly what that
			// preserves (call-log-only metadata, title, authoritative
			// occurred_at).
			var (
				contentChanged bool
				upsertErr      error
			)
			if _, merging := batch[i].Metadata["transcript_source_id"]; merging {
				contentChanged, upsertErr = s.store.AttachTranscript(ctx, &batch[i])
			} else {
				contentChanged, upsertErr = s.store.UpsertTracked(ctx, &batch[i])
			}
			if upsertErr != nil {
				if errors.Is(upsertErr, store.ErrDuplicateTranscript) {
					slog.Debug("scheduler: skipped duplicate call content",
						"collector", col.Name(),
						"source_id", logsafe.SafeSourceID(batch[i].SourceID))
					continue
				}
				slog.Warn("scheduler: upsert failed",
					"collector", col.Name(),
					"source_id", logsafe.SafeSourceID(batch[i].SourceID),
					"error", upsertErr)
				continue
			}
			count++

			// 저장된 content 가 그대로면(같은 내용 재수집, 또는 통화 전사 보호로
			// 들어온 요약 대신 기존 전사가 남음) 뒤따르는 작업을 모두 건너뛴다
			// (#292). batch[i].Content 는 들어온 값이라 저장된 값과 다를 수 있다 —
			// 그것으로 청크를 다시 만들면 문서 행은 전사, 청크·청크 임베딩은 통화
			// 요약인 불일치가 된다(리뷰 재현: SMS XML 수집기가 전사된 통화를 재수집).
			//   - 청크·청크 임베딩: 내용이 같으면 이미 그 내용의 청크가 있다.
			//     재임베딩 비용(API 호출)도 아낀다.
			//   - 엔티티 추출(LLM): 추출은 문서 content 에서 한다. 내용이 같으면
			//     처음 저장할 때 이미 했고, 실패했거나 꺼져 있던 문서는
			//     entities_processed_at 기준의 EntityWorker 백필이 채운다. 보호된
			//     통화에서는 요약으로 추출하면 전사와 무관한 연결만 늘어난다.
			// 잃는 것: 예전에는 같은 내용을 다시 수집할 때마다 청크를 다시 만들어,
			// 앞선 청크 저장 실패가 우연히 복구됐다. 정기 수집은 워터마크 뒤의
			// 새·변경 문서만 가져오므로 이 복구는 실제로는 거의 일어나지 않았고,
			// 수동 전량 재수집(ForceCollectSlackChannel)은 여전히 무조건 다시 만든다.
			if !contentChanged {
				continue
			}

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
		// CountActiveDocuments is now a required method on DocumentUpserter (#148),
		// so the guard is always enforced — no silent bypass via missing type assertion.
		activeInDB, countErr := s.store.CountActiveDocuments(ctx, col.Source())
		if countErr != nil {
			slog.Warn("scheduler: deletion detection skipped — could not count active documents",
				"collector", col.Name(), "error", countErr)
			return
		}
		if deletionRatioWouldExceed(activeInDB, len(allIDs)) {
			wouldDelete := activeInDB - len(allIDs)
			// #147 escape hatch: DELETION_RATIO_OVERRIDE bypasses the ratio guard
			// for a legitimate one-off large deletion. A warning is always emitted
			// so the bypass is visible in production logs (intentional audit trail).
			if s.deletionRatioOverride {
				slog.Warn("scheduler: deletion ratio exceeds threshold BUT override is active — proceeding with deletion",
					"collector", col.Name(),
					"active_in_db", activeInDB,
					"active_on_source", len(allIDs),
					"would_delete", wouldDelete,
					"threshold_pct", int(deletionRatioThreshold*100),
					"override", "DELETION_RATIO_OVERRIDE=true")
				// fall through to MarkDeleted
			} else {
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

	// Backfill chunk embeddings for chunks whose embedding is NULL (e.g. due to
	// transient 429 errors during initial embedChunks call). Runs after the
	// document backfill so that document-level embeddings are prioritised.
	if s.chunkStore != nil {
		s.backfillChunkEmbeddings(ctx)
	}
}

// backfillBatchSize is the number of unembedded documents processed per
// backfill cycle. Kept small enough to fit within OpenAI's rate limits:
// text-embedding-3-small allows 3,000 RPM; one batch of 200 documents costs
// one API call, so a single backfill pass is far below that ceiling.
// Raise this value only after verifying that the embedding endpoint can sustain
// the resulting request rate without triggering 429 errors.
const backfillBatchSize = 200

// 재임베딩 모드(EMBEDDING_REEMBED_ENABLED=true)에서 한 사이클에 처리하는 건수.
//
// 평상시 백필은 "429 로 몇 건 밀린 것"을 따라잡는 용도라 작은 배치로 충분하다.
// 반면 재임베딩은 코퍼스 전체(문서 4.6만 / 청크 8.2만)를 한 번 훑어야 한다 —
// 청크 100건/사이클, 수집 주기 10분이면 827 사이클 = 5.7일이 걸려 사실상 못
// 쓴다. 그래서 재임베딩일 때만 배치를 키운다(청크 8.2만 ÷ 1000 ≈ 83 사이클 =
// 약 14시간).
//
// 상한의 근거: EmbedBatch 가 50만 자 단위로 하위 배치를 나눠 보내므로
// (internal/search/embed.go maxBatchChars) 요청 크기는 자동으로 분할된다.
// 여기서 제한하는 것은 한 사이클이 붙잡는 DB 행 수와 API 호출량이다.
const (
	reembedBatchSize      = 500
	chunkReembedBatchSize = 1000
)

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

	selector := s.embeddingSelectorVersion(s.documentEmbedVersion)
	batch := backfillBatchSize
	if selector != "" {
		batch = reembedBatchSize
	}
	docs, err := s.store.ListDocumentsNeedingEmbedding(ctx, batch, selector)
	if err != nil {
		slog.Warn("scheduler: backfill list unembedded failed", "error", err)
		return
	}
	if len(docs) == 0 {
		return
	}
	if selector != "" {
		// 모델/레시피 전환 중: 이 배치를 처리하는 동안에도 인덱스에는 옛 버전
		// 벡터가 남아 있다. 검색을 막지는 않되 상태는 드러낸다.
		slog.Warn("scheduler: re-embedding documents with a stale embedding version",
			"target_version", selector,
			"batch", len(docs),
		)
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
		doc.EmbeddingVersion = s.documentEmbedVersion
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

// chunkBackfillBatchSize is the number of unembedded chunks processed per
// backfill cycle. Smaller than the document backfill batch (200) because each
// chunk is typically shorter than a full document, but there may be many more
// of them. Keeping it at 100 limits per-cycle API load.
const chunkBackfillBatchSize = 100

// backfillChunkEmbeddings queries for active chunks with a NULL embedding and
// embeds them in batches of chunkBackfillBatchSize. It is called at the end of
// every collection cycle (after backfillEmbeddings) so that chunks whose
// embedding failed transiently (e.g. due to a 429 rate limit during
// embedChunks) are recovered on subsequent ticks.
//
// On any EmbedBatch failure the entire cycle is aborted and retried on the
// next scheduler tick. Chunks that already have an embedding are never returned
// by ListUnembeddedChunks and are therefore not re-processed.
//
// This is the per-chunk analogue of backfillEmbeddings (#141).
func (s *Scheduler) backfillChunkEmbeddings(ctx context.Context) {
	if !s.embed.Enabled() {
		return
	}

	selector := s.embeddingSelectorVersion(s.chunkEmbedVersion)
	batch := chunkBackfillBatchSize
	if selector != "" {
		batch = chunkReembedBatchSize
	}
	chunks, err := s.chunkStore.ListChunksNeedingEmbedding(ctx, batch, selector)
	if err != nil {
		slog.Warn("scheduler: chunk backfill list unembedded failed", "error", err)
		return
	}
	if len(chunks) > 0 && selector != "" {
		// 모델/레시피 전환 중 — backfillEmbeddings 쪽 주석 참고.
		slog.Warn("scheduler: re-embedding chunks with a stale embedding version",
			"target_version", selector,
			"batch", len(chunks),
		)
	}
	if len(chunks) == 0 {
		return
	}

	// 임베딩 입력에는 문맥 헤더를 붙인다(저장된 청크 본문은 건드리지 않는다).
	// 백필 경로는 문서 행을 함께 읽어 오므로(store.UnembeddedChunk) 인라인
	// 경로와 같은 헤더를 재구성할 수 있다.
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = withChunkContextHeader(model.Document{
			ID:         c.DocumentID,
			SourceType: c.SourceType,
			Title:      c.Title,
			OccurredAt: c.OccurredAt,
			Metadata:   c.Metadata,
		}, c.Content)
	}

	vecs, err := s.embed.EmbedBatch(ctx, texts)
	if err != nil {
		// Rate-limit or transient API error: skip this cycle and retry next tick.
		slog.Warn("scheduler: chunk backfill embedding failed, will retry next cycle",
			"error", err, "count", len(chunks))
		return
	}

	embeddings := make([]store.ChunkEmbedding, 0, len(chunks))
	for i, c := range chunks {
		if i >= len(vecs) || len(vecs[i]) == 0 {
			continue
		}
		embeddings = append(embeddings, store.ChunkEmbedding{
			ChunkID:   c.ID,
			Embedding: vecs[i],
			Version:   s.chunkEmbedVersion,
		})
	}

	if len(embeddings) == 0 {
		return
	}

	if err := s.chunkStore.UpdateChunkEmbeddings(ctx, embeddings); err != nil {
		slog.Warn("scheduler: chunk backfill update embeddings failed",
			"error", err, "count", len(embeddings))
		return
	}

	slog.Info("scheduler: chunk backfill embeddings complete",
		"processed", len(chunks),
		"succeeded", len(embeddings),
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
			if i < len(vecs) && len(vecs[i]) > 0 {
				docs[start+i].Embedding = vecs[i]
				// 벡터와 버전을 같은 자리에서 찍는다. Upsert 는 벡터가 있을
				// 때만 embedding_version 을 덮어쓴다(internal/store/document.go).
				docs[start+i].EmbeddingVersion = s.documentEmbedVersion
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
			"source_id", logsafe.SafeSourceID(doc.SourceID),
			"chunk_count", len(chunks),
		)
		return // no point embedding if we could not store chunks
	}

	// Per-chunk embedding: generate and persist vectors for each chunk.
	// This is a best-effort operation: failure is non-fatal and only affects
	// vector search quality. FTS-based chunk search remains available.
	if s.embed.Enabled() {
		s.embedChunks(ctx, *doc, chunks)
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
func (s *Scheduler) embedChunks(ctx context.Context, doc model.Document, chunks []store.Chunk) {
	if len(chunks) == 0 {
		return
	}

	docID := doc.ID

	// 임베딩 입력 = 문맥 헤더 + 청크 본문. 저장된 chunk.Content 는 그대로다
	// (FTS 인덱스와 검색 결과 표시가 헤더 텍스트로 오염되면 안 된다).
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = withChunkContextHeader(doc, c.Content)
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
			Version:   s.chunkEmbedVersion,
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
			"doc_id", doc.ID, "source_id", logsafe.SafeSourceID(doc.SourceID), "error", err)
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
					"source_id", logsafe.SafeSourceID(docs[i].SourceID))
				continue
			}
			slog.Warn("scheduler: force-collect upsert failed",
				"channel_id", channelID,
				"source_id", logsafe.SafeSourceID(docs[i].SourceID),
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
