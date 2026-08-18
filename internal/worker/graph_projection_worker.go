package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/baekenough/second-brain/internal/model"
)

// Tunables for the graph projection worker.
const (
	defaultGraphProjectionInterval = 5 * time.Minute
	defaultGraphProjectionBatch    = 500

	// defaultGraphProjectionOverlap re-reads this many relation ids below the
	// watermark on every cycle. entity_relations ids come from a BIGSERIAL, so
	// a transaction that reserved a lower id can commit after one with a
	// higher id; without an overlap window a strictly-increasing watermark
	// would step over it. Re-reading is free in correctness terms because
	// every projection write is idempotent (relationship MERGE keys on pgId).
	defaultGraphProjectionOverlap int64 = 1000

	// graphProjectionTickTimeout bounds one whole tick. Unlike the extraction
	// worker there is no LLM in this path, so a single modest budget is
	// enough — the only remote calls are PostgreSQL reads and Neo4j writes.
	graphProjectionTickTimeout = 2 * time.Minute

	// maxRelationBatchesPerTick keeps a first-run backfill from monopolising
	// the process: the remaining batches are simply picked up next tick.
	maxRelationBatchesPerTick = 20

	// maxMentionPagesPerTick bounds the full sweep of document_entities for
	// the same reason.
	maxMentionPagesPerTick = 200
)

// GraphProjectionSource is the PostgreSQL read side. *store.GraphSource
// satisfies it.
type GraphProjectionSource interface {
	ListRelationsAfter(ctx context.Context, afterID int64, limit int) ([]model.ProjectionRelation, error)
	ListMentionsAfter(ctx context.Context, afterDocumentID string, afterEntityID int64, limit int) ([]model.ProjectionMention, error)
}

// GraphProjector is the Neo4j write side. *graph.Projector satisfies it; the
// interface is declared here so tests can substitute a fake without a live
// database.
type GraphProjector interface {
	EnsureSchema(ctx context.Context) error
	State(ctx context.Context) (model.GraphProjectionState, error)
	UpsertRelations(ctx context.Context, rows []model.ProjectionRelation) error
	UpsertMentions(ctx context.Context, rows []model.ProjectionMention) error
	SetLastRelationID(ctx context.Context, id int64) error
	SetResetToken(ctx context.Context, token string) error
	Wipe(ctx context.Context) error
}

// GraphProjectionWorkerConfig holds configuration for GraphProjectionWorker.
type GraphProjectionWorkerConfig struct {
	Source    GraphProjectionSource
	Projector GraphProjector
	Interval  time.Duration
	BatchSize int
	Overlap   int64
	// ResetToken is compared against the token stored in the graph. Changing
	// it is the rebuild switch: the next tick wipes the graph and reprojects
	// from scratch. Empty on both sides (the default) means "never rebuild".
	ResetToken string
}

// GraphProjectionWorker mirrors ExtractionWorker's shape — a ticker driving
// serial batches, where an individual failure logs and returns instead of
// killing the worker.
//
// The one invariant worth stating out loud: the watermark only advances after
// the corresponding batch has been written successfully. Advancing past a
// failed batch would lose those relations permanently, since nothing ever
// revisits ids below the watermark (outside the overlap window).
type GraphProjectionWorker struct {
	source     GraphProjectionSource
	projector  GraphProjector
	interval   time.Duration
	batchSize  int
	overlap    int64
	resetToken string

	// schemaReady records that EnsureSchema succeeded once in this process.
	// Constraints survive a Wipe, so this is not reset on rebuild.
	schemaReady bool
}

// NewGraphProjectionWorker constructs a worker from cfg. Panics when required
// fields are nil (programming error) — mirrors NewExtractionWorker.
func NewGraphProjectionWorker(cfg GraphProjectionWorkerConfig) *GraphProjectionWorker {
	if cfg.Source == nil {
		panic("GraphProjectionWorkerConfig.Source must not be nil")
	}
	if cfg.Projector == nil {
		panic("GraphProjectionWorkerConfig.Projector must not be nil")
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultGraphProjectionInterval
	}
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = defaultGraphProjectionBatch
	}
	overlap := cfg.Overlap
	if overlap <= 0 {
		overlap = defaultGraphProjectionOverlap
	}
	return &GraphProjectionWorker{
		source:     cfg.Source,
		projector:  cfg.Projector,
		interval:   interval,
		batchSize:  batchSize,
		overlap:    overlap,
		resetToken: cfg.ResetToken,
	}
}

// Run blocks until ctx is cancelled (mirrors ExtractionWorker.Run).
func (w *GraphProjectionWorker) Run(ctx context.Context) {
	slog.Info("graph projection worker started", "interval", w.interval, "batch_size", w.batchSize)
	w.tick(ctx)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("graph projection worker stopped")
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

// tick projects one cycle: schema → state/rebuild → relations → mentions.
//
// Logging carries counts and durations only. Entity names, document titles and
// Cypher parameter maps are never logged (privacy policy §4) — the graph's
// value is precisely that it contains real names.
func (w *GraphProjectionWorker) tick(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, graphProjectionTickTimeout)
	defer cancel()
	started := time.Now()

	if !w.schemaReady {
		if err := w.projector.EnsureSchema(ctx); err != nil {
			slog.Warn("graph projection: ensure schema failed", "error", err)
			return
		}
		w.schemaReady = true
	}

	state, err := w.projector.State(ctx)
	if err != nil {
		slog.Warn("graph projection: read state failed", "error", err)
		return
	}

	if state.ResetToken != w.resetToken {
		slog.Info("graph projection: reset token changed, rebuilding graph")
		if err := w.projector.Wipe(ctx); err != nil {
			slog.Warn("graph projection: wipe failed", "error", err)
			return
		}
		if err := w.projector.SetResetToken(ctx, w.resetToken); err != nil {
			slog.Warn("graph projection: set reset token failed", "error", err)
			return
		}
		state = model.GraphProjectionState{ResetToken: w.resetToken}
	}

	relations := w.projectRelations(ctx, state.LastRelationID)
	mentions := w.projectMentions(ctx)

	if relations > 0 || mentions > 0 {
		slog.Info("graph projection: cycle complete",
			"relations", relations, "mentions", mentions, "duration_ms", time.Since(started).Milliseconds())
	}
}

// projectRelations walks forward from the watermark (minus the overlap window)
// and returns how many rows were projected.
func (w *GraphProjectionWorker) projectRelations(ctx context.Context, watermark int64) int {
	cursor := watermark - w.overlap
	if cursor < 0 {
		cursor = 0
	}

	projected := 0
	for batch := 0; batch < maxRelationBatchesPerTick; batch++ {
		if ctx.Err() != nil {
			return projected
		}
		rows, err := w.source.ListRelationsAfter(ctx, cursor, w.batchSize)
		if err != nil {
			slog.Warn("graph projection: list relations failed", "error", err)
			return projected
		}
		if len(rows) == 0 {
			return projected
		}
		if err := w.projector.UpsertRelations(ctx, rows); err != nil {
			// Return WITHOUT advancing the watermark. The same rows are read
			// again next tick.
			slog.Warn("graph projection: upsert relations failed", "count", len(rows), "error", err)
			return projected
		}
		projected += len(rows)

		maxID := rows[len(rows)-1].ID
		if err := w.projector.SetLastRelationID(ctx, maxID); err != nil {
			slog.Warn("graph projection: set watermark failed", "error", err)
			return projected
		}
		cursor = maxID
		if len(rows) < w.batchSize {
			return projected
		}
	}
	slog.Info("graph projection: relation batch cap reached, continuing next tick")
	return projected
}

// projectMentions sweeps document_entities from the beginning every cycle.
//
// A saved cursor cannot work here: the keyset is (document_id, entity_id) and
// document ids are random UUIDs, so a newly linked document can sort BELOW a
// previously saved cursor and would never be picked up. The table is small
// enough that a full sweep is cheap; the plan flags ~100k rows as the point to
// switch to a created_at watermark instead.
func (w *GraphProjectionWorker) projectMentions(ctx context.Context) int {
	var (
		cursorDoc    string
		cursorEntity int64
		projected    int
	)
	for page := 0; page < maxMentionPagesPerTick; page++ {
		if ctx.Err() != nil {
			return projected
		}
		rows, err := w.source.ListMentionsAfter(ctx, cursorDoc, cursorEntity, w.batchSize)
		if err != nil {
			slog.Warn("graph projection: list mentions failed", "error", err)
			return projected
		}
		if len(rows) == 0 {
			return projected
		}
		if err := w.projector.UpsertMentions(ctx, rows); err != nil {
			slog.Warn("graph projection: upsert mentions failed", "count", len(rows), "error", err)
			return projected
		}
		projected += len(rows)

		last := rows[len(rows)-1]
		cursorDoc, cursorEntity = last.DocumentID, last.EntityID
		if len(rows) < w.batchSize {
			return projected
		}
	}
	slog.Info("graph projection: mention page cap reached, continuing next tick")
	return projected
}
