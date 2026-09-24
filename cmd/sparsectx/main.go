// Command sparsectx backfills chunk_sparse_context (migration 040, #270
// phase B) — the derived sparse-matching text (title/participants header +
// chunk body) the SEARCH_CHUNK_SPARSE=fuse_ctx search knob reads.
//
// # Usage
//
//	sparsectx --recipe=v1-tp [--batch 500] [--sleep 200ms] [--dry-run] [--sweep] [--limit 5000]
//
// --recipe selects the sparse-text construction AND the chunk_sparse_context
// context_version the batch writes to: v1-tp (title+participants only) or
// v1-full (the same header the embedding pipeline uses). Required.
//
// --dry-run (default true) computes candidates and reports what WOULD be
// written but performs no database mutation — mirrors cmd/recordingbackfill's
// dry-run-by-default convention for scripts that touch production data.
// Pass --dry-run=false to actually write.
//
// --sweep restarts the checkpoint at 0 so every chunk is re-examined; rows
// whose document has not changed since the last pass are still skipped
// (UpsertSparseContextBatch's fingerprint comparison), so a sweep is cheap
// once the corpus is mostly caught up.
//
// --limit caps the total number of chunks processed in this invocation
// (converted internally to a --batch-sized number of batches) — for staged
// production rollout: dry-run, then a couple of small batches, then the full
// off-peak pass (see docs/chunk-sparse-context.md).
//
// Idempotent and resumable: interrupting a run (or the process being killed)
// leaves the checkpoint at the last FULLY COMMITTED batch — see
// internal/store.ChunkStore.UpsertSparseContextBatch's doc comment. Re-running
// with the same flags picks up from there.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/baekenough/second-brain/internal/chunkctx"
	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/sparsectx"
	"github.com/baekenough/second-brain/internal/store"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))
	if err := run(); err != nil {
		slog.Error("sparsectx failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	recipe := flag.String("recipe", "",
		"chunk_sparse_context recipe/context_version: "+chunkctx.RecipeTP+" (title+participants) or "+chunkctx.RecipeFull+" (embedding header). required")
	batch := flag.Int("batch", 500, "chunks per transaction (fetch + upsert + checkpoint advance)")
	sleep := flag.Duration("sleep", 200*time.Millisecond, "pause between batches (throttles load against a live database)")
	dryRun := flag.Bool("dry-run", true, "plan and log only, no database mutation. Pass --dry-run=false to actually write chunk_sparse_context rows and advance the checkpoint.")
	sweep := flag.Bool("sweep", false, "restart the checkpoint at 0 and re-examine every chunk (recomputes stale rows; unchanged rows still write 0)")
	limit := flag.Int("limit", 0, "cap the total chunks processed this run (0 = unlimited, i.e. run until exhausted or interrupted)")
	flag.Parse()

	if *recipe != chunkctx.RecipeTP && *recipe != chunkctx.RecipeFull {
		return fmt.Errorf("sparsectx: --recipe must be %q or %q, got %q", chunkctx.RecipeTP, chunkctx.RecipeFull, *recipe)
	}
	if *batch <= 0 {
		return fmt.Errorf("sparsectx: --batch must be > 0, got %d", *batch)
	}
	maxBatches := 0
	if *limit > 0 {
		maxBatches = (*limit + *batch - 1) / *batch // ceil(limit/batch)
	}

	// Best-effort local-dev convenience; Actions/production inject env vars directly.
	_ = godotenv.Overload()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pg, err := store.NewPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer pg.Close()
	cs := store.NewChunkStore(pg)

	if *dryRun {
		slog.Info("sparsectx: DRY_RUN mode (default) — no chunk_sparse_context rows will be written; pass --dry-run=false to apply",
			"recipe", *recipe, "batch", *batch, "sweep", *sweep)
	} else {
		slog.Warn("sparsectx: EXECUTE mode — writing chunk_sparse_context rows and advancing the checkpoint",
			"recipe", *recipe, "batch", *batch, "sweep", *sweep)
	}

	summary, runErr := sparsectx.Run(ctx, cs, sparsectx.Config{
		Recipe:     *recipe,
		BatchSize:  *batch,
		Sleep:      *sleep,
		DryRun:     *dryRun,
		Sweep:      *sweep,
		MaxBatches: maxBatches,
	}, func(format string, args ...any) {
		fmt.Printf(format+"\n", args...)
	})

	slog.Info("sparsectx: summary",
		"recipe", *recipe,
		"batches_run", summary.BatchesRun,
		"candidates", summary.Candidates,
		"written", summary.Written,
		"last_chunk_id", summary.LastChunkID,
		"done", summary.Done,
		"dry_run", *dryRun,
	)
	if runErr != nil {
		return runErr
	}
	return nil
}
