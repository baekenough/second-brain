// Command recordingbackfill migrates call-recording audio files (and their
// JSON sidecars) that were ingested before v0.21.2 (#164) and therefore still
// carry a raw phone number in plaintext:
//
//   - filenames of the form {rawNumber}_{YYYYMMDDHHMMSS}[-N].{ext}, and/or
//   - sidecars ({audioPath}.meta.json) with a plaintext "number" field.
//
// Both are rewritten to the current scheme: filenames of the form
// {numHash}_{YYYYMMDDHHMMSS}[-N].{ext} and sidecars carrying "number_hash"
// only (see internal/api/ingest_recording.go). Renaming an audio file changes
// the SourceID that WhisperCollector computes for it
// ("transcript:"+relPath) — this tool renames the corresponding
// documents.source_id row in the SAME migration step so that the existing
// call-transcript document is neither re-transcribed nor orphaned. See
// internal/recordingbackfill's package doc for the full hazard analysis and
// crash-safety ordering.
//
// # Usage
//
//	recordingbackfill [--execute] [--dir <path>]
//
// DRY_RUN is the default: with no flags, the tool only scans and logs the
// actions it WOULD take (no file or database mutation). Pass --execute to
// actually perform the migration. This mirrors a hard project rule from a
// past ops incident: scripts that mutate production data default to
// dry-run and require an explicit opt-in flag to mutate anything.
//
// --dir overrides the recording root directory; it defaults to
// $WHISPER_AUDIO_DIR (the same directory WhisperCollector scans).
//
// Idempotent: files already in the new scheme (hash-format filename with a
// number_hash-only sidecar) are detected and skipped on every run, so running
// this tool twice — or resuming after an interrupted --execute run — is safe.
//
// Exit codes: 0 success, 1 on a fatal error OR when any file could not be
// migrated cleanly (skips due to an undetectable phone number, per-file
// errors, or source_id conflicts — see the logged summary for details).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"
	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/recordingbackfill"
	"github.com/baekenough/second-brain/internal/store"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))
	if err := run(); err != nil {
		slog.Error("recordingbackfill failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	execute := flag.Bool("execute", false, "perform the migration (renames files, rewrites sidecars, updates documents.source_id). Default is dry-run: plan and log only, no mutation.")
	dir := flag.String("dir", "", "recording root directory to scan (default: $WHISPER_AUDIO_DIR)")
	flag.Parse()

	// Best-effort local-dev convenience; Actions/production inject env vars directly.
	_ = godotenv.Overload()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	rootDir := cfg.WhisperAudioDir
	if *dir != "" {
		rootDir = *dir
	}
	if rootDir == "" {
		return fmt.Errorf("recording root directory not set: pass --dir or set WHISPER_AUDIO_DIR")
	}
	if info, statErr := os.Stat(rootDir); statErr != nil || !info.IsDir() {
		return fmt.Errorf("recording root directory %q is not accessible: %v", rootDir, statErr)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	dryRun := !*execute
	if dryRun {
		slog.Info("recordingbackfill: DRY_RUN mode (default) — no files or database rows will be modified; pass --execute to apply changes",
			"dir", rootDir)
	} else {
		slog.Warn("recordingbackfill: EXECUTE mode — files will be renamed, sidecars rewritten, and documents.source_id updated",
			"dir", rootDir)
	}

	// The database is only required in execute mode: dry-run purely inspects
	// the filesystem and never calls SourceIDStore (see recordingbackfill.Run).
	var sidStore recordingbackfill.SourceIDStore
	if !dryRun {
		pg, pgErr := store.NewPostgres(ctx, cfg.DatabaseURL)
		if pgErr != nil {
			return fmt.Errorf("connect to postgres: %w", pgErr)
		}
		defer pg.Close()
		sidStore = recordingbackfill.NewPostgresSourceIDStore(pg.Pool())
	}

	summary, runErr := recordingbackfill.Run(ctx, recordingbackfill.RunConfig{
		RootDir: rootDir,
		DryRun:  dryRun,
	}, sidStore, func(format string, args ...any) {
		fmt.Printf(format+"\n", args...)
	})
	if runErr != nil {
		return runErr
	}

	slog.Info("recordingbackfill: done",
		"total_files", summary.TotalFiles,
		"renamed", summary.Renamed,
		"sidecar_rewritten", summary.SidecarRewritten,
		"already_migrated", summary.AlreadyMigrated,
		"unrecognized", summary.Unrecognized,
		"skipped", summary.Skipped,
		"conflicts", summary.Conflicts,
		"errors", summary.Errors,
		"dry_run", dryRun,
	)

	if summary.Errors > 0 || summary.Conflicts > 0 {
		return fmt.Errorf("recordingbackfill completed with %d error(s) and %d conflict(s) — see logs for details", summary.Errors, summary.Conflicts)
	}
	return nil
}
