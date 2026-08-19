// Command tune searches for better hybrid-search weights against the labelled
// feedback collected by /ask, and — when the evidence is strong enough —
// promotes them.
//
// Usage:
//
//	tune -propose            # measure and record a candidate; change nothing
//	tune -promote            # same, and make it live if the gate agrees
//	tune -rollback           # undo the current promotion
//	tune -dry-run -promote   # measure and report, write nothing
//
// It refuses to do anything unless TUNE_ENABLED is set to a true value.
//
// WHERE THIS RUNS: on the same host as PostgreSQL. macmini containers cannot
// reach ubuntu1's Tailscale addresses and the reverse is blocked too (measured,
// not assumed), so a batch job on the far side of the database simply does not
// work. It also means the reranker HTTP service is not reachable from here,
// which is why every evaluation runs with rerank off — see tune.RunnerFor.
// When PostgreSQL moves to ubuntu1, this binary moves with it.
//
// It does not run migrations. Schema changes arrive with the server; a batch
// job that silently migrates the database of a service it does not own is a
// surprise waiting to happen.
//
// Exit codes:
//
//	0 — ran successfully, or declined to run because TUNE_ENABLED is unset
//	1 — failed
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/dataset"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/baekenough/second-brain/internal/telemetry"
	"github.com/joho/godotenv"
)

// toolVersion is recorded in the history row's metadata so a decision can be
// attributed to a build.
const toolVersion = "tune/1"

// otelShutdownTimeout bounds how long flushing spans may delay exit. A dead
// collector must never hold up a batch job.
const otelShutdownTimeout = 5 * time.Second

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if err := realMain(); err != nil {
		if errors.Is(err, errDisabled) {
			// Not a failure: the operator has said, by omission, that tuning is
			// off on this host. Exiting non-zero here would make a weekly cron
			// mail an error every week for a deliberately disabled feature,
			// which is how real alerts get filtered away.
			slog.Warn("tune: disabled (set TUNE_ENABLED=true to run)")
			return
		}
		slog.Error("tune failed", "error", err)
		os.Exit(1)
	}
}

func realMain() error {
	propose := flag.Bool("propose", false, "measure a candidate and record it without changing the live weights (default)")
	promote := flag.Bool("promote", false, "measure a candidate and make it live if the promotion gate allows it")
	rollback := flag.Bool("rollback", false, "undo the current promotion and restore the configuration it replaced")
	dryRun := flag.Bool("dry-run", false, "measure and report, but write nothing to the database")
	flag.Parse()

	mode, err := selectMode(*propose, *promote, *rollback)
	if err != nil {
		return err
	}

	// The flag is checked before anything is opened: with tuning off this
	// binary must not connect to the database or read a single label.
	if !tuneEnabled() {
		return errDisabled
	}

	_ = godotenv.Overload()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	otelShutdown, err := telemetry.InitOTel(ctx, cfg.LangfuseOTLPEndpoint, cfg.LangfusePublicKey, cfg.LangfuseSecretKey)
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), otelShutdownTimeout)
		defer shutdownCancel()
		if err := otelShutdown(shutdownCtx); err != nil {
			slog.Warn("telemetry: shutdown error (spans may have been dropped)", "error", err)
		}
	}()

	pg, err := store.NewPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pg.Close()

	evalStore := store.NewEvalStore(pg)
	history := store.NewWeightsHistoryStore(pg)

	d := deps{
		Load: func(ctx context.Context) (*dataset.TrainSet, *dataset.HoldoutSet, error) {
			return dataset.Load(ctx, evalStore)
		},
		History: history,
	}

	// Rollback needs neither search nor embeddings; building them would make
	// the one command that must work when things are broken depend on the most
	// fragile parts of the system.
	if mode != modeRollback {
		embedClient, err := search.NewEmbeddingEngine(cfg)
		if err != nil {
			return fmt.Errorf("embedding engine: %w", err)
		}
		// No reranker is wired: it is unreachable from the batch host and
		// tune.RunnerFor forces UseRerank=false regardless.
		d.Search = search.NewService(store.NewDocumentStore(pg), embedClient).
			WithChunkStore(store.NewChunkStore(pg))
	}

	out, err := run(ctx, d, options{Mode: mode, DryRun: *dryRun, ToolVersion: toolVersion})
	if err != nil {
		return err
	}

	// The report goes to stdout as JSON so a cron job can archive it. It
	// contains weights and aggregate numbers — no queries, no document IDs.
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("write report: %w", err)
	}

	slog.Info("tune: done",
		"mode", out.Mode,
		"dry_run", out.DryRun,
		"history_id", out.HistoryID,
		"promoted", out.Promoted,
		"gate_reason", out.GateReason,
		"train_queries", out.TrainQueries,
		"holdout_queries", out.HoldoutQueries)
	return nil
}

// selectMode turns the three boolean flags into one mode, refusing ambiguity
// rather than picking a winner: "-promote -rollback" is a typo whose two
// possible readings do opposite things to the live configuration.
func selectMode(propose, promote, rollback bool) (string, error) {
	n := 0
	for _, b := range []bool{propose, promote, rollback} {
		if b {
			n++
		}
	}
	if n > 1 {
		return "", errors.New("tune: choose exactly one of -propose, -promote, -rollback")
	}
	switch {
	case promote:
		return modePromote, nil
	case rollback:
		return modeRollback, nil
	default:
		// No flag at all means the safe mode: measure and record.
		return modePropose, nil
	}
}
