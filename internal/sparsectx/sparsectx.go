// Package sparsectx runs the #270 phase B backfill: it fills
// chunk_sparse_context (migration 040) with deterministic sparse-matching
// text (internal/chunkctx.BuildSparseText) for every chunk, in batches so a
// run can be interrupted and resumed without re-scanning everything or
// losing progress.
//
// This package contains no SQL — internal/store.ChunkStore does the reading
// and writing (FetchSparseContextCandidates, UpsertSparseContextBatch); this
// package only sequences those calls and turns
// internal/chunkctx.BuildSparseText's output into rows. See cmd/sparsectx
// for the CLI wrapper.
package sparsectx

import (
	"context"
	"fmt"
	"time"

	"github.com/baekenough/second-brain/internal/chunkctx"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// Store is the subset of *store.ChunkStore this package needs. Defined here
// (rather than depending on the concrete type directly) purely for test
// substitutability — production always passes a *store.ChunkStore.
//
// Only two methods: FetchSparseContextCandidates now decides what still
// needs work directly from the data (an anti-join against
// chunk_sparse_context, #270 deep-verify HIGH), so Run no longer reads or
// resets a persisted checkpoint before starting a pass — see
// FetchSparseContextCandidates' doc comment (internal/store/chunk_sparse_context.go)
// for why a checkpoint-based `WHERE c.id > $checkpoint` filter could
// permanently skip a chunk whose insert commits out of id order, and for
// what its afterChunkID parameter is (and is not) for.
type Store interface {
	FetchSparseContextCandidates(ctx context.Context, version string, sweep bool, afterChunkID int64, batchSize int) ([]store.SparseContextCandidate, error)
	UpsertSparseContextBatch(ctx context.Context, version string, rows []store.SparseContextRow, checkpoint int64) (int64, error)
}

// Config controls one Run invocation.
type Config struct {
	// Recipe selects the sparse-text construction
	// (chunkctx.RecipeTP/RecipeFull) and doubles as chunk_sparse_context's
	// context_version — the two are the same string by design (#270 phase B).
	Recipe string

	// BatchSize is how many chunks FetchSparseContextCandidates reads (and
	// UpsertSparseContextBatch writes, in one transaction) per iteration.
	BatchSize int

	// Sleep pauses between batches — throttles backfill load against a
	// production database serving live traffic concurrently.
	Sleep time.Duration

	// DryRun computes candidates and would-be sparse_text but performs no
	// database write (no INSERT).
	DryRun bool

	// Sweep makes FetchSparseContextCandidates also re-examine chunks whose
	// chunk_sparse_context row EXISTS but has gone stale — its fingerprint no
	// longer matches the parent document's current one (title edit, metadata
	// merge, PII redaction). A plain (Sweep=false) pass only picks up chunks
	// with NO row yet, e.g. newly chunked documents; rows unchanged since the
	// last write are skipped either way by UpsertSparseContextBatch's
	// fingerprint comparison — Sweep does not force a rewrite of
	// already-current rows, it only widens which chunks are considered.
	Sweep bool

	// MaxBatches caps how many batches this Run call performs. 0 means no
	// cap (run until FetchSparseContextCandidates returns nothing).
	MaxBatches int
}

// Summary reports what one Run call did.
type Summary struct {
	BatchesRun int
	Candidates int
	Written    int64
	// LastChunkID is the highest chunk_id seen in the LAST batch this call
	// processed — a progress indicator for logging only (#270 deep-verify
	// HIGH follow-up: Run no longer uses any checkpoint to decide what to
	// fetch, so this field carries no resume semantics).
	LastChunkID int64
	// Done is true when the pass found nothing left needing work (the last
	// FetchSparseContextCandidates call returned zero rows) — as opposed to
	// stopping because MaxBatches was reached.
	Done bool
}

// LogFunc receives one line per batch (or dry-run projection). Modelled on
// cmd/recordingbackfill's callback so callers can route it to stdout, a
// structured logger, or nowhere (nil is safe: Run treats a nil LogFunc as
// "discard").
type LogFunc func(format string, args ...any)

// Run performs one backfill pass and returns when either
// FetchSparseContextCandidates is exhausted (Summary.Done=true) or
// cfg.MaxBatches batches have run, whichever comes first. ctx cancellation is
// checked between batches and during the inter-batch sleep.
//
// Run holds no state ACROSS CALLS and needs none for its real (writing)
// path: FetchSparseContextCandidates decides what still needs work directly
// from chunk_sparse_context's contents (an anti-join, #270 deep-verify HIGH
// — see its doc comment in internal/store/chunk_sparse_context.go), so
// interrupting a run and calling Run again with the same Config simply
// continues — a chunk already written (and still fresh, for Sweep=false)
// stops matching and is never re-fetched. Every non-dry-run batch passes
// afterChunkID=0, so nothing here can cause a WRITE to skip a chunk,
// including one whose insert committed, out of id order, after an earlier
// batch — even one earlier in THIS SAME Run call — already looked past its
// id.
//
// --dry-run is the one exception, and needs a small amount of state WITHIN
// a single call: it never writes anything, so nothing else can advance the
// anti-join between its batches — without SOME way to page forward, a
// dry-run spanning more chunks than one batch would refetch the identical
// first batch forever. Run tracks a local, in-memory-only cursor for this
// case (see FetchSparseContextCandidates' afterChunkID doc comment for why
// this is safe: a dry-run preview "skipping" a late-arriving low-id chunk
// has no lasting effect, since it writes nothing — the very next real run
// recomputes everything from scratch via the anti-join).
func Run(ctx context.Context, s Store, cfg Config, logf LogFunc) (Summary, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if _, ok := chunkctx.BuildSparseText(cfg.Recipe, chunkctxProbeDoc, ""); !ok {
		return Summary{}, fmt.Errorf("sparsectx: unknown --recipe %q", cfg.Recipe)
	}
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 500
	}

	version := cfg.Recipe
	if cfg.Sweep {
		logf("sweep: %s will also re-examine chunks whose row has gone stale (fingerprint mismatch)", version)
	} else {
		logf("running %s (chunks with no row yet)", version)
	}

	var summary Summary
	var dryRunCursor int64 // only read/advanced when cfg.DryRun; always 0 otherwise (see doc comment above)
	for {
		if ctx.Err() != nil {
			return summary, ctx.Err()
		}
		if cfg.MaxBatches > 0 && summary.BatchesRun >= cfg.MaxBatches {
			logf("stopping: reached --max-batches=%d", cfg.MaxBatches)
			break
		}

		candidates, err := s.FetchSparseContextCandidates(ctx, version, cfg.Sweep, dryRunCursor, batchSize)
		if err != nil {
			return summary, fmt.Errorf("fetch candidates: %w", err)
		}
		if len(candidates) == 0 {
			summary.Done = true
			logf("done: no chunks left needing a %s row", version)
			break
		}
		summary.BatchesRun++
		summary.Candidates += len(candidates)

		rows := make([]store.SparseContextRow, 0, len(candidates))
		var batchLastID int64
		for _, c := range candidates {
			text, ok := chunkctx.BuildSparseText(cfg.Recipe, c.Doc, c.Content)
			if !ok {
				// Validated once above via chunkctxProbeDoc — reaching here
				// would mean cfg.Recipe changed mid-run, which Run's single
				// invocation never does. Defensive only.
				return summary, fmt.Errorf("sparsectx: unknown --recipe %q on chunk_id %d", cfg.Recipe, c.ChunkID)
			}
			rows = append(rows, store.SparseContextRow{
				ChunkID:     c.ChunkID,
				Fingerprint: c.Fingerprint,
				SparseText:  text,
			})
			if c.ChunkID > batchLastID { // candidates arrive ascending by c.id (FetchSparseContextCandidates), so this is simply the last element's id
				batchLastID = c.ChunkID
			}
		}
		summary.LastChunkID = batchLastID

		if cfg.DryRun {
			dryRunCursor = batchLastID // advance the LOCAL preview-only cursor so the next fetch pages forward instead of repeating this batch
			logf("dry-run: batch %d would process %d chunk(s), up through chunk_id %d",
				summary.BatchesRun, len(rows), batchLastID)
		} else {
			written, err := s.UpsertSparseContextBatch(ctx, version, rows, batchLastID)
			if err != nil {
				return summary, fmt.Errorf("upsert batch: %w", err)
			}
			summary.Written += written
			logf("batch %d: %d candidate(s), %d written, up through chunk_id %d",
				summary.BatchesRun, len(rows), written, batchLastID)
		}

		if cfg.Sleep > 0 {
			select {
			case <-time.After(cfg.Sleep):
			case <-ctx.Done():
				return summary, ctx.Err()
			}
		}
	}

	return summary, nil
}

// chunkctxProbeDoc is an arbitrary, empty document used only to validate
// cfg.Recipe up front (chunkctx.BuildSparseText's ok return value) before
// touching the database — an unknown --recipe should fail immediately, not
// after a query round-trip.
var chunkctxProbeDoc = model.Document{}
