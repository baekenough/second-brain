package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/baekenough/second-brain/internal/collector"
	"github.com/baekenough/second-brain/internal/collector/extractor"
	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/scheduler"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/baekenough/second-brain/internal/worker"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	if err := run(); err != nil {
		slog.Error("startup failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	// Load .env file if present (ignore error — env vars may be set directly).
	_ = godotenv.Load()

	cfg, err := config.LoadCollector()
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// --- Database ---
	pg, err := store.NewPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pg.Close()

	// Run migrations from the migrations/ directory relative to the binary.
	migrationsDir := migrationsPath()
	if err := pg.RunMigrations(ctx, migrationsDir); err != nil {
		return err
	}

	docStore := store.NewDocumentStore(pg)
	extractionFailureStore := store.NewExtractionFailureStore(pg)
	chunkStore := store.NewChunkStore(pg)

	// --- Extraction retry worker ---
	// Periodically re-attempts failed file extractions. Remote-source failures
	// (Slack attachments) are skipped — see worker package for details.
	extractorReg := extractor.NewRegistry()
	retryWorker := worker.New(worker.Config{
		FailureStore: extractionFailureStore,
		DocStore:     docStore,
		Extractor:    worker.NewRegistryExtractor(extractorReg, 0),
		Interval:     time.Minute,
		BatchSize:    20,
	})
	go retryWorker.Run(ctx)

	// --- Embedding client ---
	embedClient := search.NewEmbedClient(cfg.EmbeddingAPIURL, cfg.EmbeddingAPIKey, cfg.CliProxyAuthFile, cfg.EmbeddingModel)
	if embedClient.Enabled() {
		slog.Info("embedding API configured", "url", cfg.EmbeddingAPIURL, "model", cfg.EmbeddingModel)
	} else {
		slog.Info("embedding API not configured — full-text search only")
	}

	// --- Collectors ---
	// Discord is intentionally excluded from the collector daemon; it is handled
	// by the API server which owns the WebSocket gateway and mention responses.
	collectors := []collector.Collector{
		collector.NewSlackCollector(cfg.SlackBotToken, cfg.SlackTeamID),
		collector.NewGitHubCollector(cfg.GitHubToken, cfg.GitHubOrg),
		collector.NewGDriveCollector(cfg.GDriveCredentialsJSON),
		collector.NewNotionCollector(cfg.NotionToken),
		collector.NewTelegramCollector(cfg.TelegramBotToken, cfg.TelegramChatIDs),
	}
	if cfg.FilesystemEnabled && cfg.FilesystemPath != "" {
		// Attempt to initialise the Drive exporter via ADC. If ADC is not
		// configured, driveExporter is nil and the filesystem collector falls
		// back to URL-only metadata for Google Workspace stub files.
		driveExporter, err := collector.NewDriveExporter(ctx)
		if err != nil {
			slog.Warn("filesystem: drive exporter init failed, workspace export disabled", "error", err)
			driveExporter = nil
		}
		collectors = append(collectors,
			collector.NewFilesystemCollectorWithDriveExport(cfg.FilesystemPath, driveExporter).
				WithExcludes(cfg.FilesystemExcludeDirs, cfg.FilesystemExcludeExts))
	}

	// --- Scheduler ---
	sched := scheduler.New(docStore, embedClient, collectors...).
		WithChunkStore(chunkStore).
		WithInstance(cfg.CollectorInstance)
	slog.Info("collector instance", "id", cfg.CollectorInstance)
	if err := sched.Register(cfg.CollectInterval); err != nil {
		return err
	}
	sched.Start()
	defer sched.Stop()

	// --- Slack channel watcher ---
	// When the bot is invited to a new channel, the watcher detects it within
	// the polling interval (60s) and triggers an immediate full-history
	// collection rather than waiting for the next cron tick (up to 10m).
	slackCol := collector.NewSlackCollector(cfg.SlackBotToken, cfg.SlackTeamID)
	if slackCol.Enabled() {
		watcher := collector.NewSlackChannelWatcher(slackCol, docStore, embedClient, 60*time.Second)
		go watcher.Run(ctx)
	}

	slog.Info("collector daemon started")
	<-ctx.Done()
	slog.Info("shutdown complete")
	return nil
}

// migrationsPath returns the path to the migrations directory.
// Resolution order:
//  1. MIGRATIONS_DIR env var (useful in Docker/k8s where -trimpath strips source paths)
//  2. runtime.Caller(0) relative path (works for go run / local dev builds)
//  3. "migrations" — CWD-relative fallback (used when WORKDIR=/app and migrations/ is there)
func migrationsPath() string {
	if dir := os.Getenv("MIGRATIONS_DIR"); dir != "" {
		return dir
	}
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return "migrations"
	}
	// When built with -trimpath, filename is a module-relative path
	// (e.g. github.com/baekenough/second-brain/cmd/collector/main.go) which is not
	// a real filesystem path. Detect this and fall back to CWD-relative path.
	if !filepath.IsAbs(filename) {
		return "migrations"
	}
	// filename is cmd/collector/main.go; walk up two levels to reach project root.
	root := filepath.Join(filepath.Dir(filename), "..", "..")
	return filepath.Join(root, "migrations")
}
