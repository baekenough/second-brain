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
	"github.com/baekenough/second-brain/internal/collector/extractor"
	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/llm"
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
	extractionFailureStore := store.NewExtractionFailureStore(pg)
	chunkStore := store.NewChunkStore(pg)       // issue #9: chunk-based FTS
	feedbackStore := store.NewFeedbackStore(pg) // issue #17: user feedback
	evalStore := store.NewEvalStore(pg)         // issue #18: eval set export

	// --- Extraction retry worker ---
	// Periodically re-attempts failed file extractions. Remote-source failures
	// (Slack/Discord attachments) are skipped — see worker package for details.
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
	// Uses NewDiscordCollectorWithAttachments so that file attachments in messages
	// are downloaded, text-extracted, and stored as separate documents (issue #27).
	discordCol := collector.NewDiscordCollectorWithAttachments(
		cfg.DiscordBotToken,
		cfg.DiscordApplicationID,
		cfg.DiscordGuildIDs,
		cfg.DiscordMentionResponseEnabled,
		docStore,
		extractionFailureStore,
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
	sched := scheduler.New(docStore, embedClient, otherCollectors...).WithChunkStore(chunkStore)
	if err := sched.Register(cfg.CollectInterval); err != nil {
		return err
	}
	if discordCol.Enabled() {
		discordSched := scheduler.New(docStore, embedClient, discordCol).WithChunkStore(chunkStore)
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
	// ChunkStore is attached to enable chunk-based FTS fallback (issue #9).
	// The embedding code path is preserved; TODO(issue#9-embed) tracks promotion
	// to per-chunk embeddings once cliproxy /v1/embeddings is confirmed (#34).
	searchSvc := search.NewService(docStore, embedClient).WithChunkStore(chunkStore)

	// --- LLM client (Discord RAG) ---
	llmClient := llm.New(llm.Config{
		BaseURL:     cfg.LLMAPIURL,
		Model:       cfg.LLMModel,
		APIKey:      cfg.LLMAPIKey,
		AuthFile:    cfg.LLMAuthFile,
		MaxTokens:   cfg.LLMMaxTokens,
		Temperature: cfg.LLMTemperature,
	}, nil)
	if llmClient.Enabled() {
		slog.Info("LLM client configured", "url", cfg.LLMAPIURL, "model", cfg.LLMModel)
	} else {
		slog.Info("LLM client not configured — Discord RAG disabled, static fallback active")
	}

	// --- Discord response metrics (issue #41) ---
	// Shared between the gateway (records per-response latency) and the HTTP
	// server (exposes snapshot via GET /api/v1/stats/baseline).
	discordMetrics := &collector.DiscordMetrics{}

	// --- Discord WebSocket gateway (mention responses + real-time collection) ---
	// The gateway is always-on when the bot token is set, independent of the
	// cron-based collection cycle.
	// searchSvc and llmClient are injected to enable the full RAG pipeline.
	//
	// SetDocStore enables the real-time MessageCreate → Upsert path (issue #38).
	// The 5-minute cron collector continues to run as a backfill for messages
	// missed during gateway downtime; duplicate source_ids are de-duplicated by
	// the UNIQUE constraint on documents.source_id.
	discordGateway := collector.NewDiscordGateway(
		cfg.DiscordBotToken,
		cfg.DiscordMentionResponseEnabled,
		searchSvc,
		llmClient,
	)
	if discordCol.Enabled() {
		// Share the same docStore as the cron collector so both paths write to
		// the same table with the same de-duplication semantics.
		discordGateway.SetDocStore(docStore)

		// Wire the feedback store so that 👍/👎 reactions on bot replies are
		// persisted. The adapter translates collector.FeedbackEntry → store.Feedback
		// and delegates to FeedbackStore.Upsert for toggle/replace semantics.
		discordGateway.SetFeedbackStore(collector.NewFeedbackStoreAdapter(feedbackStore))
	}
	// Always inject metrics so the gateway records latency even when the
	// Discord collector is disabled (gateway can still be enabled via token).
	discordGateway.SetMetrics(discordMetrics)

	if discordGateway.Enabled() {
		go discordGateway.Run(ctx)
	}

	// --- HTTP server ---
	srv := api.NewServer(docStore, searchSvc, sched, feedbackStore, evalStore, cfg.FilesystemPath, cfg.APIKey)
	srv.SetDiscordMetrics(discordMetrics)
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
