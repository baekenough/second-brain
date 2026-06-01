// Package main implements the MCP (Model Context Protocol) server for second-brain.
// It exposes search, document retrieval, and stats tools so that AI agents can
// query the knowledge base via the standard MCP protocol.
//
// Transport: streamable HTTP (POST /mcp + GET /mcp/sse).
// Port:      MCP_PORT env var (default 8090).
//
// Tools:
//   - search       — hybrid FTS + vector search over collected documents
//   - get_document — fetch a single document by UUID
//   - stats        — per-source document / chunk count statistics
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
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

	// --- Embedding client ---
	embedClient := search.NewEmbedClient(
		cfg.EmbeddingAPIURL,
		cfg.EmbeddingAPIKey,
		cfg.CliProxyAuthFile,
		cfg.EmbeddingModel,
	)
	if embedClient.Enabled() {
		slog.Info("embedding API configured", "url", cfg.EmbeddingAPIURL, "model", cfg.EmbeddingModel)
	} else {
		slog.Info("embedding API not configured — full-text search only")
	}

	// --- Search service (same assembly as cmd/server) ---
	searchSvc := search.NewService(docStore, embedClient).
		WithChunkStore(chunkStore)

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

	// Register the three tools.
	registerSearchTool(s, searchSvc)
	registerGetDocumentTool(s, docStore)
	registerStatsTool(s, docStore)

	addr := bindAddr + ":" + mcpPort
	slog.Info("MCP server starting", "addr", addr, "transport", "streamable-http")

	httpSrv := server.NewStreamableHTTPServer(s)

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
// Tool: search
// ---------------------------------------------------------------------------

// allowedSourceTypes is the set of valid model.SourceType values accepted by
// the search tool's "source" parameter. Defined once for reuse and validation.
var allowedSourceTypes = map[model.SourceType]struct{}{
	model.SourceSlack:      {},
	model.SourceGitHub:     {},
	model.SourceGDrive:     {},
	model.SourceNotion:     {},
	model.SourceFilesystem: {},
	model.SourceDiscord:    {},
	model.SourceTelegram:   {},
	model.SourceSecretary:  {},
	model.SourceLLMMemory:  {},
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
}

func registerSearchTool(s *server.MCPServer, svc *search.Service) {
	tool := mcp.NewTool(
		"search",
		mcp.WithDescription(
			"Hybrid full-text and semantic search over the second-brain knowledge base. "+
				"Returns matching documents ordered by relevance score.",
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
					"filesystem, discord, telegram, secretary, llm-memory.",
			),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
					"unknown source type %q; allowed: slack, github, gdrive, notion, filesystem, discord, telegram, secretary, llm-memory",
					src,
				)), nil
			}
			sq.SourceType = &st
		}

		results, err := svc.Search(ctx, sq)
		if err != nil {
			slog.Error("mcp search: query failed", "error", err, "query", query)
			return mcp.NewToolResultError(fmt.Sprintf("search failed: %s", err.Error())), nil
		}

		out := make([]searchResult, 0, len(results))
		for _, r := range results {
			out = append(out, searchResult{
				DocumentID: r.ID.String(),
				Title:      r.Title,
				SourceType: string(r.SourceType),
				Score:      r.Score,
				MatchType:  r.MatchType,
				Snippet:    truncateRunes(r.Content, 500),
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
	ID          string         `json:"id"`
	SourceType  string         `json:"source_type"`
	SourceID    string         `json:"source_id"`
	Title       string         `json:"title"`
	Content     string         `json:"content"`
	Metadata    map[string]any `json:"metadata"`
	Status      string         `json:"status"`
	// OccurredAt is the original event time (email sent date, calendar start,
	// SMS/call time, etc.). Empty string when not available.
	OccurredAt  string         `json:"occurred_at,omitempty"`
	CollectedAt string         `json:"collected_at"`
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
		baseline, err := docs.QueryBaselineStats(ctx)
		if err != nil {
			slog.Error("mcp stats: query failed", "error", err)
			// Fall back to simple count-by-source when the full baseline query fails.
			counts, cerr := docs.CountBySource(ctx)
			if cerr != nil {
				return mcp.NewToolResultError(fmt.Sprintf("stats query failed: baseline: %s; fallback: %s", err, cerr)), nil
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
			"total_documents":   baseline.Documents.Total,
			"documents_by_source": bySource,
			"chunks": map[string]any{
				"total":                  baseline.Chunks.Total,
				"avg_chunks_per_document": baseline.Chunks.AvgChunksPerDocument,
				"avg_chunk_size_bytes":   baseline.Chunks.AvgChunkSizeBytes,
			},
		})
		if err != nil {
			return mcp.NewToolResultError("failed to encode stats"), nil
		}
		return mcp.NewToolResultText(string(data)), nil
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// truncateRunes returns the first n runes of s.
// When s is shorter than n runes it is returned unchanged.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n])
}

