package note

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/baekenough/second-brain/internal/chunker"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// DocumentUpserter is the subset of DocumentStore used by Save.
type DocumentUpserter interface {
	Upsert(ctx context.Context, doc *model.Document) error
}

// ChunkWriter is the subset of ChunkStore used by Save.
type ChunkWriter interface {
	ReplaceDocument(ctx context.Context, documentID uuid.UUID, chunks []store.Chunk) error
	ListByDocument(ctx context.Context, documentID uuid.UUID) ([]store.Chunk, error)
	UpdateChunkEmbeddings(ctx context.Context, embeddings []store.ChunkEmbedding) error
}

// Embedder is the subset of EmbedClient used by Save.
type Embedder interface {
	Enabled() bool
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
}

// Result is returned by Save on success.
type Result struct {
	ID               string `json:"id"`
	ChunksCreated    int    `json:"chunks_created"`
	EmbeddingCreated bool   `json:"embedding_created"`
}

// Save persists a note document, splits it into chunks, and optionally
// embeds those chunks. It is the extraction of the former cmd/mcp/main.go
// handleAddNote, parameterized so both the MCP add_note tool and the
// POST /api/v1/notes REST handler share identical persistence behaviour.
//
// sourceType controls the stored model.SourceType (model.SourceAgentNote for
// MCP, model.SourceNote for the REST path — spec §6.2, updated 2026-08-25;
// MCP add_note previously wrote model.SourceLLMMemory, since deprecated).
//
// requireTitle=true rejects an empty title (MCP add_note's pre-extraction
// behaviour). requireTitle=false allows an empty title, which
// POST /api/v1/notes relies on: the enrichment worker fills in the real
// title later rather than the server guessing one from content (spec §6.1).
//
// Returns (result, "") on success, or (nil, errMsg) on failure.
func Save(
	ctx context.Context,
	docs DocumentUpserter,
	chunks ChunkWriter,
	embed Embedder,
	sourceType model.SourceType,
	title, content, sourceID string,
	metadata map[string]any,
	doEmbed bool,
	requireTitle bool,
) (*Result, string) {
	if requireTitle && strings.TrimSpace(title) == "" {
		return nil, "title is required and must be non-empty"
	}
	if len(title) > MaxTitleBytes {
		return nil, fmt.Sprintf("title exceeds maximum size of %d bytes", MaxTitleBytes)
	}
	if strings.TrimSpace(content) == "" {
		return nil, "content is required and must be non-empty"
	}
	if len(content) > MaxContentBytes {
		return nil, fmt.Sprintf("content exceeds maximum size of %d bytes", MaxContentBytes)
	}

	if strings.TrimSpace(sourceID) == "" {
		sourceID = uuid.New().String()
	}

	// CollectedAt must be set explicitly; leaving it as the zero value
	// causes the note to sort to the bottom of ORDER BY collected_at DESC
	// queries (issue #87 deep-verify, carried over from handleAddNote).
	doc := &model.Document{
		SourceType:  sourceType,
		SourceID:    sourceID,
		Title:       strings.TrimSpace(title),
		Content:     content,
		Metadata:    metadata,
		Status:      "active",
		CollectedAt: time.Now().UTC(),
	}

	if err := docs.Upsert(ctx, doc); err != nil {
		slog.Error("note: upsert failed", "source_id", sourceID, "error", err)
		return nil, "internal error saving note"
	}

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
		slog.Error("note: chunk replace failed", "doc_id", doc.ID, "error", err)
		return nil, "internal error storing note chunks"
	}

	result := &Result{
		ID:            doc.ID.String(),
		ChunksCreated: len(chunkSlice),
	}

	if doEmbed && embed.Enabled() && len(chunkSlice) > 0 {
		embErr := embedChunks(ctx, doc.ID, chunkSlice, chunks, embed)
		switch {
		case embErr == nil:
			result.EmbeddingCreated = true
		case errors.Is(embErr, ErrEmbedSkipped):
			// Deliberate skip — EmbeddingCreated stays false; warning already
			// logged inside embedChunks.
		default:
			slog.Warn("note: embedding failed (non-fatal)", "doc_id", doc.ID, "error", embErr)
		}
	}

	return result, ""
}

// embedChunks generates and persists embedding vectors for the given chunks.
func embedChunks(
	ctx context.Context,
	docID uuid.UUID,
	chunkSlice []store.Chunk,
	chunkStore ChunkWriter,
	embedClient Embedder,
) error {
	if len(chunkSlice) > MaxEmbedChunks {
		slog.Warn("note: chunk count exceeds embed limit, skipping embedding",
			"doc_id", docID, "chunks", len(chunkSlice), "limit", MaxEmbedChunks)
		return ErrEmbedSkipped
	}

	texts := make([]string, 0, len(chunkSlice))
	for _, c := range chunkSlice {
		texts = append(texts, c.Content)
	}

	vectors, err := embedClient.EmbedBatch(ctx, texts)
	if err != nil {
		return fmt.Errorf("embed batch: %w", err)
	}

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
			slog.Warn("note: chunk index not found in stored chunks",
				"doc_id", docID, "chunk_index", chunkSlice[i].ChunkIndex)
			continue
		}
		embeddings = append(embeddings, store.ChunkEmbedding{ChunkID: id, Embedding: vec})
	}

	if len(embeddings) == 0 {
		return nil
	}
	return chunkStore.UpdateChunkEmbeddings(ctx, embeddings)
}
