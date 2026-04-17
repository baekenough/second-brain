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
	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/llm"
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
	chunkStore := store.NewChunkStore(pg)           // issue #9: chunk-based FTS
	feedbackStore := store.NewFeedbackStore(pg)     // issue #17: user feedback
	evalStore := store.NewEvalStore(pg)             // issue #18: eval set export
	metricsStore := store.NewEvalMetricsStore(pg)   // issue #19: eval metrics history
	reindexStateStore := store.NewReindexStateStore(pg) // issue #20: reindex state tracking

	// --- Embedding client ---
	embedClient := search.NewEmbedClient(cfg.EmbeddingAPIURL, cfg.EmbeddingAPIKey, cfg.CliProxyAuthFile, cfg.EmbeddingModel)
	if embedClient.Enabled() {
		slog.Info("embedding API configured", "url", cfg.EmbeddingAPIURL, "model", cfg.EmbeddingModel)
	} else {
		slog.Info("embedding API not configured — full-text search only")
	}

	// --- Reranker (optional) ---
	reranker := search.NewHTTPReranker(cfg.RerankURL, cfg.RerankAPIKey, cfg.RerankModel, 0)
	if reranker.Enabled() {
		slog.Info("reranker configured", "url", cfg.RerankURL, "model", cfg.RerankModel)
	}

	// --- Search service ---
	// ChunkStore is attached to enable chunk-based FTS fallback (issue #9).
	searchSvc := search.NewService(docStore, embedClient).
		WithChunkStore(chunkStore).
		WithReranker(reranker)

	// --- LLM client (curation) ---
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
		slog.Info("LLM client not configured — curation features disabled")
	}

	// --- HTTP server ---
	srv := api.NewServer(docStore, searchSvc, feedbackStore, evalStore, llmClient, cfg.FilesystemPath, cfg.APIKey).
		WithReindexState(reindexStateStore).
		WithEvalMetrics(metricsStore).
		WithReindexCheck(search.NewReindexChecker(
			search.DefaultReindexConfig(),
			metricsStore,
			docStore,
			reindexStateStore,
		))
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
