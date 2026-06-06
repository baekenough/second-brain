// Package store provides PostgreSQL-backed persistence for documents and chunks.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	pgvector "github.com/pgvector/pgvector-go"
)

// Chunk represents one text segment of a document stored in the chunks table.
type Chunk struct {
	ID         int64
	DocumentID uuid.UUID
	ChunkIndex int
	Content    string
	ByteSize   int
	CreatedAt  time.Time
}

// ChunkSearchResult extends Chunk with rank/score and parent document metadata.
// Only the fields needed for API responses are included; the full document is
// not re-fetched to keep the query fast.
type ChunkSearchResult struct {
	Chunk

	// Rank is the ts_rank value from PostgreSQL for FTS results.
	Rank float64

	// Score is the cosine similarity for vector search results (1 - distance).
	// Zero when the result comes from FTS. Use whichever is non-zero, or fuse
	// both via RRF in the search layer.
	Score float64

	// Document metadata joined from the documents table.
	DocumentTitle  string
	DocumentSource string // source_type value (e.g. "slack", "github")
	DocumentStatus string
}

// ChunkStore provides chunk persistence and FTS search operations.
type ChunkStore struct {
	pg *Postgres
}

// NewChunkStore returns a ChunkStore backed by the given Postgres instance.
func NewChunkStore(pg *Postgres) *ChunkStore {
	return &ChunkStore{pg: pg}
}

// ReplaceDocument atomically replaces all chunks for documentID.
// It first deletes existing chunks for the document, then batch-inserts the
// provided chunks in a single transaction. If chunks is empty, only the delete
// is executed (effectively clearing chunks for the document).
func (s *ChunkStore) ReplaceDocument(ctx context.Context, documentID uuid.UUID, chunks []Chunk) error {
	tx, err := s.pg.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("chunks replace begin tx: %w", err)
	}
	defer func() {
		// Rollback is a no-op if the transaction has already been committed.
		_ = tx.Rollback(ctx)
	}()

	// Delete existing chunks for this document.
	if _, err := tx.Exec(ctx,
		`DELETE FROM chunks WHERE document_id = $1`, documentID,
	); err != nil {
		return fmt.Errorf("chunks replace delete for document %s: %w", documentID, err)
	}

	if len(chunks) == 0 {
		return tx.Commit(ctx)
	}

	// Batch insert using pgx CopyFrom for efficiency.
	rows := make([][]interface{}, 0, len(chunks))
	for _, c := range chunks {
		rows = append(rows, []interface{}{
			documentID,
			c.ChunkIndex,
			c.Content,
			c.ByteSize,
		})
	}

	_, err = tx.CopyFrom(
		ctx,
		pgx.Identifier{"chunks"},
		[]string{"document_id", "chunk_index", "content", "byte_size"},
		pgx.CopyFromRows(rows),
	)
	if err != nil {
		return fmt.Errorf("chunks replace insert for document %s: %w", documentID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("chunks replace commit for document %s: %w", documentID, err)
	}
	return nil
}

// SearchFTS performs full-text search across chunks using the 'simple' dictionary
// (matching the generated content_tsv column in the chunks table).
//
// Results are ordered by ts_rank DESC. Each row includes chunk content and
// parent document metadata joined from the documents table.
//
// Query plan notes:
//   - content_tsv @@ plainto_tsquery uses the GIN index idx_chunks_tsv.
//   - ts_rank is computed only for matching rows (post-filter).
//   - The JOIN on documents uses the primary key (idx scan).
func (s *ChunkStore) SearchFTS(ctx context.Context, query string, limit int) ([]ChunkSearchResult, error) {
	if limit <= 0 {
		limit = 20
	}

	const q = `
		SELECT
			c.id,
			c.document_id,
			c.chunk_index,
			c.content,
			c.byte_size,
			c.created_at,
			ts_rank(c.content_tsv, plainto_tsquery('simple', $1)) AS rank,
			d.title          AS document_title,
			d.source_type    AS document_source,
			d.status         AS document_status
		FROM chunks c
		JOIN documents d ON d.id = c.document_id
		WHERE c.content_tsv @@ plainto_tsquery('simple', $1)
		  AND d.status = 'active'
		ORDER BY rank DESC
		LIMIT $2`

	rows, err := s.pg.pool.Query(ctx, q, query, limit)
	if err != nil {
		return nil, fmt.Errorf("chunks search FTS: %w", err)
	}
	defer rows.Close()

	var results []ChunkSearchResult
	for rows.Next() {
		var r ChunkSearchResult
		if err := rows.Scan(
			&r.ID,
			&r.DocumentID,
			&r.ChunkIndex,
			&r.Content,
			&r.ByteSize,
			&r.CreatedAt,
			&r.Rank,
			&r.DocumentTitle,
			&r.DocumentSource,
			&r.DocumentStatus,
		); err != nil {
			return nil, fmt.Errorf("chunks search FTS scan: %w", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chunks search FTS iter: %w", err)
	}
	return results, nil
}

// ListByDocument returns all chunks for the given document ordered by
// chunk_index ascending. It is used by the scheduler to fetch chunk IDs after
// a ReplaceDocument call (CopyFrom does not return inserted IDs).
func (s *ChunkStore) ListByDocument(ctx context.Context, documentID uuid.UUID) ([]Chunk, error) {
	const q = `
		SELECT id, document_id, chunk_index, content, byte_size, created_at
		FROM chunks
		WHERE document_id = $1
		ORDER BY chunk_index ASC`

	rows, err := s.pg.pool.Query(ctx, q, documentID)
	if err != nil {
		return nil, fmt.Errorf("chunks list by document %s: %w", documentID, err)
	}
	defer rows.Close()

	var chunks []Chunk
	for rows.Next() {
		var c Chunk
		if err := rows.Scan(
			&c.ID,
			&c.DocumentID,
			&c.ChunkIndex,
			&c.Content,
			&c.ByteSize,
			&c.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("chunks list by document scan: %w", err)
		}
		chunks = append(chunks, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chunks list by document iter: %w", err)
	}
	return chunks, nil
}

// ChunkEmbedding pairs a chunk ID with its embedding vector.
// Used by UpdateChunkEmbeddings for batch persistence.
type ChunkEmbedding struct {
	ChunkID   int64
	Embedding []float32
}

// UpdateChunkEmbeddings persists embedding vectors for a batch of chunks in a
// single transaction. Each entry in embeddings identifies the chunk by its
// database ID and carries the embedding vector to store.
//
// Chunks with an empty embedding slice are silently skipped so that partial
// batch failures in the scheduler do not block the rest of the batch.
//
// This is the per-chunk analogue of DocumentStore.UpdateEmbedding.
func (s *ChunkStore) UpdateChunkEmbeddings(ctx context.Context, embeddings []ChunkEmbedding) error {
	if len(embeddings) == 0 {
		return nil
	}

	tx, err := s.pg.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("chunk update embeddings begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, ce := range embeddings {
		if len(ce.Embedding) == 0 {
			continue
		}
		if _, err := tx.Exec(ctx,
			`UPDATE chunks SET embedding = $1 WHERE id = $2`,
			pgvector.NewVector(ce.Embedding),
			ce.ChunkID,
		); err != nil {
			return fmt.Errorf("chunk update embedding id=%d: %w", ce.ChunkID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("chunk update embeddings commit: %w", err)
	}
	return nil
}

// SearchVector performs approximate nearest-neighbour (ANN) vector search over
// chunks using the HNSW index on chunks.embedding. Results are ordered by cosine
// similarity (highest first). Only active documents are included.
//
// queryVec must have the same dimension as the stored vectors; mismatches result
// in a pgvector error. The caller is responsible for truncating/padding if needed.
func (s *ChunkStore) SearchVector(ctx context.Context, queryVec []float32, limit int) ([]ChunkSearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	if len(queryVec) == 0 {
		return nil, fmt.Errorf("chunk vector search: empty query vector")
	}

	// cosine distance operator <=> returns 0 (identical) to 2 (opposite).
	// Score = 1 - distance maps it to [−1, 1] with 1 being perfect match.
	const q = `
		SELECT
			c.id,
			c.document_id,
			c.chunk_index,
			c.content,
			c.byte_size,
			c.created_at,
			1 - (c.embedding <=> $1::vector)  AS score,
			d.title          AS document_title,
			d.source_type    AS document_source,
			d.status         AS document_status
		FROM chunks c
		JOIN documents d ON d.id = c.document_id
		WHERE c.embedding IS NOT NULL
		  AND d.status = 'active'
		ORDER BY c.embedding <=> $1::vector
		LIMIT $2`

	rows, err := s.pg.pool.Query(ctx, q, pgvector.NewVector(queryVec), limit)
	if err != nil {
		return nil, fmt.Errorf("chunks search vector: %w", err)
	}
	defer rows.Close()

	var results []ChunkSearchResult
	for rows.Next() {
		var r ChunkSearchResult
		if err := rows.Scan(
			&r.ID,
			&r.DocumentID,
			&r.ChunkIndex,
			&r.Content,
			&r.ByteSize,
			&r.CreatedAt,
			&r.Score,
			&r.DocumentTitle,
			&r.DocumentSource,
			&r.DocumentStatus,
		); err != nil {
			return nil, fmt.Errorf("chunks search vector scan: %w", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chunks search vector iter: %w", err)
	}
	return results, nil
}
