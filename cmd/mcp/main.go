// Package main implements the MCP (Model Context Protocol) server for second-brain.
// It exposes search, document retrieval, stats, and note-writing tools so that
// AI agents can query and populate the knowledge base via the standard MCP protocol.
//
// Transport: streamable HTTP (POST /mcp + GET /mcp/sse).
// Port:      MCP_PORT env var (default 8090).
//
// Tools:
//   - search       — hybrid FTS + vector search over collected documents
//   - get_document — fetch a single document by UUID
//   - stats        — per-source document / chunk count statistics
//   - add_note     — persist a note or memory into the knowledge base (requires auth)
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

	"github.com/baekenough/second-brain/internal/chunker"
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

	// Register tools.
	registerSearchTool(s, searchSvc)
	registerGetDocumentTool(s, docStore)
	registerStatsTool(s, docStore)
	registerAddNoteTool(s, docStore, chunkStore, embedClient, cfg.APIKey)

	addr := bindAddr + ":" + mcpPort
	slog.Info("MCP server starting", "addr", addr, "transport", "streamable-http",
		"add_note_auth", cfg.APIKey != "")

	// WithHTTPContextFunc injects the Bearer-token validation result into the
	// request context so tool handlers can check authorisation without direct
	// access to HTTP headers.
	httpSrv := server.NewStreamableHTTPServer(s,
		server.WithHTTPContextFunc(mcpAuthContextFunc(cfg.APIKey)),
	)

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
			return mcp.NewToolResultError("internal error searching documents"), nil
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

// maxNoteContentBytes is the upper bound for add_note content (10 MiB).
const maxNoteContentBytes = 10 * 1024 * 1024

// NoteDocumentUpserter is the subset of DocumentStore used by the add_note tool.
type NoteDocumentUpserter interface {
	Upsert(ctx context.Context, doc *model.Document) error
}

// NoteChunkWriter is the subset of ChunkStore used by the add_note tool.
type NoteChunkWriter interface {
	ReplaceDocument(ctx context.Context, documentID uuid.UUID, chunks []store.Chunk) error
	ListByDocument(ctx context.Context, documentID uuid.UUID) ([]store.Chunk, error)
	UpdateChunkEmbeddings(ctx context.Context, embeddings []store.ChunkEmbedding) error
}

// NoteEmbedder is the subset of EmbedClient used by the add_note tool.
type NoteEmbedder interface {
	Enabled() bool
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
}

// addNoteResult is the JSON response returned by the add_note tool.
type addNoteResult struct {
	ID               string `json:"id"`
	ChunksCreated    int    `json:"chunks_created"`
	EmbeddingCreated bool   `json:"embedding_created"`
}

// handleAddNote contains the core logic for add_note, extracted for unit
// testability without the MCP framing overhead.
//
// Returns (result, "") on success, or (nil, errMsg) on failure.
func handleAddNote(
	ctx context.Context,
	docs NoteDocumentUpserter,
	chunks NoteChunkWriter,
	embed NoteEmbedder,
	title, content, sourceID string,
	metadata map[string]any,
	doEmbed bool,
) (*addNoteResult, string) {
	// Input validation.
	if strings.TrimSpace(title) == "" {
		return nil, "title is required and must be non-empty"
	}
	if strings.TrimSpace(content) == "" {
		return nil, "content is required and must be non-empty"
	}
	if len(content) > maxNoteContentBytes {
		return nil, fmt.Sprintf("content exceeds maximum size of %d bytes", maxNoteContentBytes)
	}

	// Resolve source_id: use provided value or generate a new UUID.
	if strings.TrimSpace(sourceID) == "" {
		sourceID = uuid.New().String()
	}

	// Build document.
	// CollectedAt must be set explicitly; leaving it as the zero value
	// (0001-01-01) causes the note to sort to the bottom of any
	// ORDER BY collected_at DESC query (issue #87 deep-verify).
	// OccurredAt is intentionally left nil: llm-memory notes have no
	// meaningful original event time, and COALESCE(occurred_at, collected_at)
	// in sort expressions will fall back to CollectedAt correctly.
	doc := &model.Document{
		SourceType:  model.SourceLLMMemory,
		SourceID:    sourceID,
		Title:       strings.TrimSpace(title),
		Content:     content,
		Metadata:    metadata,
		Status:      "active",
		CollectedAt: time.Now().UTC(),
	}

	// Persist document (upsert by source_type + source_id).
	if err := docs.Upsert(ctx, doc); err != nil {
		slog.Error("mcp add_note: upsert failed", "source_id", sourceID, "error", err)
		return nil, "internal error saving note"
	}

	// Split content into chunks.
	texts := chunker.Split(content, chunker.SelectOptions(*doc))
	chunkSlice := make([]store.Chunk, 0, len(texts))
	for i, t := range texts {
		chunkSlice = append(chunkSlice, store.Chunk{
			DocumentID: doc.ID,
			ChunkIndex: i,
			Content:    t,
			ByteSize:   len(t),
		})
	}

	if err := chunks.ReplaceDocument(ctx, doc.ID, chunkSlice); err != nil {
		slog.Error("mcp add_note: chunk replace failed", "doc_id", doc.ID, "error", err)
		return nil, "internal error storing note chunks"
	}

	result := &addNoteResult{
		ID:            doc.ID.String(),
		ChunksCreated: len(chunkSlice),
	}

	// Optionally embed chunks.
	if doEmbed && embed.Enabled() && len(chunkSlice) > 0 {
		if embErr := embedNoteChunks(ctx, doc.ID, chunkSlice, chunks, embed); embErr != nil {
			// Embedding failure is non-fatal: the note is already stored and
			// searchable via FTS; only vector search is degraded.
			slog.Warn("mcp add_note: embedding failed (non-fatal)", "doc_id", doc.ID, "error", embErr)
		} else {
			result.EmbeddingCreated = true
		}
	}

	return result, ""
}

// embedNoteChunks generates and persists embedding vectors for the given chunks.
func embedNoteChunks(
	ctx context.Context,
	docID uuid.UUID,
	chunkSlice []store.Chunk,
	chunkStore NoteChunkWriter,
	embedClient NoteEmbedder,
) error {
	texts := make([]string, 0, len(chunkSlice))
	for _, c := range chunkSlice {
		texts = append(texts, c.Content)
	}

	vectors, err := embedClient.EmbedBatch(ctx, texts)
	if err != nil {
		return fmt.Errorf("embed batch: %w", err)
	}

	// Fetch stored chunk IDs to build the ChunkEmbedding slice.
	storedChunks, err := chunkStore.ListByDocument(ctx, docID)
	if err != nil {
		return fmt.Errorf("list stored chunks: %w", err)
	}

	idxToID := make(map[int]int64, len(storedChunks))
	for _, sc := range storedChunks {
		idxToID[sc.ChunkIndex] = sc.ID
	}

	embeddings := make([]store.ChunkEmbedding, 0, len(chunkSlice))
	for i, vec := range vectors {
		if i >= len(chunkSlice) {
			break
		}
		id, ok := idxToID[chunkSlice[i].ChunkIndex]
		if !ok {
			continue
		}
		embeddings = append(embeddings, store.ChunkEmbedding{
			ChunkID:   id,
			Embedding: vec,
		})
	}

	if len(embeddings) == 0 {
		return nil
	}
	return chunkStore.UpdateChunkEmbeddings(ctx, embeddings)
}

func registerAddNoteTool(
	s *server.MCPServer,
	docs NoteDocumentUpserter,
	chunks NoteChunkWriter,
	embed NoteEmbedder,
	_ string, // apiKey is consumed via WithHTTPContextFunc; kept for clarity
) {
	tool := mcp.NewTool(
		"add_note",
		mcp.WithDescription(
			"Persist a note or memory into the second-brain knowledge base. "+
				"The note is stored with source_type=llm-memory and split into searchable "+
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

		result, errMsg := handleAddNote(ctx, docs, chunks, embed, title, content, sourceID, metadata, doEmbed)
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

// truncateRunes returns the first n runes of s.
// When s is shorter than n runes it is returned unchanged.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n])
}
