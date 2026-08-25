package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/baekenough/second-brain/internal/api"
	"github.com/baekenough/second-brain/internal/briefing"
	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/graph"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/baekenough/second-brain/internal/telemetry"
	"github.com/joho/godotenv"
)

// briefingCacheSize bounds the in-memory LRU briefing.Cache (plan Task 12).
// Small on purpose: the cache key is the full open-action set, so a single-
// user deployment rarely has more than a handful of distinct sets active at
// once.
const briefingCacheSize = 8

// otelShutdownTimeout bounds how long the deferred telemetry shutdown may
// block process exit. A dead/unreachable Langfuse collector must never
// delay shutdown — see internal/telemetry's non-blocking guarantee.
const otelShutdownTimeout = 5 * time.Second

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
	// Overload .env file if present (ignore error — env vars may be set directly).
	// Overload() forces .env values to win over pre-existing env vars, preventing
	// stale/empty values (e.g. empty ANTHROPIC_API_KEY) from causing 401 failures.
	_ = godotenv.Overload()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// --- Telemetry (OpenTelemetry → Langfuse, OTLP/HTTP) ---
	// A no-op TracerProvider is configured when cfg.LangfuseOTLPEndpoint is
	// empty (the default) — zero behavior change until an operator sets
	// LANGFUSE_OTLP_ENDPOINT/LANGFUSE_PUBLIC_KEY/LANGFUSE_SECRET_KEY.
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
	chunkStore := store.NewChunkStore(pg)               // issue #9: chunk-based FTS
	feedbackStore := store.NewFeedbackStore(pg)         // issue #17: user feedback
	evalStore := store.NewEvalStore(pg)                 // issue #18: eval set export
	metricsStore := store.NewEvalMetricsStore(pg)       // issue #19: eval metrics history
	reindexStateStore := store.NewReindexStateStore(pg) // issue #20: reindex state tracking
	entityStore := store.NewEntityStore(pg)             // issue #77: knowledge-graph entities
	askSessionStore := store.NewAskSessionStore(pg)     // multi-turn /ask conversation history

	// --- Embedding engine ---
	embedClient, err := search.NewEmbeddingEngine(cfg)
	if err != nil {
		return fmt.Errorf("embedding engine: %w", err)
	}
	if embedClient.Enabled() {
		slog.Info("embedding engine configured", "provider", cfg.EmbeddingProvider)
	} else {
		slog.Info("embedding engine not configured — full-text search only")
	}

	// --- Reranker (optional) ---
	reranker := search.NewHTTPReranker(cfg.RerankURL, cfg.RerankAPIKey, cfg.RerankModel, 0)
	if reranker.Enabled() {
		slog.Info("reranker configured", "url", cfg.RerankURL, "model", cfg.RerankModel)
	}

	// --- OpenSearch BM25 (nori) lane (optional) ---
	// Disabled unless OPENSEARCH_URL is set — see internal/config.Config
	// and internal/search/opensearch.go for why this lane exists.
	osLane := search.NewOpenSearchLane(cfg)
	if osLane != nil {
		slog.Info("opensearch BM25 lane enabled", "url", cfg.OpensearchURL, "index", cfg.OpensearchIndex)
	} else {
		slog.Info("opensearch BM25 lane disabled — OPENSEARCH_URL not set")
	}

	// --- Search service ---
	// ChunkStore is attached to enable chunk-based FTS fallback (issue #9).
	// EntityStore is attached to surface entities in search results (issue #77).
	//
	// WeightsHistoryStore closes the tuning loop (#214): cmd/tune -promote
	// writes the winning configuration to search_weights_history, and this is
	// the only process that reads it back. The reader is attached
	// unconditionally and the flag decides whether it is consulted, so that
	// turning SEARCH_ACTIVE_WEIGHTS_ENABLED on is a restart and not a redeploy.
	// It is attached HERE and nowhere else on purpose: cmd/tune must keep
	// measuring against the compiled defaults, or its baseline would drift to
	// whatever it last promoted and every subsequent run would compare a
	// configuration against itself.
	weightsHistoryStore := store.NewWeightsHistoryStore(pg)
	searchSvc := search.NewService(docStore, embedClient).
		WithChunkStore(chunkStore).
		WithReranker(reranker).
		WithEntityFetcher(entityStore).
		WithOpenSearch(osLane).
		WithActiveWeights(weightsHistoryStore, cfg.SearchActiveWeightsEnabled)
	if cfg.SearchActiveWeightsEnabled {
		slog.Info("search: serving the promoted weights from search_weights_history",
			"flag", "SEARCH_ACTIVE_WEIGHTS_ENABLED")
	}

	// --- LLM client (curation) ---
	llmClient := llm.New(llm.Config{
		BaseURL:     cfg.LLMAPIURL,
		Model:       cfg.LLMModel,
		APIKey:      cfg.LLMAPIKey,
		AuthFile:    cfg.LLMAuthFile,
		MaxTokens:   cfg.LLMMaxTokens,
		Temperature: cfg.LLMTemperature,
		Thinking:    cfg.LLMThinking,
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
		)).
		WithIngestFile(docStore, chunkStore, embedClient, cfg.IngestMaxFileBytes).
		WithPIINumberHashing(cfg.PIINumberHashingEnabled).
		WithIngestMessages(docStore, chunkStore, embedClient, cfg.IngestMaxBatchMessages, cfg.CollectorCutover).
		WithIngestRecording(docStore, cfg.IngestRecordingDir, cfg.IngestMaxFileBytes, cfg.CollectorCutover).
		WithNotes(docStore, chunkStore, embedClient).
		WithAskConfig(time.Duration(cfg.AskTimeoutSeconds)*time.Second, cfg.AskContextTopK, cfg.AskContextInsightM).
		WithAskSessions(askSessionStore)

	// --- Actions & Briefing (Part C, plan Task 12, feature-flagged off) ---
	actionStore := store.NewActionQueryStore(pg)
	srv = wireActionsAndBriefing(srv, cfg, actionStore, actionStore, llmClient.Enabled())

	// --- Per-evidence feedback (Part D, feature-flagged off) ---
	// The votes live in the same feedback store as the existing answer-level
	// feedback, so no new connection or store is introduced here.
	srv = wireEvidenceFeedback(srv, cfg, feedbackStore)

	// --- Graph read API (Part B, optional) ---
	// Wired only when both NEO4J_URI and NEO4J_PASSWORD are present. A
	// connection failure logs a warning and leaves the graph routes
	// unregistered: the API server must never fail to start because a derived
	// projection is unavailable.
	if neo4jURI, neo4jPassword := os.Getenv("NEO4J_URI"), os.Getenv("NEO4J_PASSWORD"); neo4jURI != "" && neo4jPassword != "" {
		graphClient, err := graph.New(ctx, graph.Config{
			URI:      neo4jURI,
			Username: os.Getenv("NEO4J_USERNAME"),
			Password: neo4jPassword,
		})
		if err != nil {
			slog.Warn("graph read API disabled — neo4j unavailable", "error", err)
		} else {
			defer func() { _ = graphClient.Close(context.Background()) }()
			srv = srv.WithGraph(graph.NewReader(graphClient))
			slog.Info("graph read API enabled")
		}
	}

	// WriteTimeout is the slow-client guard, so it stays finite and is NOT
	// sized for the slowest endpoint. It is sized for the slowest ordinary
	// one — /ask (ASK_TIMEOUT_SECONDS, default 60s, streamed as SSE) and a
	// cold-start /search, whose first query after a container restart has been
	// measured past the previous hardcoded 30s (issue #195, where that
	// deadline turned a slow search into an empty response). The briefing,
	// which can legitimately run for minutes, extends its own write deadline
	// per request instead (internal/api/briefing.go), so this value does not
	// have to grow with BRIEFING_TIMEOUT_SECONDS.
	if writeTimeout := cfg.HTTPWriteTimeout; writeTimeout <= time.Duration(cfg.AskTimeoutSeconds)*time.Second {
		slog.Warn("HTTP_WRITE_TIMEOUT_SECONDS is not above ASK_TIMEOUT_SECONDS — /ask responses may be truncated mid-stream",
			"http_write_timeout", writeTimeout.String(),
			"ask_timeout_seconds", cfg.AskTimeoutSeconds,
		)
	}

	httpServer := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      srv.Handler(),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: cfg.HTTPWriteTimeout,
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

// wireActionsAndBriefing conditionally registers the /api/v1/actions and
// /api/v1/briefing routes (plan Task 12). It is a pure function — no I/O —
// so the on/off wiring driven by cfg is unit-testable without a live
// Postgres connection (see main_test.go).
//
// Both feature flags default to false: leaving them unset means the routes
// simply do not exist in the router (404), which IS the rollback mechanism
// (spec §Rollback). BriefingEnabled only takes effect when ActionsAPIEnabled
// is also true — the briefing reads through the actions query path — and is
// additionally skipped when the LLM client is not configured (llmEnabled
// false), since api.Server's own llmClient != nil guard cannot detect
// "configured but disabled" (llm.New always returns a non-nil *Client).
func wireActionsAndBriefing(srv *api.Server, cfg *config.Config, lister api.ActionLister, setter api.ActionStateSetter, llmEnabled bool) *api.Server {
	if !cfg.ActionsAPIEnabled {
		return srv
	}
	srv = srv.WithActions(lister, setter)

	if !cfg.BriefingEnabled {
		return srv
	}
	if !llmEnabled {
		slog.Warn("briefing disabled — LLM client not configured despite BRIEFING_ENABLED=true")
		return srv
	}
	return srv.WithBriefing(briefing.NewCache(briefingCacheSize), cfg.BriefingMaxActions)
}

// wireEvidenceFeedback conditionally registers POST /api/v1/feedback/evidence
// (Part D). Like wireActionsAndBriefing it is a pure function so the on/off
// decision is unit-testable without a live Postgres connection.
//
// The flag defaults to false: an unset FEEDBACK_EVIDENCE_ENABLED leaves the
// route out of the router entirely (404), which is the rollback mechanism.
// A nil voter is also treated as "off" — turning the flag on without a store
// must not register a route whose only possible outcome is a nil-pointer
// panic.
//
// The api handler re-checks the same environment variable per request, so
// this wiring is necessary but not sufficient to expose the endpoint; both
// layers read the same truthy set (see config.envFlag).
func wireEvidenceFeedback(srv *api.Server, cfg *config.Config, voter api.EvidenceVoter) *api.Server {
	if !cfg.FeedbackEvidenceEnabled {
		return srv
	}
	if voter == nil {
		slog.Warn("evidence feedback disabled — no feedback store available despite FEEDBACK_EVIDENCE_ENABLED=true")
		return srv
	}
	return srv.WithEvidenceFeedback(voter)
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
