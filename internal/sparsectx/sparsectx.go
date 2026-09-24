// Package sparsectx runs the #270 phase B backfill: it fills
// chunk_sparse_context (migration 040) with deterministic sparse-matching
// text (internal/chunkctx.BuildSparseText) for every chunk, in checkpointed
// batches so a run can be interrupted and resumed without re-scanning
// everything or losing progress.
//
// This package contains no SQL — internal/store.ChunkStore does the reading
// and writing (FetchSparseContextCandidates, UpsertSparseContextBatch,
// SparseBackfillCheckpoint, ResetSparseBackfillCheckpoint); this package only
// sequences those calls and turns internal/chunkctx.BuildSparseText's output
// into rows. See cmd/sparsectx for the CLI wrapper.
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
type Store interface {
	FetchSparseContextCandidates(ctx context.Context, afterChunkID int64, batchSize int) ([]store.SparseContextCandidate, error)
	UpsertSparseContextBatch(ctx context.Context, version string, rows []store.SparseContextRow, checkpoint int64) (int64, error)
	SparseBackfillCheckpoint(ctx context.Context, version string) (int64, error)
	ResetSparseBackfillCheckpoint(ctx context.Context, version string) error
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
	// database write (no INSERT, no checkpoint advance).
	DryRun bool

	// Sweep restarts the checkpoint at 0 before running, so every chunk is
	// re-examined and any row whose document changed since the last full
	// pass (title edit, metadata merge, PII redaction) is recomputed. Rows
	// unchanged since the last pass are skipped by
	// UpsertSparseContextBatch's fingerprint comparison — Sweep does not
	// force a rewrite of already-current rows.
	Sweep bool

	// MaxBatches caps how many batches this Run call performs. 0 means no
	// cap (run until FetchSparseContextCandidates returns nothing).
	MaxBatches int
}

// Summary reports what one Run call did.
type Summary struct {
	BatchesRun  int
	Candidates  int
	Written     int64
	LastChunkID int64
	// Done is true when the pass reached the end of the chunks table (the
	// last FetchSparseContextCandidates call returned zero rows) — as
	// opposed to stopping because MaxBatches was reached.
	Done bool
}

// LogFunc receives one line per batch (or dry-run projection). Modelled on
// cmd/recordingbackfill's callback so callers can route it to stdout, a
// structured logger, or nowhere (nil is safe: Run treats a nil LogFunc as
// "discard").
type LogFunc func(format string, args ...any)

// Run performs a checkpointed backfill pass and returns when either
// FetchSparseContextCandidates is exhausted (Summary.Done=true) or
// cfg.MaxBatches batches have run, whichever comes first. ctx cancellation
// is checked between batches and during the inter-batch sleep — a run always
// stops at a batch boundary, never mid-transaction, so the checkpoint it
// leaves behind is always consistent with what was actually written (see
// store.ChunkStore.UpsertSparseContextBatch's doc comment).
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
	var checkpoint int64
	if cfg.Sweep {
		logf("sweep: restarting %s from chunk_id 0 (stale rows are skipped by the fingerprint check, not force-rewritten)", version)
		if !cfg.DryRun {
			if err := s.ResetSparseBackfillCheckpoint(ctx, version); err != nil {
				return Summary{}, fmt.Errorf("reset checkpoint: %w", err)
			}
		}
	} else {
		cp, err := s.SparseBackfillCheckpoint(ctx, version)
		if err != nil {
			return Summary{}, fmt.Errorf("read checkpoint: %w", err)
		}
		checkpoint = cp
		logf("resuming %s from chunk_id %d", version, checkpoint)
	}

	summary := Summary{LastChunkID: checkpoint}
	for {
		if ctx.Err() != nil {
			return summary, ctx.Err()
		}
		if cfg.MaxBatches > 0 && summary.BatchesRun >= cfg.MaxBatches {
			logf("stopping: reached --max-batches=%d", cfg.MaxBatches)
			break
		}

		candidates, err := s.FetchSparseContextCandidates(ctx, checkpoint, batchSize)
		if err != nil {
			return summary, fmt.Errorf("fetch candidates: %w", err)
		}
		if len(candidates) == 0 {
			summary.Done = true
			logf("done: no more chunks after chunk_id %d", checkpoint)
			break
		}
		summary.BatchesRun++
		summary.Candidates += len(candidates)

		rows := make([]store.SparseContextRow, 0, len(candidates))
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
			checkpoint = c.ChunkID // candidates are ordered by c.id ascending (FetchSparseContextCandidates)
		}

		if cfg.DryRun {
			logf("dry-run: batch %d would process %d chunk(s), checkpoint would advance to %d",
				summary.BatchesRun, len(rows), checkpoint)
		} else {
			written, err := s.UpsertSparseContextBatch(ctx, version, rows, checkpoint)
			if err != nil {
				return summary, fmt.Errorf("upsert batch: %w", err)
			}
			summary.Written += written
			logf("batch %d: %d candidate(s), %d written, checkpoint=%d",
				summary.BatchesRun, len(rows), written, checkpoint)
		}

		if cfg.Sleep > 0 {
			select {
			case <-time.After(cfg.Sleep):
			case <-ctx.Done():
				summary.LastChunkID = checkpoint
				return summary, ctx.Err()
			}
		}
	}

	summary.LastChunkID = checkpoint
	return summary, nil
}

// chunkctxProbeDoc is an arbitrary, empty document used only to validate
// cfg.Recipe up front (chunkctx.BuildSparseText's ok return value) before
// touching the database — an unknown --recipe should fail immediately, not
// after a query round-trip.
var chunkctxProbeDoc = model.Document{}
