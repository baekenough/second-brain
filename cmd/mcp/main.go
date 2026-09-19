// Package main implements the MCP (Model Context Protocol) server for second-brain.
// It exposes search, document retrieval, stats, and note-writing tools so that
// AI agents can query and populate the knowledge base via the standard MCP protocol.
//
// Transport: streamable HTTP (POST /mcp + GET /mcp/sse).
// Port:      MCP_PORT env var (default 8090).
//
// Tools (all require Bearer token auth when API_KEY is set):
//   - search       — hybrid FTS + vector search over collected documents
//   - get_document — fetch a single document by UUID
//   - stats        — per-source document / chunk count statistics
//   - add_note     — persist a note or memory into the knowledge base
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/note"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/baekenough/second-brain/internal/timeutil"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	if err := run(); err != nil {
		slog.Error("mcp server startup failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	// Overload .env file when present; unlike Load(), Overload() forces .env
	// values to win over pre-existing env vars, preventing stale/empty values
	// (e.g. empty ANTHROPIC_API_KEY) from causing 401 auth failures.
	// Ignore error because env vars may be injected directly (Docker, k8s, etc.).
	_ = godotenv.Overload()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// --- Database ---
	// Migrations are NOT run here; the server target already applies them on
	// every startup. Running them a second time from the MCP process causes
	// a race condition when both start concurrently.
	pg, err := store.NewPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pg.Close()

	docStore := store.NewDocumentStore(pg)
	chunkStore := store.NewChunkStore(pg)

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

	// --- Reranker (optional — same assembly as cmd/server). Enabled() gates
	// on RerankURL being non-empty, so this is a no-op when unconfigured.
	reranker := search.NewHTTPReranker(cfg.RerankURL, cfg.RerankAPIKey, cfg.RerankModel, cfg.RerankTopN)
	if reranker.Enabled() {
		slog.Info("reranker configured", "url", cfg.RerankURL, "model", cfg.RerankModel)
	}

	// --- Search service (same assembly as cmd/server) ---
	searchSvc := search.NewService(docStore, embedClient).
		WithChunkStore(chunkStore).
		WithReranker(reranker)

	// --- MCP server ---
	mcpPort := os.Getenv("MCP_PORT")
	if mcpPort == "" {
		mcpPort = "8090"
	}
	bindAddr := os.Getenv("MCP_BIND_ADDR")
	if bindAddr == "" {
		bindAddr = "127.0.0.1"
	}

	s := server.NewMCPServer(
		"second-brain",
		"1.0.0",
		server.WithToolCapabilities(false),
	)

	// Register tools.
	registerSearchTool(s, searchSvc, cfg.RerankDefault)
	registerGetDocumentTool(s, docStore)
	registerStatsTool(s, docStore)
	registerAddNoteTool(s, docStore, chunkStore, embedClient, cfg.APIKey)

	addr := bindAddr + ":" + mcpPort
	slog.Info("MCP server starting", "addr", addr, "transport", "streamable-http",
		"add_note_auth", cfg.APIKey != "")

	// Build a custom *http.Server with timeouts to guard against slow clients
	// and runaway connections. WriteTimeout is generous (5 min) to accommodate
	// large embed operations that may take a while to stream back.
	// Handler is wired after httpSrv is constructed (see below).
	customHTTPSrv := &http.Server{
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  2 * time.Minute,
	}

	// WithHTTPContextFunc injects the Bearer-token validation result into the
	// request context so tool handlers can check authorisation without direct
	// access to HTTP headers.
	// WithStreamableHTTPServer injects the pre-configured *http.Server so that
	// our timeout settings are applied; Start(addr) will set Addr on it.
	httpSrv := server.NewStreamableHTTPServer(s,
		server.WithHTTPContextFunc(mcpAuthContextFunc(cfg.APIKey)),
		server.WithStreamableHTTPServer(customHTTPSrv),
	)
	// Wire the handler after httpSrv is constructed; the library uses
	// customHTTPSrv.Handler as-is when httpServer is pre-set via
	// WithStreamableHTTPServer (it skips the internal mux setup).
	customHTTPSrv.Handler = httpSrv

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Start(addr); err != nil {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("MCP server shutting down...")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return err
	}

	slog.Info("MCP server shutdown complete")
	return nil
}

// ---------------------------------------------------------------------------
// Bearer-token authentication
// ---------------------------------------------------------------------------

// mcpAuthKey is the context key used to propagate Bearer-token validation.
type mcpAuthKey struct{}

// mcpAuthContextFunc returns a server.HTTPContextFunc that extracts the Bearer
// token from every incoming HTTP request and stores the validation result in
// the context under mcpAuthKey{}.
//
// When apiKey is empty the function marks every request as authorised,
// preserving backward compatibility in development environments.
// Timing-safe comparison via crypto/subtle prevents timing attacks.
func mcpAuthContextFunc(apiKey string) server.HTTPContextFunc {
	expected := []byte(apiKey)
	enabled := len(apiKey) > 0
	return func(ctx context.Context, r *http.Request) context.Context {
		if !enabled {
			return context.WithValue(ctx, mcpAuthKey{}, true)
		}
		const prefix = "Bearer "
		authz := r.Header.Get("Authorization")
		if !strings.HasPrefix(authz, prefix) {
			return context.WithValue(ctx, mcpAuthKey{}, false)
		}
		token := []byte(strings.TrimPrefix(authz, prefix))
		ok := subtle.ConstantTimeCompare(token, expected) == 1
		return context.WithValue(ctx, mcpAuthKey{}, ok)
	}
}

// isAuthorized reports whether ctx carries a valid Bearer-token claim.
func isAuthorized(ctx context.Context) bool {
	v, _ := ctx.Value(mcpAuthKey{}).(bool)
	return v
}

// ---------------------------------------------------------------------------
// Tool: search
// ---------------------------------------------------------------------------

// allowedSourceTypes is the set of valid model.SourceType values accepted by
// the search tool's "source" parameter. Defined once for reuse and validation.
var allowedSourceTypes = map[model.SourceType]struct{}{
	model.SourceSlack:          {},
	model.SourceGitHub:         {},
	model.SourceGDrive:         {},
	model.SourceNotion:         {},
	model.SourceFilesystem:     {},
	model.SourceDiscord:        {},
	model.SourceTelegram:       {},
	model.SourceSecretary:      {},
	model.SourceLLMMemory:      {}, // deprecated (see model.SourceLLMMemory doc comment); kept so legacy rows remain searchable
	model.SourceGmail:          {},
	model.SourceCalendar:       {},
	model.SourceSMS:            {},
	model.SourceCall:           {},
	model.SourceCallLog:        {}, // deprecated (see model.SourceCallLog doc comment); kept so legacy rows remain searchable
	model.SourceCallTranscript: {}, // deprecated (see model.SourceCallLog doc comment); kept so legacy rows remain searchable
	model.SourceUpload:         {},
	model.SourceAgentNote:      {},
}

// searchResult is the MCP-friendly projection of model.SearchResult.
// Embedding is omitted (too large and not useful for LLM callers).
type searchResult struct {
	DocumentID string  `json:"document_id"`
	Title      string  `json:"title"`
	SourceType string  `json:"source_type"`
	Score      float64 `json:"score"`
	MatchType  string  `json:"match_type"`
	Snippet    string  `json:"snippet"` // first 500 runes of content
	// OccurredAt is the original event time (RFC3339), nil when the document
	// carries no occurred_at (see model.Document.OccurredAt doc comment).
	OccurredAt *string `json:"occurred_at"`
	// Retention is documents.metadata["retention"] ("keep"/"low"/"disposable",
	// see model.RetentionTag* constants), nil when untagged. "low" and
	// "disposable" mark lower-confidence evidence — see the tool description.
	Retention *string `json:"retention"`
	// Segment is documents.metadata["segment"], nil when absent. No collector
	// in this codebase writes this key yet (as of 2026-09-19, segmentation
	// tagging is done out-of-band and only populates "retention") — exposed
	// pre-emptively so callers do not need a schema change once one does.
	Segment *string `json:"segment"`
}

// metadataString reads a string value out of a document's Metadata map,
// returning nil when the key is absent, the map is nil, or the value is not
// a non-empty string. Mirrors model.Document.RetentionTag's nil-safety but
// stays generic (segment metadata has no dedicated helper/constants yet).
func metadataString(m map[string]any, key string) *string {
	if m == nil {
		return nil
	}
	v, ok := m[key]
	if !ok {
		return nil
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return nil
	}
	return &s
}

func registerSearchTool(s *server.MCPServer, svc *search.Service, rerankDefault bool) {
	tool := mcp.NewTool(
		"search",
		mcp.WithDescription(
			"Hybrid full-text and semantic search over the second-brain knowledge base. "+
				"Returns matching documents ordered by relevance score. "+
				"Use occurred_from/occurred_to to restrict results to an event-time window "+
				"(e.g. \"next week's calendar\", \"messages from yesterday\"). "+
				"retention=low/disposable results are lower-confidence evidence — "+
				"see the retention field on each result.",
		),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("The search query text."),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of results to return (1–50, default 10)."),
		),
		mcp.WithString("source",
			mcp.Description(
				"Optional source type filter. One of: slack, github, gdrive, notion, "+
					"filesystem, discord, telegram, secretary, llm-memory (deprecated), "+
					"gmail, calendar, sms, call, call-log (deprecated), call-transcript (deprecated), "+
					"upload, agent-note. call-log/call-transcript were unified into a single "+
					"'call' source type (one document per phone call, recorded or not).",
			),
		),
		mcp.WithString("occurred_from",
			mcp.Description(
				"Optional start of an event-time window (inclusive) on documents.occurred_at. "+
					"Together with occurred_to this forms a half-open range [occurred_from, occurred_to) — "+
					"documents with no occurred_at (e.g. some notes/attachments) are excluded whenever "+
					"either bound is set. Accepts RFC3339 (\"2026-09-05T00:00:00+09:00\") or a bare "+
					"date (\"2026-09-05\"); a bare date is interpreted as KST midnight. May be given alone.",
			),
		),
		mcp.WithString("occurred_to",
			mcp.Description(
				"Optional end of the event-time window (exclusive) on documents.occurred_at — see "+
					"occurred_from for the window semantics and accepted formats. Must be strictly "+
					"after occurred_from when both are given. May be given alone.",
			),
		),
		mcp.WithString("sort",
			mcp.Enum("relevance", "recent"),
			mcp.Description(
				"Result ordering. \"relevance\" (default) ranks by search score; \"recent\" "+
					"ranks by documents.occurred_at (soonest-first when occurred_from lies "+
					"entirely in the future, newest-first otherwise). When occurred_from or "+
					"occurred_to is given and sort is omitted, \"recent\" is applied "+
					"automatically — the same default /api/v1/ask uses for windowed queries — "+
					"so a caller must pass sort=\"relevance\" explicitly to keep relevance "+
					"ordering inside a time window.",
			),
		),
		mcp.WithBoolean("include_retention",
			mcp.Description(
				"When true, disables the default exclusion of retention=disposable documents "+
					"(gmail newsletters/notifications/transactional noise tagged by the segmentation "+
					"pass). Default false — most callers want that noise filtered out.",
			),
		),
		mcp.WithBoolean("use_rerank",
			mcp.Description(
				"Whether to apply cross-encoder reranking to the results. When omitted, "+
					"falls back to the server's SEARCH_RERANK_DEFAULT setting — EXCEPT when "+
					"sort resolves to \"recent\" (explicitly or auto-applied for a time "+
					"window), in which case the default is skipped so recency order is not "+
					"silently overridden; set use_rerank=true explicitly to rerank anyway. "+
					"Has no effect when the server has no RERANKER_URL configured.",
			),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Auth check — only enforced when API_KEY is set (isAuthorized returns
		// true unconditionally when no key is configured).
		if !isAuthorized(ctx) {
			return mcp.NewToolResultError("unauthorized: Bearer token required"), nil
		}

		query, err := req.RequireString("query")
		if err != nil || strings.TrimSpace(query) == "" {
			return mcp.NewToolResultError("query parameter is required and must be non-empty"), nil
		}

		limit := req.GetInt("limit", 10)
		if limit <= 0 {
			limit = 10
		}
		if limit > 50 {
			limit = 50
		}

		sq := model.SearchQuery{
			Query: strings.TrimSpace(query),
			Limit: limit,
		}

		if src := req.GetString("source", ""); src != "" {
			st := model.SourceType(strings.TrimSpace(src))
			if _, ok := allowedSourceTypes[st]; !ok {
				return mcp.NewToolResultError(fmt.Sprintf(
					"unknown source type %q; allowed: slack, github, gdrive, notion, filesystem, discord, telegram, secretary, llm-memory, gmail, calendar, sms, call, call-log (deprecated), call-transcript (deprecated), upload, agent-note",
					src,
				)), nil
			}
			sq.SourceType = &st
		}

		if raw := req.GetString("occurred_from", ""); raw != "" {
			t, err := parseOccurredBound(raw)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("invalid occurred_from %q: %v", raw, err)), nil
			}
			sq.OccurredFrom = t
		}
		if raw := req.GetString("occurred_to", ""); raw != "" {
			t, err := parseOccurredBound(raw)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("invalid occurred_to %q: %v", raw, err)), nil
			}
			sq.OccurredTo = t
		}
		if sq.OccurredFrom != nil && sq.OccurredTo != nil && !sq.OccurredTo.After(*sq.OccurredFrom) {
			return mcp.NewToolResultError(
				"occurred_to must be strictly after occurred_from (half-open window [occurred_from, occurred_to))",
			), nil
		}

		// sort: "relevance" (default) | "recent". Mirrors internal/api/
		// ask_retrieval.go's windowed-plan default — when an event-time
		// window is given and sort is left unspecified, recency ordering is
		// applied automatically, because relevance order inside an
		// already-narrowed time window is rarely what a temporal query
		// wants. An explicit sort=relevance still wins inside a window.
		switch sortRaw := strings.TrimSpace(strings.ToLower(req.GetString("sort", ""))); sortRaw {
		case "":
			if sq.OccurredFrom != nil || sq.OccurredTo != nil {
				sq.Sort = model.SortRecent
			}
		case "relevance":
			// explicit request for the default ordering — nothing to set
		case model.SortRecent:
			sq.Sort = model.SortRecent
		default:
			return mcp.NewToolResultError(fmt.Sprintf(
				"unknown sort %q; allowed: relevance, recent", sortRaw,
			)), nil
		}

		sq.IncludeRetention = req.GetBool("include_retention", false)

		// use_rerank: falls back to the server default, EXCEPT when the
		// query resolved to recency ordering (explicitly or via the
		// auto-apply above) and the caller did not explicitly ask for
		// reranking. search.Service applies the cross-encoder AFTER the
		// recency sort and its output order is final (internal/search/
		// search.go's applyRerank ordering comment) — silently defaulting
		// rerank on would silently undo the recency order this tool just
		// promised. An explicit use_rerank=true is still honored: that is a
		// deliberate caller choice, not a silent default.
		_, useRerankExplicit := req.GetArguments()["use_rerank"]
		sq.UseRerank = req.GetBool("use_rerank", rerankDefault)
		if !useRerankExplicit && sq.SortsByRecency() {
			sq.UseRerank = false
		}

		results, err := svc.Search(ctx, sq)
		if err != nil {
			slog.Error("mcp search: query failed", "error", err, "query", query)
			return mcp.NewToolResultError("internal error searching documents"), nil
		}

		out := make([]searchResult, 0, len(results))
		for _, r := range results {
			var occurredAt *string
			if r.OccurredAt != nil {
				s := r.OccurredAt.Format(time.RFC3339)
				occurredAt = &s
			}
			var retention *string
			if tag, ok := r.RetentionTag(); ok {
				retention = &tag
			}
			out = append(out, searchResult{
				DocumentID: r.ID.String(),
				Title:      r.Title,
				SourceType: string(r.SourceType),
				Score:      r.Score,
				MatchType:  r.MatchType,
				Snippet:    truncateRunes(r.Content, 500),
				OccurredAt: occurredAt,
				Retention:  retention,
				Segment:    metadataString(r.Metadata, "segment"),
			})
		}

		data, err := json.Marshal(map[string]any{
			"results": out,
			"count":   len(out),
			"query":   sq.Query,
		})
		if err != nil {
			return mcp.NewToolResultError("failed to encode results"), nil
		}
		return mcp.NewToolResultText(string(data)), nil
	})
}

// ---------------------------------------------------------------------------
// Tool: get_document
// ---------------------------------------------------------------------------

// documentResult is the MCP-friendly projection of model.Document.
// Embedding is omitted.
type documentResult struct {
	ID         string         `json:"id"`
	SourceType string         `json:"source_type"`
	SourceID   string         `json:"source_id"`
	Title      string         `json:"title"`
	Content    string         `json:"content"`
	Metadata   map[string]any `json:"metadata"`
	Status     string         `json:"status"`
	// OccurredAt is the original event time (email sent date, calendar start,
	// SMS/call time, etc.). Empty string when not available.
	OccurredAt  string `json:"occurred_at,omitempty"`
	CollectedAt string `json:"collected_at"`
}

// DocumentGetter is the subset of DocumentStore used by the get_document tool.
type DocumentGetter interface {
	GetByID(ctx context.Context, id uuid.UUID) (*model.Document, error)
}

func registerGetDocumentTool(s *server.MCPServer, docs DocumentGetter) {
	tool := mcp.NewTool(
		"get_document",
		mcp.WithDescription(
			"Retrieve a single document by its UUID. "+
				"Returns the full document including title, content, source type, and metadata.",
		),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("The document UUID (e.g. 123e4567-e89b-12d3-a456-426614174000)."),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Auth check — only enforced when API_KEY is set (isAuthorized returns
		// true unconditionally when no key is configured).
		if !isAuthorized(ctx) {
			return mcp.NewToolResultError("unauthorized: Bearer token required"), nil
		}

		idStr, err := req.RequireString("id")
		if err != nil || strings.TrimSpace(idStr) == "" {
			return mcp.NewToolResultError("id parameter is required"), nil
		}

		id, err := uuid.Parse(strings.TrimSpace(idStr))
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid document ID %q: must be a valid UUID", idStr)), nil
		}

		doc, err := docs.GetByID(ctx, id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return mcp.NewToolResultError(fmt.Sprintf("document %q not found", idStr)), nil
			}
			slog.Error("mcp get_document: internal error", "id", idStr, "error", err)
			return mcp.NewToolResultError("internal error retrieving document"), nil
		}

		result := documentResult{
			ID:          doc.ID.String(),
			SourceType:  string(doc.SourceType),
			SourceID:    doc.SourceID,
			Title:       doc.Title,
			Content:     doc.Content,
			Metadata:    doc.Metadata,
			Status:      doc.Status,
			CollectedAt: doc.CollectedAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
		if doc.OccurredAt != nil {
			result.OccurredAt = doc.OccurredAt.UTC().Format("2006-01-02T15:04:05Z")
		}

		data, err := json.Marshal(result)
		if err != nil {
			return mcp.NewToolResultError("failed to encode document"), nil
		}
		return mcp.NewToolResultText(string(data)), nil
	})
}

// ---------------------------------------------------------------------------
// Tool: stats
// ---------------------------------------------------------------------------

// StatsProvider is the subset of DocumentStore used by the stats tool.
type StatsProvider interface {
	CountBySource(ctx context.Context) (map[string]int, error)
	QueryBaselineStats(ctx context.Context) (*store.BaselineStats, error)
}

func registerStatsTool(s *server.MCPServer, docs StatsProvider) {
	tool := mcp.NewTool(
		"stats",
		mcp.WithDescription(
			"Return document and chunk statistics for the second-brain knowledge base. "+
				"Shows per-source document counts and overall chunk metrics.",
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Auth check — only enforced when API_KEY is set (isAuthorized returns
		// true unconditionally when no key is configured).
		if !isAuthorized(ctx) {
			return mcp.NewToolResultError("unauthorized: Bearer token required"), nil
		}

		baseline, err := docs.QueryBaselineStats(ctx)
		if err != nil {
			slog.Error("mcp stats: baseline query failed", "error", err)
			// Fall back to simple count-by-source when the full baseline query fails.
			counts, cerr := docs.CountBySource(ctx)
			if cerr != nil {
				// Both queries failed — log details server-side only; never expose
				// raw DB errors (connection strings, table names) to callers.
				slog.Error("mcp stats: fallback count-by-source also failed", "error", cerr)
				return mcp.NewToolResultError("internal error retrieving stats"), nil
			}
			data, _ := json.Marshal(map[string]any{
				"documents_by_source": counts,
			})
			return mcp.NewToolResultText(string(data)), nil
		}

		// Build a simplified view for MCP callers.
		bySource := make(map[string]int, len(baseline.Documents.BySource))
		for src, st := range baseline.Documents.BySource {
			bySource[src] = st.Count
		}

		data, err := json.Marshal(map[string]any{
			"total_documents":     baseline.Documents.Total,
			"documents_by_source": bySource,
			"chunks": map[string]any{
				"total":                   baseline.Chunks.Total,
				"avg_chunks_per_document": baseline.Chunks.AvgChunksPerDocument,
				"avg_chunk_size_bytes":    baseline.Chunks.AvgChunkSizeBytes,
			},
		})
		if err != nil {
			return mcp.NewToolResultError("failed to encode stats"), nil
		}
		return mcp.NewToolResultText(string(data)), nil
	})
}

// ---------------------------------------------------------------------------
// Tool: add_note
// ---------------------------------------------------------------------------

func registerAddNoteTool(
	s *server.MCPServer,
	docs note.DocumentUpserter,
	chunks note.ChunkWriter,
	embed note.Embedder,
	_ string, // apiKey is consumed via WithHTTPContextFunc; kept for clarity
) {
	tool := mcp.NewTool(
		"add_note",
		mcp.WithDescription(
			"Persist a note or memory into the second-brain knowledge base. "+
				"The note is stored with source_type=agent-note and split into searchable "+
				"chunks. Re-using the same source_id updates the existing note (upsert). "+
				"Requires Bearer token authentication when API_KEY is configured.",
		),
		mcp.WithString("title",
			mcp.Required(),
			mcp.Description("Short title for the note (non-empty)."),
		),
		mcp.WithString("content",
			mcp.Required(),
			mcp.Description("Full text content of the note (max 10 MiB)."),
		),
		mcp.WithString("source_id",
			mcp.Description(
				"Optional stable identifier for the note. "+
					"When omitted a random UUID is generated. "+
					"Re-using the same source_id updates the existing note (upsert).",
			),
		),
		mcp.WithObject("metadata",
			mcp.Description("Optional JSON object of arbitrary key-value pairs attached to the note."),
		),
		mcp.WithBoolean("embed",
			mcp.Description(
				"Whether to generate embedding vectors for the chunks (default true). "+
					"Set false to skip embeddings when the embedding API is unavailable.",
			),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Auth check — only enforced when API_KEY is set (isAuthorized returns
		// true unconditionally when no key is configured).
		if !isAuthorized(ctx) {
			return mcp.NewToolResultError("unauthorized: Bearer token required"), nil
		}

		title := req.GetString("title", "")
		content := req.GetString("content", "")
		sourceID := req.GetString("source_id", "")
		doEmbed := req.GetBool("embed", true)

		// Extract optional metadata object from the raw arguments map.
		var metadata map[string]any
		if args := req.GetArguments(); args != nil {
			if raw, ok := args["metadata"]; ok && raw != nil {
				if m, ok := raw.(map[string]any); ok {
					metadata = m
				}
			}
		}

		// MCP add_note always writes model.SourceAgentNote (spec §6.2,
		// updated 2026-08-25 — see model.SourceLLMMemory's doc comment for
		// why this is no longer model.SourceLLMMemory) and always requires a
		// non-empty title — the latter is the one deliberate behavioural
		// difference from POST /api/v1/notes.
		result, errMsg := note.Save(ctx, docs, chunks, embed,
			model.SourceAgentNote, title, content, sourceID, metadata, doEmbed, true)
		if errMsg != "" {
			return mcp.NewToolResultError(errMsg), nil
		}

		data, err := json.Marshal(result)
		if err != nil {
			return mcp.NewToolResultError("failed to encode response"), nil
		}
		return mcp.NewToolResultText(string(data)), nil
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// parseOccurredBound parses one occurred_from/occurred_to argument of the
// search tool. It accepts either a full RFC3339 timestamp
// (e.g. "2026-09-05T00:00:00+09:00") or a bare date ("2026-09-05").
//
// A bare date is interpreted at midnight in KST (UTC+9). This mirrors the
// convention internal/intent/plan.go's parseWindow already uses for the same
// documents.occurred_at half-open window: the query planner resolves LLM- and
// heuristic-produced "YYYY-MM-DD" bounds against timeutil.KST(), and second-
// brain's users and data are Korea-based, so a bare date given to this MCP
// tool must resolve to the same instant a planner-produced date would — an
// MCP client asking for "2026-09-05" should get the same window as /ask
// asking for "오늘(9/5)".
//
// The caller distinguishes "not provided" (empty string, skip the field
// entirely) from "provided but malformed" (returns an error) — this function
// only handles the latter; empty-string short-circuiting happens at the call
// site so a truly optional bound leaves the corresponding model.SearchQuery
// field nil.
func parseOccurredBound(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return &t, nil
	}
	t, err := time.ParseInLocation("2006-01-02", s, timeutil.KST())
	if err != nil {
		return nil, fmt.Errorf(
			"must be RFC3339 (e.g. 2026-09-05T00:00:00+09:00) or a date (e.g. 2026-09-05)")
	}
	return &t, nil
}

// truncateRunes returns the first n runes of s.
// When s is shorter than n runes it is returned unchanged.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n])
}
