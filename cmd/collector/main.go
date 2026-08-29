package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/baekenough/second-brain/internal/api"
	"github.com/baekenough/second-brain/internal/collector"
	"github.com/baekenough/second-brain/internal/collector/extractor"
	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/graph"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/scheduler"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/setup"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/baekenough/second-brain/internal/telemetry"
	"github.com/baekenough/second-brain/internal/worker"
	"github.com/joho/godotenv"
)

// otelShutdownTimeout bounds how long the deferred telemetry shutdown may
// block process exit. A dead/unreachable Langfuse collector must never
// delay shutdown — see internal/telemetry's non-blocking guarantee.
const otelShutdownTimeout = 5 * time.Second

// extractionMaxDetachedWriteDuration bounds how long ExtractionWorker's
// detached persist+bookkeeping write phases can keep running after the
// shutdown context is cancelled (mirrors
// NoteEnrichmentWorker.MaxDetachedWriteDuration()'s formula: persist timeout
// + 2×bookkeeping timeout).
//
// This is a literal duration, not a call to an equivalent method on
// ExtractionWorker, because internal/worker/extraction_worker.go is out of
// scope for this change (concurrent work elsewhere in internal/ during this
// task) — see internal/worker/extraction_worker.go's own
// defaultExtractionPersistTimeout (30s) and
// defaultExtractionBookkeepingTimeout (10s) constants, which this value is
// computed from and which ExtractionWorkerConfig does not currently expose
// as configurable fields (only LLMTimeout affects the worker's other,
// cancellable, work budget).
const extractionMaxDetachedWriteDuration = 30*time.Second + 2*10*time.Second

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
	extractionFailureStore := store.NewExtractionFailureStore(pg)
	chunkStore := store.NewChunkStore(pg)
	entityStore := store.NewEntityStore(pg)

	// wg tracks long-running background goroutines that need a graceful drain
	// window on SIGTERM before the process exits (#65).
	var wg sync.WaitGroup

	// --- Extraction retry worker ---
	// Periodically re-attempts failed file extractions.
	// Remote-source failures (Discord/Slack attachments) are retried via
	// re-download: Discord uses the CDN URL stored in FilePath (no auth needed);
	// Slack uses the bot token. Sources with no matching backend fall back to
	// skip-and-debug-log, preserving pre-#72 behaviour.
	extractorReg := extractor.NewRegistry()
	retryWorker := worker.New(worker.Config{
		FailureStore: extractionFailureStore,
		DocStore:     docStore,
		Extractor:    worker.NewRegistryExtractor(extractorReg, 0),
		Refetcher:    worker.NewURLRefetcher(cfg.SlackBotToken),
		Interval:     time.Minute,
		BatchSize:    20,
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		retryWorker.Run(ctx)
	}()

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

	// --- LLM client (for summarization) ---
	llmClient := llm.New(llm.Config{
		BaseURL:     cfg.LLMAPIURL,
		Model:       cfg.LLMModel,
		APIKey:      cfg.LLMAPIKey,
		AuthFile:    cfg.LLMAuthFile,
		MaxTokens:   cfg.LLMMaxTokens,
		Temperature: cfg.LLMTemperature,
		Timeout:     time.Duration(cfg.LLMTimeoutSeconds) * time.Second,
		Thinking:    cfg.LLMThinking,
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
		Store:           docStore,
		LLM:             llmClient,
		Embedder:        embedClient,
		Interval:        cfg.SummarizerInterval,
		BatchSize:       cfg.SummarizerBatchSize,
		DocTimeout:      cfg.SummarizerDocTimeout,
		Concurrency:     cfg.SummarizerConcurrency,
		BackfillEnabled: &cfg.SummarizerBackfillEnabled,
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

	// --- Entity extraction worker (issue #77) ---
	// Entity extraction makes one LLM call per document and is therefore
	// OPT-IN. Set ENTITY_EXTRACTION_ENABLED=true (or =1) to enable both the
	// inline scheduler extraction and the backfill worker. Any other value
	// (including unset) leaves extraction disabled so that a vanilla deployment
	// incurs zero extra LLM cost.
	//
	// The server-side read path (search.WithEntityFetcher) is always active —
	// it is a cheap DB read and returns omitempty when no entities exist.
	ev := os.Getenv("ENTITY_EXTRACTION_ENABLED")
	entityExtractionEnabled := ev == "true" || ev == "1"
	if entityExtractionEnabled {
		slog.Info("entity extraction enabled via ENTITY_EXTRACTION_ENABLED=" + ev)
	} else {
		slog.Info("entity extraction disabled (set ENTITY_EXTRACTION_ENABLED=true to enable)")
	}
	entityWorker := worker.NewEntityWorker(worker.EntityWorkerConfig{
		Store:     docStore,
		Entities:  entityStore,
		LLM:       llmClient,
		Interval:  10 * time.Minute,
		BatchSize: 5,
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		if !entityExtractionEnabled {
			// Block until shutdown so the WaitGroup stays balanced.
			<-ctx.Done()
			return
		}
		entityWorker.Run(ctx)
	}()

	// --- Extraction pipeline (Part A, knowledge-graph/actions design) ---
	// One LLM call per document via ExtractionWorker, plus a SQL-only
	// structural-signal pass via StructuralSignalWorker — see
	// internal/worker/extraction_worker.go and internal/worker/structural_signals.go
	// package comments for the retry/deadline model.
	//
	// Both workers are gated behind the single EXTRACTION_ENABLED flag,
	// mirroring ENTITY_EXTRACTION_ENABLED's exact opt-in pattern above: a
	// vanilla deployment must incur zero extra LLM cost, and the summary
	// backfill currently in progress (45% complete) shares the same LLM
	// quota as ExtractionWorker, so nothing here may start firing at deploy
	// time without an explicit operator decision.
	//
	// Deviation from the design doc: docs/superpowers/plans/2026-08-17-extraction-pipeline.md's
	// Task 13 has StructuralSignalWorker run unconditionally (it is SQL-only,
	// no LLM call, so the doc reasons it is safe to always run). This
	// deployment gates it behind EXTRACTION_ENABLED as well, on explicit
	// instruction, so the two workers this pipeline introduces have exactly
	// one on/off switch operators need to reason about during backfill.
	relationStore := store.NewRelationStore(pg)
	actionStore := store.NewActionStore(pg)

	xv := os.Getenv("EXTRACTION_ENABLED")
	extractionEnabled := xv == "true" || xv == "1"
	if extractionEnabled {
		slog.Info("extraction pipeline enabled via EXTRACTION_ENABLED=" + xv)
	} else {
		slog.Info("extraction pipeline disabled (set EXTRACTION_ENABLED=true to enable)")
	}

	// Interval/BatchSize are environment-configurable, mirroring
	// SummarizerWorker's cfg.SummarizerInterval/cfg.SummarizerBatchSize
	// pattern (see envDuration/envInt below) — unlike EntityWorker's
	// hardcoded Interval/BatchSize a few lines above, which throttles it to
	// ~30 docs/hour and is out of scope for this change.
	extractionWorker := worker.NewExtractionWorker(worker.ExtractionWorkerConfig{
		Store:         docStore,
		Entities:      entityStore,
		Relations:     relationStore,
		Actions:       actionStore,
		LLM:           llmClient,
		UserAddresses: cfg.UserEmailAddresses,
		Interval:      envDuration("EXTRACTION_INTERVAL", 5*time.Minute),
		BatchSize:     envInt("EXTRACTION_BATCH_SIZE", 5),
		LLMTimeout:    time.Duration(cfg.LLMTimeoutSeconds) * time.Second,
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		if !extractionEnabled {
			// Block until shutdown so the WaitGroup stays balanced.
			<-ctx.Done()
			return
		}
		extractionWorker.Run(ctx)
	}()

	structuralSignalWorker := worker.NewStructuralSignalWorker(worker.StructuralSignalWorkerConfig{
		Store:         worker.NewPgStructuralSignalLister(pg.Pool()),
		Actions:       actionStore,
		UserAddresses: cfg.UserEmailAddresses,
		Interval:      envDuration("STRUCTURAL_SIGNAL_INTERVAL", 10*time.Minute),
		BatchSize:     envInt("STRUCTURAL_SIGNAL_BATCH_SIZE", 500),
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		if !extractionEnabled {
			// Block until shutdown so the WaitGroup stays balanced.
			<-ctx.Done()
			return
		}
		structuralSignalWorker.Run(ctx)
	}()

	// --- Graph projection worker (Part B) ---
	// Gated by its OWN flag, not EXTRACTION_ENABLED: extraction is a decision
	// about LLM spend, projection is not, so folding them into one switch would
	// remove the ability to turn off just one of them. Default is off.
	//
	// Neo4j is a derived projection — a connection failure must never take the
	// collector down with it. A failure here disables projection for the
	// lifetime of this process (restart the collector once Neo4j is up); that
	// is the deliberate counterpart to not declaring depends_on: neo4j in
	// compose.
	gv := os.Getenv("GRAPH_PROJECTION_ENABLED")
	graphEnabled := gv == "true" || gv == "1"
	wg.Add(1)
	go func() {
		defer wg.Done()
		if !graphEnabled {
			// Block until shutdown so the WaitGroup stays balanced.
			slog.Info("graph projection disabled (set GRAPH_PROJECTION_ENABLED=true to enable)")
			<-ctx.Done()
			return
		}
		gc, err := graph.New(ctx, graph.Config{
			URI:      os.Getenv("NEO4J_URI"),
			Username: os.Getenv("NEO4J_USERNAME"),
			Password: os.Getenv("NEO4J_PASSWORD"),
		})
		if err != nil {
			slog.Error("graph projection disabled — neo4j unavailable", "error", err)
			<-ctx.Done()
			return
		}
		defer func() { _ = gc.Close(context.Background()) }()
		worker.NewGraphProjectionWorker(worker.GraphProjectionWorkerConfig{
			Source:     store.NewGraphSource(pg),
			Projector:  graph.NewProjector(gc),
			Interval:   envDuration("GRAPH_PROJECTION_INTERVAL", 5*time.Minute),
			BatchSize:  envInt("GRAPH_PROJECTION_BATCH_SIZE", 500),
			ResetToken: os.Getenv("GRAPH_PROJECTION_RESET_TOKEN"),
		}).Run(ctx)
	}()

	// --- Note enrichment worker (Capture backend) ---
	// Runs unconditionally, like retryWorker and summarizerWorker — unlike
	// entity extraction, there is no feature-flag gate here because Capture
	// notes are only created via an explicit user action (POST
	// /api/v1/notes), not a bulk backfill over the existing 46,000+ document
	// corpus. The worker itself idles gracefully when llmClient.Enabled() is
	// false (see NoteEnrichmentWorker.Run), matching EntityWorker's pattern.
	//
	// LLMTimeout is passed explicitly rather than left to the worker's own
	// default: the worker derives its per-note work deadline FROM the LLM
	// client's timeout (see note_enrichment_worker.go's deadline model), and
	// passing the same cfg value used to construct llmClient above is what
	// keeps that relationship a wiring fact instead of two constants that
	// happen to agree today.
	noteEnrichmentWorker := worker.NewNoteEnrichmentWorker(worker.NoteEnrichmentWorkerConfig{
		Store:      docStore,
		Insights:   worker.NewInsightSaver(docStore, chunkStore, embedClient),
		Entities:   entityStore,
		LLM:        llmClient,
		Interval:   5 * time.Minute,
		BatchSize:  5,
		LLMTimeout: time.Duration(cfg.LLMTimeoutSeconds) * time.Second,
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		noteEnrichmentWorker.Run(ctx)
	}()

	// drainTimeout is the maximum time to wait for in-flight ticks to finish
	// after the shutdown signal is received.
	//
	// It is derived from the workers that DETACH writes from the shutdown
	// context (context.WithoutCancel) rather than from a bare constant. Those
	// writes keep running after SIGTERM by design, so the drain window has to
	// outlast them or the process exits mid-write — defeating the whole point
	// of detaching them.
	//
	//   - SummarizerWorker detaches a whole document (LLM call + embed +
	//     UpdateSummary) and bounds it by SummarizerDocTimeout (default 30 s;
	//     #170, was a fixed 8 s "maxTickDuration" tuned for a fast local LLM).
	//   - NoteEnrichmentWorker detaches only its WRITE phases — its LLM call
	//     is cancelled by the shutdown context — so it contributes
	//     MaxDetachedWriteDuration() (persist + two bookkeeping budgets),
	//     not its much larger LLM work budget. The status write it protects
	//     is the one that enforces the enrichment retry cap.
	//
	//   - ExtractionWorker (when EXTRACTION_ENABLED) detaches only its
	//     persist and bookkeeping write phases — its LLM call is cancelled by
	//     the shutdown context, same shape as NoteEnrichmentWorker — so it
	//     contributes extractionMaxDetachedWriteDuration, not its larger LLM
	//     work budget.
	//
	// The +10s margin covers goroutine scheduling/cleanup overhead on top of
	// those budgets. The entity-extraction, extraction-retry, and
	// structural-signal workers use the raw (non-detached) shutdown context
	// directly, so they are cancelled near-instantly and do not drive this
	// timeout.
	drainTimeout := cfg.SummarizerDocTimeout
	if d := noteEnrichmentWorker.MaxDetachedWriteDuration(); d > drainTimeout {
		drainTimeout = d
	}
	if extractionEnabled && extractionMaxDetachedWriteDuration > drainTimeout {
		drainTimeout = extractionMaxDetachedWriteDuration
	}
	drainTimeout += 10 * time.Second
	if drainTimeout < 15*time.Second {
		drainTimeout = 15 * time.Second
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
		collector.NewGmailCollector(cfg),
		collector.NewCalendarCollector(cfg).WithCancellationStore(docStore),
		collector.NewSMSCollector(cfg.SMSSourceDir, cfg.SMSMaxFileBytes).
			WithNumberHashingEnabled(cfg.PIINumberHashingEnabled),
		collector.NewWhisperCollector(cfg),
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
	// SecretaryCollector was decommissioned in #101 and removed in #151.
	// The SECRETARY_DB_PATH env var and model.SourceSecretary constant are
	// intentionally kept for backward-compatibility with existing DB rows.
	if cfg.LLMMemoryDBPath != "" {
		collectors = append(collectors, collector.NewLLMMemoryCollector(cfg.LLMMemoryDBPath))
	}

	// --- Scheduler ---
	// WithEntityExtraction is only called when extraction is explicitly enabled
	// so that the inline per-document LLM call is never made in a default deployment.
	sched := scheduler.New(docStore, embedClient, collectors...).
		WithChunkStore(chunkStore).
		WithInstance(cfg.CollectorInstance).
		WithCutover(cfg.CollectorCutover).
		WithDeletionRatioOverride(cfg.DeletionRatioOverride)
	if entityExtractionEnabled {
		sched = sched.WithEntityExtraction(entityStore, llmClient)
	}
	if cfg.DeletionRatioOverride {
		slog.Warn("deletion ratio override active — 50% guard bypassed (DELETION_RATIO_OVERRIDE=true)")
	}
	slog.Info("collector instance", "id", cfg.CollectorInstance)
	if err := sched.Register(cfg.CollectInterval, cfg.CollectIntervalPerSource); err != nil {
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

	// --- Freshness checker (#137 + #159) ---
	//
	// Monitors two layers of health:
	//   1. collection_log freshness — catches failures in pull-based collectors
	//      that write a log entry on every run.
	//   2. document-level freshness — catches silent push-ingest failures (e.g.
	//      SMS via Android push app) that bypass collection_log entirely. This
	//      was the dead-code gap from #137 now wired here as part of #159.
	//
	// Runs immediately after start, then on every FreshnessCheckInterval tick.
	// Errors from the store are logged but never crash the scheduler — the collector
	// continues even if the freshness check cannot reach the database momentarily.
	freshnessChecker := api.NewFreshnessChecker(docStore, cfg.AlertWebhookURL, 2*time.Hour, 3).
		WithRetiredSources(cfg.RetiredSources...).
		WithDocumentFreshness(docStore, map[string]time.Duration{
			"sms": cfg.SMSFreshnessMaxAge,
		})
	go func() {
		// Run once immediately so operators get fast feedback on startup.
		if err := freshnessChecker.Check(ctx); err != nil {
			slog.Warn("freshness checker: initial check failed", "error", err)
		}
		ticker := time.NewTicker(cfg.FreshnessCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := freshnessChecker.Check(ctx); err != nil {
					slog.Warn("freshness checker: periodic check failed", "error", err)
				}
			}
		}
	}()

	slog.Info("collector daemon started",
		"freshness_check_interval", cfg.FreshnessCheckInterval,
		"sms_freshness_max_age", cfg.SMSFreshnessMaxAge,
	)
	<-ctx.Done()

	// Bounded drain: give in-flight goroutines (summarizer, retry worker) time
	// to finish their current tick before the process exits.
	//
	// SummarizerWorker bounds any in-flight document to SummarizerDocTimeout
	// (default 30 s), and drainTimeout is derived from that same value above,
	// so in-flight documents always complete before the drain window closes
	// (#65, #170).
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

// envDuration parses a time.Duration from the environment variable key,
// falling back to def when unset or invalid. Mirrors
// internal/config.summarizerInterval's convention (parse, log a warning on
// an invalid value, never fail startup) — duplicated here rather than added
// to internal/config because internal/ is out of scope for this change
// (concurrent work elsewhere in internal/ during this task).
func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		slog.Warn("config: invalid duration env var, using default",
			"key", key, "value", v, "default", def, "error", err)
		return def
	}
	return d
}

// envInt parses an int from the environment variable key, falling back to
// def when unset or invalid. See envDuration's doc comment for why this
// lives here instead of internal/config.
func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		slog.Warn("config: invalid int env var, using default",
			"key", key, "value", v, "default", def, "error", err)
		return def
	}
	return n
}
