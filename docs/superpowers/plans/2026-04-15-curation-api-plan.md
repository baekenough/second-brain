# Curation API Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** Refactor second-brain into a dual-binary architecture (API server + collector daemon) with LLM curation and Korean search support.

**Architecture:** Split cmd/server into cmd/server (API only) and cmd/collector (daemon). Add pg_bigm for Korean 2-gram search. Add internal/curation package for LLM re-ranking. Remove Discord bot response code.

**Tech Stack:** Go 1.25, chi/v5, pgx/v5, pgvector, pg_bigm, robfig/cron/v3, Docker multi-stage

---

## Task 1: pg_bigm Migration

**Files:**
- Create: `migrations/006_bigm.sql`

- [ ] **Step 1: Write the migration file**

```sql
-- 006_bigm.sql
-- Enable pg_bigm extension for Korean 2-gram partial matching.
-- pg_bigm must be installed in the PostgreSQL image (see Dockerfile/compose).

CREATE EXTENSION IF NOT EXISTS pg_bigm;

-- Documents table: 2-gram indexes for Korean partial matching
CREATE INDEX IF NOT EXISTS idx_documents_content_bigm ON documents USING gin (content gin_bigm_ops);
CREATE INDEX IF NOT EXISTS idx_documents_title_bigm ON documents USING gin (title gin_bigm_ops);

-- Chunks table: 2-gram index for chunk-level Korean matching
CREATE INDEX IF NOT EXISTS idx_chunks_content_bigm ON chunks USING gin (content gin_bigm_ops);
```

- [ ] **Step 2: Verify migration loads**

Run: `go run ./cmd/server/` (it auto-applies migrations on startup)
Expected: Migration 006 applied without errors. Check logs for "applied migration 006_bigm.sql".

Note: pg_bigm extension must be available in the PostgreSQL image. For local dev, install via `apt-get install postgresql-16-pg-bigm` or use a custom Docker image. If extension is not available, the migration will fail — this is expected and will be resolved in Task 7 (Docker setup).

- [ ] **Step 3: Commit**

```bash
git add migrations/006_bigm.sql
git commit -m "feat: add pg_bigm migration for Korean 2-gram search"
```

---

## Task 2: Collector Binary Entry Point

**Files:**
- Create: `cmd/collector/main.go`
- Modify: `internal/config/config.go`

- [ ] **Step 1: Write collector config helper**

Add to `internal/config/config.go` — a new function `LoadCollector` that loads only collector-relevant config (no API_KEY, no PORT, no Discord bot response config):

```go
// LoadCollector reads configuration for the collector daemon.
// It excludes server-only fields (PORT, API_KEY) and Discord bot response fields.
func LoadCollector() (*Config, error) {
	cfg, err := Load()
	if err != nil {
		return nil, err
	}
	// Collector doesn't serve HTTP — clear server-only fields.
	cfg.Port = ""
	cfg.APIKey = ""
	return cfg, nil
}
```

- [ ] **Step 2: Write cmd/collector/main.go**

```go
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
		slog.Error("collector startup failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
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

	// Run migrations (collector also applies migrations to ensure schema is current).
	migrationsDir := migrationsPath()
	if err := pg.RunMigrations(ctx, migrationsDir); err != nil {
		return err
	}

	docStore := store.NewDocumentStore(pg)
	extractionFailureStore := store.NewExtractionFailureStore(pg)
	chunkStore := store.NewChunkStore(pg)

	// --- Extraction retry worker ---
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
		slog.Info("embedding API not configured — documents stored without embeddings")
	}

	// --- Collectors ---
	collectors := []collector.Collector{
		collector.NewSlackCollector(cfg.SlackBotToken, cfg.SlackTeamID),
		collector.NewGitHubCollector(cfg.GitHubToken, cfg.GitHubOrg),
		collector.NewGDriveCollector(cfg.GDriveCredentialsJSON),
	}
	if cfg.FilesystemEnabled && cfg.FilesystemPath != "" {
		driveExporter, err := collector.NewDriveExporter(ctx)
		if err != nil {
			slog.Warn("filesystem: drive exporter init failed", "error", err)
			driveExporter = nil
		}
		collectors = append(collectors,
			collector.NewFilesystemCollectorWithDriveExport(cfg.FilesystemPath, driveExporter))
	}

	// --- Scheduler ---
	sched := scheduler.New(docStore, embedClient, collectors...).WithChunkStore(chunkStore)
	if err := sched.Register(cfg.CollectInterval); err != nil {
		return err
	}
	sched.Start()
	defer sched.Stop()

	// --- Slack channel watcher ---
	slackCol := collector.NewSlackCollector(cfg.SlackBotToken, cfg.SlackTeamID)
	if slackCol.Enabled() {
		watcher := collector.NewSlackChannelWatcher(slackCol, docStore, embedClient, 60*time.Second)
		go watcher.Run(ctx)
	}

	slog.Info("collector daemon started", "interval", cfg.CollectInterval)

	// Block until signal.
	<-ctx.Done()
	slog.Info("collector daemon shutting down...")
	return nil
}

func migrationsPath() string {
	if dir := os.Getenv("MIGRATIONS_DIR"); dir != "" {
		return dir
	}
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return "migrations"
	}
	if !filepath.IsAbs(filename) {
		return "migrations"
	}
	root := filepath.Join(filepath.Dir(filename), "..", "..")
	return filepath.Join(root, "migrations")
}
```

- [ ] **Step 3: Verify collector binary builds**

Run: `go build ./cmd/collector/`
Expected: Binary compiles without errors.

- [ ] **Step 4: Commit**

```bash
git add cmd/collector/main.go internal/config/config.go
git commit -m "feat: add collector daemon binary entry point"
```

---

## Task 3: Strip Collector Logic from Server

**Files:**
- Modify: `cmd/server/main.go`
- Modify: `internal/api/router.go`

- [ ] **Step 1: Remove collector and scheduler code from cmd/server/main.go**

Replace cmd/server/main.go with a server-only version. Remove all collector registration, scheduler creation, Discord gateway, Slack watcher. Keep: database, stores, embed client, search service, LLM client (for curation), HTTP server.

```go
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

	migrationsDir := migrationsPath()
	if err := pg.RunMigrations(ctx, migrationsDir); err != nil {
		return err
	}

	docStore := store.NewDocumentStore(pg)
	chunkStore := store.NewChunkStore(pg)
	feedbackStore := store.NewFeedbackStore(pg)
	evalStore := store.NewEvalStore(pg)

	// --- Embedding client ---
	embedClient := search.NewEmbedClient(cfg.EmbeddingAPIURL, cfg.EmbeddingAPIKey, cfg.CliProxyAuthFile, cfg.EmbeddingModel)

	// --- Search service ---
	searchSvc := search.NewService(docStore, embedClient).WithChunkStore(chunkStore)

	// --- LLM client (curation) ---
	llmClient := llm.New(llm.Config{
		BaseURL:     cfg.LLMAPIURL,
		Model:       cfg.LLMModel,
		APIKey:      cfg.LLMAPIKey,
		AuthFile:    cfg.LLMAuthFile,
		MaxTokens:   cfg.LLMMaxTokens,
		Temperature: cfg.LLMTemperature,
	}, nil)

	// --- HTTP server ---
	srv := api.NewServer(docStore, searchSvc, feedbackStore, evalStore, llmClient, cfg.FilesystemPath, cfg.APIKey)
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
	return httpServer.Shutdown(shutdownCtx)
}

func migrationsPath() string {
	if dir := os.Getenv("MIGRATIONS_DIR"); dir != "" {
		return dir
	}
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return "migrations"
	}
	if !filepath.IsAbs(filename) {
		return "migrations"
	}
	root := filepath.Join(filepath.Dir(filename), "..", "..")
	return filepath.Join(root, "migrations")
}
```

- [ ] **Step 2: Update api.Server to remove scheduler dependency**

Modify `internal/api/router.go`:

1. Remove `scheduler` field from Server struct
2. Remove `scheduler` parameter from NewServer
3. Remove `collector` import
4. Remove `discordMetrics` field and `SetDiscordMetrics` method
5. Add `llmClient` field (for curation, Task 5)
6. Remove trigger/collect endpoints from router

Updated NewServer signature:

```go
func NewServer(
	docs DocumentStore,
	svc *search.Service,
	feedback FeedbackRecorder,
	eval EvalExporter,
	llmClient llm.Completer,
	filesystemPath string,
	apiKey string,
) *Server {
	return &Server{
		docs:           docs,
		search:         svc,
		feedback:       feedback,
		eval:           eval,
		llmClient:      llmClient,
		filesystemPath: filesystemPath,
		apiKey:         apiKey,
	}
}
```

Updated Server struct:

```go
type Server struct {
	docs           DocumentStore
	search         *search.Service
	feedback       FeedbackRecorder
	eval           EvalExporter
	llmClient      llm.Completer
	filesystemPath string
	apiKey         string
}
```

Updated Handler() — remove trigger/collect routes, remove discord metrics from stats:

```go
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RealIP)
	r.Use(requestLogger)
	r.Use(recoverer)

	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	r.Group(func(r chi.Router) {
		r.Use(requireAPIKey(s.apiKey))

		r.Post("/api/v1/search", s.searchHandler)
		r.Get("/api/v1/search", s.searchGetHandler) // GET alias for AI agents

		r.Get("/api/v1/documents", s.listDocumentsHandler)
		r.Get("/api/v1/documents/{id}", s.getDocumentHandler)
		r.Get("/api/v1/documents/{id}/raw", s.getDocumentRawHandler)

		r.Get("/api/v1/sources", s.listSourcesHandler)

		r.Get("/api/v1/stats", s.statsHandler)
		r.Get("/api/v1/stats/baseline", s.baselineStatsHandler)

		r.Post("/api/v1/feedback", s.feedbackHandler)

		r.Get("/api/v1/eval/export", s.evalExportHandler)
	})

	return r
}
```

- [ ] **Step 3: Remove collect_channel.go and update source.go**

Delete `internal/api/collect_channel.go` (the `collectSlackChannelHandler` — moved to collector).

Update `internal/api/source.go`: remove `triggerCollectHandler` function. Update `listSourcesHandler` to read sources from database instead of scheduler:

```go
func (s *Server) listSourcesHandler(w http.ResponseWriter, r *http.Request) {
	counts, err := s.docs.CountBySource(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list sources")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"sources": counts,
	})
}
```

- [ ] **Step 4: Update stats handlers**

Modify `internal/api/stats.go`: remove discord metrics references, remove scheduler timing references. The stats endpoint should read from the database (collection_log table) instead of the scheduler.

- [ ] **Step 5: Fix all compilation errors**

Run: `go build ./cmd/server/`
Expected: Compiles without errors. Fix any remaining import or reference issues.

- [ ] **Step 6: Run tests**

Run: `go test ./internal/api/...`
Expected: Existing tests pass (some may need updates for new NewServer signature).

- [ ] **Step 7: Commit**

```bash
git add cmd/server/main.go internal/api/
git commit -m "refactor: strip collector logic from API server"
```

---

## Task 4: pg_bigm Search Integration

**Files:**
- Modify: `internal/store/document.go` (add bigm query)
- Create: `internal/store/bigm.go`
- Create: `internal/store/bigm_test.go`

- [ ] **Step 1: Write the failing test for bigm search**

Create `internal/store/bigm_test.go`:

```go
package store

import (
	"testing"
)

func TestBuildBigmQuery(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  string
	}{
		{
			name:  "korean query",
			query: "배포 완료",
			want:  "배포 완료",
		},
		{
			name:  "empty query",
			query: "",
			want:  "",
		},
		{
			name:  "english query",
			query: "deploy complete",
			want:  "deploy complete",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildBigmQuery(tt.query)
			if got != tt.want {
				t.Errorf("BuildBigmQuery(%q) = %q, want %q", tt.query, got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestBuildBigmQuery -v`
Expected: FAIL — `BuildBigmQuery` not defined.

- [ ] **Step 3: Write bigm.go implementation**

Create `internal/store/bigm.go`:

```go
package store

// BuildBigmQuery returns the query string for pg_bigm LIKE search.
// pg_bigm uses 2-gram indexing so no special transformation is needed —
// a plain LIKE '%query%' leverages the gin_bigm_ops index automatically.
func BuildBigmQuery(query string) string {
	return query
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -run TestBuildBigmQuery -v`
Expected: PASS

- [ ] **Step 5: Integrate bigm into document search query**

Modify the SQL search query in `internal/store/document.go` to include a pg_bigm LIKE clause as an additional signal in the RRF merge. The existing `Search` method builds a SQL query — add a third CTE for bigm matching:

```sql
bigm AS (
    SELECT id, 1.0 AS rank
    FROM documents
    WHERE content LIKE '%' || $bigm_query || '%'
      AND status = 'active'
    LIMIT $limit
)
```

Then include `bigm` in the RRF merge alongside `fts` and `vec` CTEs.

- [ ] **Step 6: Run full test suite**

Run: `go test ./internal/store/... -v`
Expected: All tests pass.

- [ ] **Step 7: Commit**

```bash
git add internal/store/bigm.go internal/store/bigm_test.go internal/store/document.go
git commit -m "feat: integrate pg_bigm 2-gram search for Korean support"
```

---

## Task 5: Curation Package

**Files:**
- Create: `internal/curation/curation.go`
- Create: `internal/curation/curation_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/curation/curation_test.go`:

```go
package curation

import (
	"context"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
)

type mockCompleter struct {
	response string
	err      error
}

func (m *mockCompleter) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	return m.response, m.err
}

func (m *mockCompleter) Enabled() bool { return true }

func TestCurator_Curate_ReturnsOriginal(t *testing.T) {
	// When LLM returns a valid JSON response, Curate should
	// return results with both summary and original data.
	llmResponse := `[{"index":0,"summary":"Onboarding guide overview","relevance":0.95}]`
	curator := New(&mockCompleter{response: llmResponse})

	results := []*model.SearchResult{
		{
			Document: model.Document{
				Title:   "Onboarding Guide",
				Content: "Welcome to the team...",
			},
			Score: 0.8,
		},
	}

	curated, err := curator.Curate(context.Background(), "onboarding", results)
	if err != nil {
		t.Fatalf("Curate() error = %v", err)
	}
	if len(curated) != 1 {
		t.Fatalf("Curate() returned %d results, want 1", len(curated))
	}
	if curated[0].Summary == "" {
		t.Error("Curate() result has empty summary")
	}
	if curated[0].Original.Title != "Onboarding Guide" {
		t.Errorf("Curate() original.title = %q, want %q", curated[0].Original.Title, "Onboarding Guide")
	}
	if curated[0].Original.Content != "Welcome to the team..." {
		t.Error("Curate() original content was modified — must be preserved exactly")
	}
}

func TestCurator_Curate_NilLLM_ReturnsPassthrough(t *testing.T) {
	// When LLM is nil, Curate should return results as-is with no summary.
	curator := New(nil)

	results := []*model.SearchResult{
		{
			Document: model.Document{Title: "Doc1", Content: "content1"},
			Score:    0.9,
		},
	}

	curated, err := curator.Curate(context.Background(), "query", results)
	if err != nil {
		t.Fatalf("Curate() error = %v", err)
	}
	if len(curated) != 1 {
		t.Fatalf("Curate() returned %d results, want 1", len(curated))
	}
	if curated[0].Original.Title != "Doc1" {
		t.Errorf("passthrough original.title = %q, want %q", curated[0].Original.Title, "Doc1")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/curation/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write curation.go implementation**

Create `internal/curation/curation.go`:

```go
// Package curation provides LLM-based search result curation: re-ranking,
// lightweight summarization, and noise filtering. Original data is always
// preserved untouched.
package curation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"

	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
)

// CuratedResult wraps a search result with an LLM-generated summary
// and preserves the original document data untouched.
type CuratedResult struct {
	Summary         string          `json:"summary"`
	Original        model.Document  `json:"original"`
	Relevance       float64         `json:"relevance"`
	RelevanceReason string          `json:"relevance_reason,omitempty"`
}

// llmRankEntry is the JSON structure we expect back from the LLM.
type llmRankEntry struct {
	Index           int     `json:"index"`
	Summary         string  `json:"summary"`
	Relevance       float64 `json:"relevance"`
	RelevanceReason string  `json:"relevance_reason,omitempty"`
}

// Curator uses an LLM to re-rank and summarize search results.
type Curator struct {
	llm llm.Completer
}

// New returns a Curator. If llm is nil, Curate returns passthrough results.
func New(llm llm.Completer) *Curator {
	return &Curator{llm: llm}
}

// Curate re-ranks and lightly summarizes the given search results.
// Original data is ALWAYS preserved — never modified or compressed.
// When the LLM is nil or disabled, results are returned as-is.
func (c *Curator) Curate(ctx context.Context, query string, results []*model.SearchResult) ([]CuratedResult, error) {
	// Passthrough when no LLM is available.
	if c.llm == nil || !c.llm.Enabled() {
		return passthrough(results), nil
	}

	// Build the LLM prompt.
	systemPrompt := `You are a search result curator. Given a query and search results, you must:
1. Re-rank results by relevance to the query (most relevant first)
2. Generate a brief summary (1-2 sentences) for each result
3. Assign a relevance score (0.0 to 1.0)
4. Filter out clearly irrelevant results (relevance < 0.3)

IMPORTANT: Do NOT modify original content. Summaries should be lightweight — preserve meaning.
For Korean content, write summaries in Korean.

Respond with a JSON array only, no markdown fencing:
[{"index": 0, "summary": "...", "relevance": 0.95, "relevance_reason": "..."}]

"index" refers to the position in the input results array (0-indexed).`

	// Build input for LLM.
	type inputItem struct {
		Index   int    `json:"index"`
		Title   string `json:"title"`
		Content string `json:"content"`
		Source  string `json:"source"`
	}
	items := make([]inputItem, len(results))
	for i, r := range results {
		// Truncate content to avoid token overflow.
		content := r.Content
		if len(content) > 2000 {
			content = content[:2000] + "..."
		}
		items[i] = inputItem{
			Index:   i,
			Title:   r.Title,
			Content: content,
			Source:  string(r.SourceType),
		}
	}

	userPrompt := fmt.Sprintf("Query: %s\n\nResults:\n%s",
		query, mustMarshal(items))

	response, err := c.llm.Complete(ctx, systemPrompt, userPrompt)
	if err != nil {
		slog.Warn("curation: LLM call failed, returning passthrough",
			"error", err)
		return passthrough(results), nil
	}

	// Parse LLM response.
	var rankings []llmRankEntry
	if err := json.Unmarshal([]byte(response), &rankings); err != nil {
		slog.Warn("curation: failed to parse LLM response, returning passthrough",
			"error", err, "response", response)
		return passthrough(results), nil
	}

	// Sort by relevance descending.
	sort.Slice(rankings, func(i, j int) bool {
		return rankings[i].Relevance > rankings[j].Relevance
	})

	// Build curated results, preserving original data.
	curated := make([]CuratedResult, 0, len(rankings))
	for _, rank := range rankings {
		if rank.Index < 0 || rank.Index >= len(results) {
			continue
		}
		if rank.Relevance < 0.3 {
			continue // filter noise
		}
		curated = append(curated, CuratedResult{
			Summary:         rank.Summary,
			Original:        results[rank.Index].Document,
			Relevance:       rank.Relevance,
			RelevanceReason: rank.RelevanceReason,
		})
	}

	return curated, nil
}

// passthrough converts search results to curated results without LLM processing.
func passthrough(results []*model.SearchResult) []CuratedResult {
	out := make([]CuratedResult, len(results))
	for i, r := range results {
		out[i] = CuratedResult{
			Original:  r.Document,
			Relevance: r.Score,
		}
	}
	return out
}

func mustMarshal(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/curation/ -v`
Expected: All tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/curation/
git commit -m "feat: add curation package for LLM re-ranking and summarization"
```

---

## Task 6: Wire Curation into Search Handler

**Files:**
- Modify: `internal/api/search.go`
- Modify: `internal/api/router.go`

- [ ] **Step 1: Add curated parameter to search handler**

Update `internal/api/search.go` — add `curated` field to searchRequest and wire up the curation logic:

```go
type searchRequest struct {
	Query              string             `json:"query"`
	SourceType         *model.SourceType  `json:"source_type"`
	ExcludeSourceTypes []model.SourceType `json:"exclude_source_types"`
	Limit              int                `json:"limit"`
	IncludeDeleted     bool               `json:"include_deleted"`
	Sort               string             `json:"sort"`
	UseHyDE            bool               `json:"use_hyde,omitempty"`
	Curated            bool               `json:"curated,omitempty"`
}

func (s *Server) searchHandler(w http.ResponseWriter, r *http.Request) {
	var req searchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Query == "" {
		writeError(w, http.StatusBadRequest, "query field is required")
		return
	}

	q := model.SearchQuery{
		Query:              req.Query,
		SourceType:         req.SourceType,
		ExcludeSourceTypes: req.ExcludeSourceTypes,
		Limit:              req.Limit,
		IncludeDeleted:     req.IncludeDeleted,
		Sort:               req.Sort,
		UseHyDE:            req.UseHyDE,
	}

	start := time.Now()
	results, err := s.search.Search(r.Context(), q)
	if err != nil {
		slog.Error("search: query failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	if req.Curated {
		curator := curation.New(s.llmClient)
		curated, err := curator.Curate(r.Context(), req.Query, results)
		if err != nil {
			slog.Error("curation: failed", "error", err)
			writeError(w, http.StatusInternalServerError, "curation failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"results": curated,
			"count":   len(curated),
			"query":   req.Query,
			"curated": true,
			"took_ms": time.Since(start).Milliseconds(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"results": results,
		"count":   len(results),
		"total":   len(results),
		"query":   req.Query,
		"took_ms": time.Since(start).Milliseconds(),
	})
}
```

- [ ] **Step 2: Add GET /api/v1/search handler**

Add to `internal/api/search.go`:

```go
// searchGetHandler handles GET /api/v1/search for AI agent convenience.
// Query parameters: q (required), source_type, limit, curated, use_hyde.
func (s *Server) searchGetHandler(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		writeError(w, http.StatusBadRequest, "q parameter is required")
		return
	}

	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}

	var srcType *model.SourceType
	if v := r.URL.Query().Get("source_type"); v != "" {
		st := model.SourceType(v)
		srcType = &st
	}

	curated := r.URL.Query().Get("curated") == "true"
	useHyDE := r.URL.Query().Get("use_hyde") == "true"

	q := model.SearchQuery{
		Query:      query,
		SourceType: srcType,
		Limit:      limit,
		UseHyDE:    useHyDE,
	}

	start := time.Now()
	results, err := s.search.Search(r.Context(), q)
	if err != nil {
		slog.Error("search: query failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	if curated {
		curator := curation.New(s.llmClient)
		curatedResults, err := curator.Curate(r.Context(), query, results)
		if err != nil {
			slog.Error("curation: failed", "error", err)
			writeError(w, http.StatusInternalServerError, "curation failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"results": curatedResults,
			"count":   len(curatedResults),
			"query":   query,
			"curated": true,
			"took_ms": time.Since(start).Milliseconds(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"results": results,
		"count":   len(results),
		"total":   len(results),
		"query":   query,
		"took_ms": time.Since(start).Milliseconds(),
	})
}
```

- [ ] **Step 3: Add imports**

Add to `internal/api/search.go`:

```go
import (
	"strconv"
	"github.com/baekenough/second-brain/internal/curation"
)
```

- [ ] **Step 4: Build and test**

Run: `go build ./cmd/server/ && go test ./internal/api/... -v`
Expected: Compiles and tests pass.

- [ ] **Step 5: Commit**

```bash
git add internal/api/search.go internal/api/router.go
git commit -m "feat: add curated search parameter and GET search endpoint"
```

---

## Task 7: Docker Multi-Target Build

**Files:**
- Modify: `Dockerfile`
- Modify: `docker-compose.yml`

- [ ] **Step 1: Update Dockerfile for multi-target**

Replace `Dockerfile` with:

```dockerfile
# syntax=docker/dockerfile:1.7
# =============================================================================
# second-brain — Multi-target Dockerfile
# Builds: server (API) and collector (daemon)
# =============================================================================

# --- Stage 1: Dependencies ---
FROM golang:1.24-alpine AS deps
WORKDIR /workspace
COPY go.mod go.sum ./
RUN GOTOOLCHAIN=auto go mod download

# --- Stage 2: Builder ---
FROM golang:1.24-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /workspace
COPY --from=deps /go/pkg/mod /go/pkg/mod
COPY --from=deps /go/pkg/mod/cache /go/pkg/mod/cache
COPY . .

# --- Build server ---
FROM builder AS build-server
RUN GOTOOLCHAIN=auto CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# --- Build collector ---
FROM builder AS build-collector
RUN GOTOOLCHAIN=auto CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/collector ./cmd/collector

# --- Runtime base ---
FROM alpine:3.21 AS runtime-base
LABEL org.opencontainers.image.source="https://github.com/baekenough/second-brain"
RUN apk add --no-cache ca-certificates tzdata wget
RUN addgroup -g 10001 -S appgroup \
 && adduser -u 10001 -S -G appgroup -H appuser

# --- Server target ---
FROM runtime-base AS server
LABEL org.opencontainers.image.title="second-brain-server"
COPY --from=build-server /out/server /app/server
COPY --from=builder /workspace/migrations /app/migrations
RUN mkdir -p /data/drive && chown appuser:appgroup /data/drive
WORKDIR /app
USER appuser:appgroup
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://localhost:8080/health || exit 1
VOLUME ["/data/drive"]
ENTRYPOINT ["/app/server"]

# --- Collector target ---
FROM runtime-base AS collector
LABEL org.opencontainers.image.title="second-brain-collector"
COPY --from=build-collector /out/collector /app/collector
COPY --from=builder /workspace/migrations /app/migrations
RUN mkdir -p /data/drive && chown appuser:appgroup /data/drive
WORKDIR /app
USER appuser:appgroup
VOLUME ["/data/drive"]
ENTRYPOINT ["/app/collector"]
```

- [ ] **Step 2: Update docker-compose.yml**

```yaml
services:
  postgres:
    image: pgvector/pgvector:pg16
    ports:
      - "5432:5432"
    environment:
      POSTGRES_DB: second_brain
      POSTGRES_USER: brain
      POSTGRES_PASSWORD: brain
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U brain -d second_brain"]
      interval: 5s
      timeout: 5s
      retries: 10

  server:
    build:
      context: .
      dockerfile: Dockerfile
      target: server
    depends_on:
      postgres:
        condition: service_healthy
    ports:
      - "8080:8080"
    environment:
      DATABASE_URL: postgres://brain:brain@postgres:5432/second_brain?sslmode=disable
      PORT: "8080"
      MIGRATIONS_DIR: /app/migrations
    env_file:
      - .env
    restart: unless-stopped

  collector:
    build:
      context: .
      dockerfile: Dockerfile
      target: collector
    depends_on:
      postgres:
        condition: service_healthy
    environment:
      DATABASE_URL: postgres://brain:brain@postgres:5432/second_brain?sslmode=disable
      COLLECT_INTERVAL: 10m
      MIGRATIONS_DIR: /app/migrations
    env_file:
      - .env
    volumes:
      - "/Users/sangyi/Google Drive/공유 드라이브/Vibers.AI:/data/drive:ro"
    restart: unless-stopped

volumes:
  pgdata:
```

- [ ] **Step 3: Update server default port**

Modify `internal/config/config.go` — change default PORT from 9200 to 8080:

```go
Port: getenv("PORT", "8080"),
```

- [ ] **Step 4: Build both targets**

Run: `docker compose build server collector`
Expected: Both images build successfully.

- [ ] **Step 5: Commit**

```bash
git add Dockerfile docker-compose.yml internal/config/config.go
git commit -m "feat: docker multi-target build for server and collector"
```

---

## Task 8: Collector Interface Stubs

**Files:**
- Create: `internal/collector/discord_stub.go`
- Create: `internal/collector/telegram_stub.go`
- Create: `internal/collector/notion_stub.go`

- [ ] **Step 1: Write Discord stub**

Create `internal/collector/discord_stub.go`:

```go
package collector

// Discord collector stub — interface defined, not implemented.
// The existing discord.go contains the full implementation;
// this stub exists as documentation that Discord collection
// is available but excluded from the default collector set
// in cmd/collector (bot response removed, collection preserved
// as opt-in for future re-enablement).
//
// To enable: register NewDiscordCollectorWithAttachments in cmd/collector/main.go.
```

- [ ] **Step 2: Write Telegram stub**

Create `internal/collector/telegram_stub.go`:

```go
package collector

import (
	"context"
	"time"

	"github.com/baekenough/second-brain/internal/model"
)

const SourceTelegram model.SourceType = "telegram"

// TelegramCollector is a placeholder for future Telegram data collection.
type TelegramCollector struct{}

func NewTelegramCollector() *TelegramCollector { return &TelegramCollector{} }

func (c *TelegramCollector) Name() string                  { return "telegram" }
func (c *TelegramCollector) Source() model.SourceType       { return SourceTelegram }
func (c *TelegramCollector) Enabled() bool                  { return false }
func (c *TelegramCollector) Collect(ctx context.Context, since time.Time) ([]model.Document, error) {
	return nil, nil
}
```

- [ ] **Step 3: Write Notion stub update**

The existing `internal/collector/notion.go` already exists. Verify it implements the Collector interface. If not, update it to match.

- [ ] **Step 4: Commit**

```bash
git add internal/collector/discord_stub.go internal/collector/telegram_stub.go
git commit -m "feat: add collector interface stubs for telegram and discord"
```

---

## Task 9: Update Documentation

**Files:**
- Modify: `README.md`
- Modify: `README.en.md`
- Modify: `ARCHITECTURE.md`
- Modify: `ARCHITECTURE.en.md`
- Modify: `EXPANSION.md`
- Modify: `docs/runbook-deploy.md`

- [ ] **Step 1: Update README.md**

Key changes to the README:
1. Update description: "LLM 큐레이션 프라이빗 검색 엔진" instead of "사내 AI 검색 엔진"
2. Update architecture diagram: show server and collector as separate services
3. Update project structure: add cmd/collector/, internal/curation/
4. Update API reference: add GET /api/v1/search, add curated parameter, remove collect/trigger endpoints
5. Update environment variables: add CURATION vars, remove Discord bot vars from server section
6. Update Docker section: show dual-service docker-compose
7. Update quick start: docker compose up instead of minikube
8. Update port from 9200 to 8080

- [ ] **Step 2: Update README.en.md**

Mirror all README.md changes in English.

- [ ] **Step 3: Update ARCHITECTURE.md**

Key changes:
1. Update vision statement to reflect curation API
2. Update system diagram: two binaries (server + collector)
3. Update service layer map: add curation layer
4. Update search pipeline: add pg_bigm as third signal
5. Update deployment architecture: docker-compose dual service

- [ ] **Step 4: Update ARCHITECTURE.en.md**

Mirror ARCHITECTURE.md changes in English.

- [ ] **Step 5: Update EXPANSION.md**

Mark completed items:
- Chunking strategies → partially done (heading-aware chunks exist)
- Binary separation → DONE
- pg_bigm Korean search → DONE
- LLM curation → DONE

Add new TODO:
- GraphQL secondary API
- Telegram collector
- Notion collector re-enablement

- [ ] **Step 6: Update docs/runbook-deploy.md**

Update deployment guide for docker-compose dual-service setup.

- [ ] **Step 7: Update GitHub repo description**

Run: `gh repo edit --description "LLM 큐레이션 프라이빗 검색 엔진. Slack/GitHub/GDrive/파일시스템 하이브리드 검색 + AI 큐레이션 API."`

- [ ] **Step 8: Commit**

```bash
git add README.md README.en.md ARCHITECTURE.md ARCHITECTURE.en.md EXPANSION.md docs/runbook-deploy.md
git commit -m "docs: update documentation for curation API architecture"
```

---

## Task 10: Integration Test

**Files:**
- No new files — verify existing tests pass with refactored code.

- [ ] **Step 1: Run full test suite**

Run: `go test ./... -v -count=1`
Expected: All tests pass. Fix any failures from refactoring.

- [ ] **Step 2: Build both binaries**

Run: `go build ./cmd/server/ && go build ./cmd/collector/`
Expected: Both compile.

- [ ] **Step 3: Docker compose build**

Run: `docker compose build`
Expected: Both server and collector images build.

- [ ] **Step 4: Docker compose up (smoke test)**

Run: `docker compose up -d && sleep 10 && curl http://localhost:8080/health`
Expected: `{"status":"ok"}`

Run: `docker compose logs collector | head -20`
Expected: Collector daemon started log message.

- [ ] **Step 5: Test search with curated parameter**

Run:
```bash
curl -X POST http://localhost:8080/api/v1/search \
  -H "Content-Type: application/json" \
  -d '{"query": "test", "curated": false}'
```
Expected: Raw search results (existing behavior).

Run:
```bash
curl "http://localhost:8080/api/v1/search?q=test&curated=true"
```
Expected: Curated results (or passthrough if LLM not configured).

- [ ] **Step 6: Docker compose down**

Run: `docker compose down`

- [ ] **Step 7: Final commit**

```bash
git add -A
git commit -m "test: verify integration after curation API refactor"
```
