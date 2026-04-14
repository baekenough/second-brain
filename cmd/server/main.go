package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/baekenough/second-brain/internal/api"
	"github.com/baekenough/second-brain/internal/collector"
	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/scheduler"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
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

	cfg, err := config.Load()
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

	// --- Embedding client ---
	embedClient := search.NewEmbedClient(cfg.EmbeddingAPIURL, cfg.EmbeddingAPIKey, cfg.CliProxyAuthFile, cfg.EmbeddingModel)
	if embedClient.Enabled() {
		slog.Info("embedding API configured", "url", cfg.EmbeddingAPIURL, "model", cfg.EmbeddingModel)
	} else {
		slog.Info("embedding API not configured — full-text search only")
	}

	// --- Collectors ---
	collectors := []collector.Collector{
		collector.NewSlackCollector(cfg.SlackBotToken, cfg.SlackTeamID),
		collector.NewGitHubCollector(cfg.GitHubToken, cfg.GitHubOrg),
		collector.NewGDriveCollector(cfg.GDriveCredentialsJSON),
		// Notion collector disabled — not in use (re-enable by adding NewNotionCollector).
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
			collector.NewFilesystemCollectorWithDriveExport(cfg.FilesystemPath, driveExporter))
	}

	// Discord collector (optional).
	discordCol := collector.NewDiscordCollector(
		cfg.DiscordBotToken,
		cfg.DiscordApplicationID,
		cfg.DiscordGuildIDs,
		cfg.DiscordMentionResponseEnabled,
	)
	if discordCol.Enabled() {
		collectors = append(collectors, discordCol)
	}

	// --- Scheduler ---
	// The Discord collector uses its own interval (DISCORD_COLLECT_INTERVAL)
	// rather than the shared COLLECT_INTERVAL, so it is registered separately.
	otherCollectors := make([]collector.Collector, 0, len(collectors))
	for _, col := range collectors {
		if col.Source() != "discord" {
			otherCollectors = append(otherCollectors, col)
		}
	}
	sched := scheduler.New(docStore, embedClient, otherCollectors...)
	if err := sched.Register(cfg.CollectInterval); err != nil {
		return err
	}
	if discordCol.Enabled() {
		discordSched := scheduler.New(docStore, embedClient, discordCol)
		if err := discordSched.Register(cfg.DiscordCollectInterval); err != nil {
			return err
		}
		discordSched.Start()
		defer discordSched.Stop()
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

	// --- Search service ---
	searchSvc := search.NewService(docStore, embedClient)

	// --- LLM client (Discord RAG) ---
	llmClient := llm.New(llm.Config{
		BaseURL:     cfg.LLMAPIURL,
		Model:       cfg.LLMModel,
		APIKey:      cfg.LLMAPIKey,
		MaxTokens:   cfg.LLMMaxTokens,
		Temperature: cfg.LLMTemperature,
	}, nil)
	if llmClient.Enabled() {
		slog.Info("LLM client configured", "url", cfg.LLMAPIURL, "model", cfg.LLMModel)
	} else {
		slog.Info("LLM client not configured — Discord RAG disabled, static fallback active")
	}

	// --- Discord WebSocket gateway (mention responses) ---
	// The gateway is always-on when the bot token is set, independent of the
	// cron-based collection cycle.
	// searchSvc and llmClient are injected to enable the full RAG pipeline.
	discordGateway := collector.NewDiscordGateway(
		cfg.DiscordBotToken,
		cfg.DiscordMentionResponseEnabled,
		searchSvc,
		llmClient,
	)
	if discordGateway.Enabled() {
		go discordGateway.Run(ctx)
	}

	// --- HTTP server ---
	srv := api.NewServer(docStore, searchSvc, sched, cfg.FilesystemPath, cfg.APIKey)
	httpServer := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      srv.Handler(),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("server listening", "addr", httpServer.Addr)
		if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutting down...")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return err
	}

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
	// (e.g. github.com/baekenough/second-brain/cmd/server/main.go) which is not
	// a real filesystem path. Detect this and fall back to CWD-relative path.
	if !filepath.IsAbs(filename) {
		return "migrations"
	}
	// filename is cmd/server/main.go; walk up two levels to reach project root.
	root := filepath.Join(filepath.Dir(filename), "..", "..")
	return filepath.Join(root, "migrations")
}
