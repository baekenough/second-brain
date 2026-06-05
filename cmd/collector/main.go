package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/baekenough/second-brain/internal/collector"
	"github.com/baekenough/second-brain/internal/collector/extractor"
	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/scheduler"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/setup"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/baekenough/second-brain/internal/worker"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "setup" {
		if err := setup.Run(os.Args[2:]); err != nil {
			// Use fmt.Fprintf instead of slog: slog.SetDefault has not been
			// called yet at this point, so slog would use the default text
			// handler — inconsistent with the daemon's JSON handler. The setup
			// subcommand is a CLI tool for humans; plain stderr is correct.
			fmt.Fprintf(os.Stderr, "setup failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	if err := run(); err != nil {
		slog.Error("startup failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	// Overload .env file if present (ignore error — env vars may be set directly).
	// Overload() forces .env values to win over pre-existing env vars, preventing
	// stale/empty values (e.g. empty ANTHROPIC_API_KEY) from causing 401 failures.
	_ = godotenv.Overload()

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
	if err := pg.RunMigrations(ctx, migrationsDir, cfg.EmbeddingDim); err != nil {
		return err
	}

	docStore := store.NewDocumentStore(pg)
	extractionFailureStore := store.NewExtractionFailureStore(pg)
	chunkStore := store.NewChunkStore(pg)

	// wg tracks long-running background goroutines that need a graceful drain
	// window on SIGTERM before the process exits (#65).
	var wg sync.WaitGroup

	// drainTimeout is the maximum time to wait for in-flight ticks to finish
	// after the shutdown signal is received.  10 s is long enough to let a
	// running UpdateSummary or embedding call complete; if the LLM is slow the
	// tick exits cleanly because it uses context.WithoutCancel internally and
	// the WaitGroup drain acts as the hard ceiling.
	const drainTimeout = 10 * time.Second

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
	wg.Add(1)
	go func() {
		defer wg.Done()
		retryWorker.Run(ctx)
	}()

	// --- Embedding client ---
	embedClient := search.NewEmbedClient(cfg.EmbeddingAPIURL, cfg.EmbeddingAPIKey, cfg.CliProxyAuthFile, cfg.EmbeddingModel)
	if embedClient.Enabled() {
		slog.Info("embedding API configured", "url", cfg.EmbeddingAPIURL, "model", cfg.EmbeddingModel)
	} else {
		slog.Info("embedding API not configured — full-text search only")
	}

	// --- LLM client (for summarization) ---
	llmClient := llm.New(llm.Config{
		BaseURL:     cfg.LLMAPIURL,
		Model:       cfg.LLMModel,
		APIKey:      cfg.LLMAPIKey,
		AuthFile:    cfg.LLMAuthFile,
		MaxTokens:   cfg.LLMMaxTokens,
		Temperature: cfg.LLMTemperature,
	}, nil)

	// --- Summarizer worker ---
	// Backfills LLM-generated title_summary / bullet_summary / summary_embedding
	// for documents that have not yet been summarized.  The worker uses
	// FOR UPDATE SKIP LOCKED so multiple collector instances share the work
	// without duplicate LLM calls (#64).
	//
	// SUMMARIZER_ENABLED=false disables the worker entirely on replicas that
	// should not run summarization (e.g. when you want a single dedicated
	// summarizer pod in k8s).  The worker itself logs a diagnostic message
	// when the LLM is not configured (#67).
	summarizerEnabled := os.Getenv("SUMMARIZER_ENABLED") != "false"
	if !summarizerEnabled {
		slog.Info("summarizer worker disabled via SUMMARIZER_ENABLED=false")
	}
	summarizerWorker := worker.NewSummarizerWorker(worker.SummarizerConfig{
		Store:     docStore,
		LLM:       llmClient,
		Embedder:  embedClient,
		Interval:  5 * time.Minute,
		BatchSize: 10,
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		if !summarizerEnabled {
			// Block until shutdown so the WaitGroup stays balanced.
			<-ctx.Done()
			return
		}
		summarizerWorker.Run(ctx)
	}()

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
	if cfg.SecretaryDBPath != "" {
		collectors = append(collectors, collector.NewSecretaryCollector(cfg.SecretaryDBPath))
	}
	if cfg.LLMMemoryDBPath != "" {
		collectors = append(collectors, collector.NewLLMMemoryCollector(cfg.LLMMemoryDBPath))
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

	// Bounded drain: give in-flight goroutines (summarizer, retry worker) time
	// to finish their current tick before the process exits.
	//
	// SummarizerWorker uses maxTickDuration (8 s) which is shorter than
	// drainTimeout (10 s), so in-flight ticks always complete before the
	// drain window closes (#65).
	//
	// The drainDone channel is buffered so that the wg.Wait() goroutine can
	// send without blocking even when the drain timeout fires first — this
	// prevents the goroutine from leaking after process exit.
	drainDone := make(chan struct{}, 1)
	go func() {
		wg.Wait()
		drainDone <- struct{}{}
	}()
	select {
	case <-drainDone:
		slog.Info("shutdown complete — all workers drained cleanly")
	case <-time.After(drainTimeout):
		slog.Warn("shutdown: drain timeout exceeded, forcing exit",
			"timeout", drainTimeout)
	}
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
