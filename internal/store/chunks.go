// Package store provides PostgreSQL-backed persistence for documents and chunks.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/baekenough/second-brain/internal/model"
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

	// DocumentOccurredAt / DocumentCollectedAt are the parent document's event
	// and ingest times. They are selected here — rather than left to a second
	// fetch — because internal/search orders a merged result set in Go, and a
	// document reachable ONLY through a chunk lane never passes through the
	// document store's ORDER BY.
	//
	// Without them such a document has no usable timestamp at all, and
	// search.recencyKey ranks it LAST in both directions. For a forward-looking
	// window that is the exact inversion of what Sort="recent" means: the most
	// imminent entry is shown as the furthest away (#215).
	//
	// DocumentOccurredAt stays a pointer, and NULL stays nil: "no event-time
	// concept" and "the epoch" are different facts, and the COALESCE onto
	// collected_at belongs to the ordering rule, not to this row.
	DocumentOccurredAt  *time.Time
	DocumentCollectedAt time.Time

	// DocumentMetadata is the parent document's metadata jsonb, decoded to a
	// map. Selected for the same reason as DocumentOccurredAt above: a
	// document reachable ONLY through a chunk lane never passes through
	// hybridSearch's WHERE predicates, so internal/search's
	// applyRetentionExclusion and applyLowRetentionPenalty have nothing to
	// read unless the chunk row carries it directly.
	// nil (not an empty map) when the source column was NULL or failed to
	// decode — model.Document.RetentionTag treats nil the same as "no tag".
	DocumentMetadata map[string]any
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
	return s.SearchFTSFiltered(ctx, model.SearchQuery{Query: query}, limit)
}

// SearchFTSFiltered applies document eligibility before the chunk candidate LIMIT.
func (s *ChunkStore) SearchFTSFiltered(ctx context.Context, filter model.SearchQuery, limit int) ([]ChunkSearchResult, error) {
	query := filter.Query
	if limit <= 0 {
		limit = 20
	}

	// The WHERE clause uses both the tsvector (GIN idx_chunks_tsv) and the
	// pg_bigm LIKE condition (GIN idx_chunks_content_bigm) so that Korean
	// partial-match queries that do not produce tsquery tokens still hit the
	// bigm index (#146). The GREATEST() rank expression prefers the FTS rank
	// when both conditions match; the bigm lane adds a small constant (0.01)
	// so bigm-only matches rank above zero but well below true FTS hits.
	// 0.01 (not 0.1) avoids over-weighting bigm-only matches relative to the
	// ts_rank distribution, which typically ranges from 0.01 to ~0.5 (#146).
	args, filters := chunkEligibilitySQL([]interface{}{query, limit}, filter)
	q := `
		SELECT
			c.id,
			c.document_id,
			c.chunk_index,
			c.content,
			c.byte_size,
			c.created_at,
			GREATEST(
				ts_rank(c.content_tsv, plainto_tsquery('simple', $1)),
				CASE WHEN c.content LIKE '%%' || $1 || '%%' THEN 0.01 ELSE 0 END
			) AS rank,
			d.title          AS document_title,
			d.source_type    AS document_source,
			d.status         AS document_status,
			d.occurred_at    AS document_occurred_at,
			d.collected_at   AS document_collected_at,
			d.metadata       AS document_metadata
		FROM chunks c
		JOIN documents d ON d.id = c.document_id
		WHERE (c.content_tsv @@ plainto_tsquery('simple', $1)
		   OR c.content LIKE '%%' || $1 || '%%')
		  AND d.status = 'active' ` + filters + `
		ORDER BY rank DESC
		LIMIT $2`

	rows, err := s.pg.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("chunks search FTS: %w", err)
	}
	defer rows.Close()

	var results []ChunkSearchResult
	for rows.Next() {
		var r ChunkSearchResult
		var metaJSON []byte
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
			&r.DocumentOccurredAt,
			&r.DocumentCollectedAt,
			&metaJSON,
		); err != nil {
			return nil, fmt.Errorf("chunks search FTS scan: %w", err)
		}
		r.DocumentMetadata = decodeChunkDocumentMetadata(metaJSON)
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chunks search FTS iter: %w", err)
	}
	return results, nil
}

// decodeChunkDocumentMetadata decodes a chunk-lane row's joined
// documents.metadata jsonb column. A NULL column, an empty payload, or a
// malformed one all degrade to nil rather than failing the whole search
// lane — nil reads as "no retention tag" (model.Document.RetentionTag),
// which is the correct, conservative fallback: a decode failure must never
// be silently treated as retention="disposable".
func decodeChunkDocumentMetadata(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
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

// ListByDocuments 는 여러 문서의 청크를 한 번의 왕복으로 읽는다. 반환 컬럼과
// 문서 내 정렬(chunk_index 오름차순)은 ListByDocument 와 같다 — 리랭크 입력
// 선택(search.bestChunkText)이 동점일 때 인덱스가 작은 청크를 고르는 규칙이
// 이 순서에 기대므로, 두 경로의 순서가 달라지면 같은 질의의 순위가 바뀐다.
//
// 청크가 하나도 없는 문서는 맵에 키 자체가 없다(빈 슬라이스로 채우지 않는다).
// ids 가 비어 있으면 DB 를 건드리지 않고 빈 맵을 돌려준다. 중복 ID 는 ANY 가
// 알아서 한 번만 매칭하므로 호출자가 미리 걸러 둘 필요는 없다.
func (s *ChunkStore) ListByDocuments(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]Chunk, error) {
	out := make(map[uuid.UUID][]Chunk, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	const q = `
		SELECT id, document_id, chunk_index, content, byte_size, created_at
		FROM chunks
		WHERE document_id = ANY($1::uuid[])
		ORDER BY document_id, chunk_index ASC`

	rows, err := s.pg.pool.Query(ctx, q, ids)
	if err != nil {
		return nil, fmt.Errorf("chunks list by documents (%d ids): %w", len(ids), err)
	}
	defer rows.Close()

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
			return nil, fmt.Errorf("chunks list by documents scan: %w", err)
		}
		out[c.DocumentID] = append(out[c.DocumentID], c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chunks list by documents iter: %w", err)
	}
	return out, nil
}

// ChunkEmbedding pairs a chunk ID with its embedding vector.
// Used by UpdateChunkEmbeddings for batch persistence.
type ChunkEmbedding struct {
	ChunkID   int64
	Embedding []float32
	// Version 은 이 벡터를 만든 설정 식별자다(internal/search.EmbeddingVersion).
	// 빈 문자열이면 chunks.embedding_version 을 건드리지 않는다 — 버전 배선이
	// 없는 호출자의 동작을 그대로 두기 위해서다. 마이그레이션 037 참고.
	Version string
}

// updateEmbeddingsBatchSize is the maximum number of UPDATE statements per
// transaction in UpdateChunkEmbeddings. Kept small (50) to minimise lock-hold
// duration per transaction: shorter transactions reduce the window during which
// another writer (persistChunks → embedChunks path) can be blocked waiting for
// the same row locks, cutting the probability of a 40P01 deadlock cycle (#157).
const updateEmbeddingsBatchSize = 50

// UpdateChunkEmbeddings persists embedding vectors for a batch of chunks.
// When the batch exceeds updateEmbeddingsBatchSize entries, it is split into
// multiple transactions of at most updateEmbeddingsBatchSize UPDATEs each.
// Each sub-transaction commits independently; a failure in one sub-batch
// returns an error but does not roll back already-committed sub-batches.
//
// Chunks with an empty embedding slice are silently skipped so that partial
// batch failures in the scheduler do not block the rest of the batch.
//
// This is the per-chunk analogue of DocumentStore.UpdateEmbedding.
func (s *ChunkStore) UpdateChunkEmbeddings(ctx context.Context, embeddings []ChunkEmbedding) error {
	if len(embeddings) == 0 {
		return nil
	}

	for start := 0; start < len(embeddings); start += updateEmbeddingsBatchSize {
		end := start + updateEmbeddingsBatchSize
		if end > len(embeddings) {
			end = len(embeddings)
		}
		batch := embeddings[start:end]

		if err := s.updateEmbeddingsBatch(ctx, batch); err != nil {
			return err
		}
	}
	return nil
}

// updateEmbeddingsBatch persists a single sub-batch of embeddings in one
// transaction. It is called exclusively by UpdateChunkEmbeddings.
//
// Lock-order discipline (#157): rows are updated in ascending chunk ID order.
// The concurrent persistChunks → embedChunks path also reaches the same rows;
// sorting by ID here ensures both paths acquire row locks in the same direction,
// eliminating the circular wait that causes 40P01 deadlocks.
func (s *ChunkStore) updateEmbeddingsBatch(ctx context.Context, embeddings []ChunkEmbedding) error {
	// Sort by ChunkID ascending to establish a consistent lock-acquisition order.
	// This prevents circular waits with other writers that touch the same rows.
	sorted := make([]ChunkEmbedding, len(embeddings))
	copy(sorted, embeddings)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].ChunkID < sorted[j].ChunkID
	})

	tx, err := s.pg.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("chunk update embeddings begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, ce := range sorted {
		if len(ce.Embedding) == 0 {
			continue
		}
		// 벡터와 버전은 반드시 같은 UPDATE 에서 쓴다. 버전을 남기지 않으면
		// 재임베딩 선별 쿼리가 같은 청크를 영원히 다시 집어 간다.
		if _, err := tx.Exec(ctx,
			`UPDATE chunks
			 SET embedding = $1,
			     embedding_version = CASE WHEN $3 = '' THEN embedding_version ELSE $3 END
			 WHERE id = $2`,
			pgvector.NewVector(ce.Embedding),
			ce.ChunkID,
			ce.Version,
		); err != nil {
			return fmt.Errorf("chunk update embedding id=%d: %w", ce.ChunkID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("chunk update embeddings commit: %w", err)
	}
	return nil
}

// UnembeddedChunk is a minimal chunk record used for embedding backfill.
//
// 청크 본문(Content) 외에 소속 문서의 메타(SourceType/Title/OccurredAt/
// Metadata)도 함께 싣는다. 청크 임베딩 입력 앞에 붙일 문맥 헤더
// (scheduler.BuildChunkContextHeader)를 만들려면 "누가·언제·무슨 제목" 이
// 필요한데, 그 정보는 청크 행이 아니라 문서 행에 있기 때문이다. 어차피 활성
// 문서인지 보려고 documents 와 조인하고 있으므로 추가 쿼리 비용은 없다.
type UnembeddedChunk struct {
	ID         int64
	Content    string
	DocumentID uuid.UUID
	SourceType model.SourceType
	Title      string
	OccurredAt *time.Time
	Metadata   map[string]any
}

// ListUnembeddedChunks returns up to limit chunks whose embedding column is
// NULL, ordered by id ASC (primary key order) so backfill progresses in a
// stable direction that matches the lock-acquisition order used by
// updateEmbeddingsBatch (#157).
//
// FOR UPDATE SKIP LOCKED: rows that are currently locked by another writer
// (e.g. the persistChunks → embedChunks path racing on the same chunks) are
// skipped rather than waited on. Skipped rows will be picked up by the next
// backfill cycle, which is correct because backfill runs repeatedly.
// This eliminates the primary source of 40P01 deadlock cycles: the backfill
// reader no longer queues behind the inline writer on the same row locks.
//
// Only chunks belonging to active documents are included. Chunks for
// soft-deleted documents are excluded — re-embedding them would waste quota.
//
// This is the per-chunk analogue of DocumentStore.ListUnembedded (#141).
func (s *ChunkStore) ListUnembeddedChunks(ctx context.Context, limit int) ([]UnembeddedChunk, error) {
	return s.ListChunksNeedingEmbedding(ctx, limit, "")
}

// listChunksNeedingEmbeddingQuery 는 ListChunksNeedingEmbedding 이 실제로
// 실행하는 SQL 이다. 테스트가 사본이 아니라 이 상수를 직접 검사하도록 패키지
// 레벨로 꺼내 두었다 — 쿼리 안의 사본을 검사하는 테스트는 본문이 바뀌어도
// 계속 통과해서 검증 구실을 못 한다.
const listChunksNeedingEmbeddingQuery = `
		SELECT c.id, c.content, d.id, d.source_type, d.title, d.occurred_at, d.metadata
		FROM chunks c
		JOIN documents d ON d.id = c.document_id
		WHERE d.status = 'active'
		  AND (c.embedding IS NULL
		       OR ($2 <> '' AND c.embedding_version IS DISTINCT FROM $2))
		ORDER BY c.id ASC
		LIMIT $1
		FOR UPDATE OF c SKIP LOCKED`

// ListChunksNeedingEmbedding 은 "임베딩을 (다시) 만들어야 하는" 청크를 최대
// limit 건 돌려준다. 대상은 두 부류다:
//
//  1. embedding IS NULL — 아직 임베딩되지 않은 청크.
//  2. embedding_version 이 currentVersion 과 다른 청크 — 다른 모델/차원/입력
//     구성으로 만들어진 옛 벡터. NULL(마이그레이션 037 이전 레거시, 즉 문맥
//     헤더 없이 청크 본문만 임베딩한 벡터)도 여기 포함된다.
//
// currentVersion 이 빈 문자열이면 2번 조건을 적용하지 않는다 — 버전 배선이
// 없거나 재임베딩이 꺼진 배포에서 기존 동작을 그대로 두기 위한 기본값이다.
//
// 잠금·정렬 규약은 ListUnembeddedChunks 문서 주석과 동일하다
// (id ASC + FOR UPDATE OF c SKIP LOCKED).
func (s *ChunkStore) ListChunksNeedingEmbedding(ctx context.Context, limit int, currentVersion string) ([]UnembeddedChunk, error) {
	rows, err := s.pg.pool.Query(ctx, listChunksNeedingEmbeddingQuery, limit, currentVersion)
	if err != nil {
		return nil, fmt.Errorf("chunks list unembedded: %w", err)
	}
	defer rows.Close()

	var chunks []UnembeddedChunk
	for rows.Next() {
		var (
			c       UnembeddedChunk
			rawMeta []byte
		)
		if err := rows.Scan(&c.ID, &c.Content, &c.DocumentID, &c.SourceType,
			&c.Title, &c.OccurredAt, &rawMeta); err != nil {
			return nil, fmt.Errorf("chunks list unembedded scan: %w", err)
		}
		if len(rawMeta) > 0 {
			// 메타데이터가 깨져 있어도 백필 자체는 계속한다 — 헤더 일부가
			// 빠질 뿐이고, 임베딩을 통째로 건너뛰는 것보다 낫다.
			if err := json.Unmarshal(rawMeta, &c.Metadata); err != nil {
				slog.Warn("chunks: metadata unmarshal failed; header context will be partial",
					"chunk_id", c.ID,
					"document_id", c.DocumentID,
					"error", err,
				)
			}
		}
		chunks = append(chunks, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chunks list unembedded iter: %w", err)
	}
	return chunks, nil
}

// CountChunksNeedingEmbedding 은 재임베딩 대상 청크 수를 센다. 진행 상황
// 로그용이며 검색 경로에서는 호출하지 않는다.
func (s *ChunkStore) CountChunksNeedingEmbedding(ctx context.Context, currentVersion string) (int, error) {
	var n int
	err := s.pg.pool.QueryRow(ctx, `
		SELECT count(*)
		FROM chunks c
		JOIN documents d ON d.id = c.document_id
		WHERE d.status = 'active'
		  AND (c.embedding IS NULL
		       OR ($1 <> '' AND c.embedding_version IS DISTINCT FROM $1))`,
		currentVersion,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count chunks needing embedding: %w", err)
	}
	return n, nil
}

// SearchVector performs approximate nearest-neighbour (ANN) vector search over
// chunks using the HNSW index on chunks.embedding. Results are ordered by cosine
// similarity (highest first). Only active documents are included.
//
// queryVec must have the same dimension as the stored vectors; mismatches result
// in a pgvector error. The caller is responsible for truncating/padding if needed.
func (s *ChunkStore) SearchVector(ctx context.Context, queryVec []float32, limit int) ([]ChunkSearchResult, error) {
	return s.SearchVectorFiltered(ctx, model.SearchQuery{Embedding: queryVec}, limit)
}

// SearchVectorFiltered applies document eligibility before the ANN candidate LIMIT.
func (s *ChunkStore) SearchVectorFiltered(ctx context.Context, filter model.SearchQuery, limit int) ([]ChunkSearchResult, error) {
	queryVec := filter.Embedding
	if limit <= 0 {
		limit = 20
	}
	if len(queryVec) == 0 {
		return nil, fmt.Errorf("chunk vector search: empty query vector")
	}

	// cosine distance operator <=> returns 0 (identical) to 2 (opposite).
	// Score = 1 - distance maps it to [−1, 1] with 1 being perfect match.
	args, filters := chunkEligibilitySQL([]interface{}{pgvector.NewVector(queryVec), limit}, filter)
	q := `
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
			d.status         AS document_status,
			d.occurred_at    AS document_occurred_at,
			d.collected_at   AS document_collected_at,
			d.metadata       AS document_metadata
		FROM chunks c
		JOIN documents d ON d.id = c.document_id
		WHERE c.embedding IS NOT NULL
		  AND d.status = 'active' ` + filters + `
		ORDER BY c.embedding <=> $1::vector
		LIMIT $2`

	rows, err := s.pg.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("chunks search vector: %w", err)
	}
	defer rows.Close()

	var results []ChunkSearchResult
	for rows.Next() {
		var r ChunkSearchResult
		var metaJSON []byte
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
			&r.DocumentOccurredAt,
			&r.DocumentCollectedAt,
			&metaJSON,
		); err != nil {
			return nil, fmt.Errorf("chunks search vector scan: %w", err)
		}
		r.DocumentMetadata = decodeChunkDocumentMetadata(metaJSON)
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chunks search vector iter: %w", err)
	}
	return results, nil
}

// chunkEligibilitySQL shares the document lane's bound filter semantics.
func chunkEligibilitySQL(args []interface{}, q model.SearchQuery) ([]interface{}, string) {
	filters := ""
	if sources := q.IncludeSourceTypes(); len(sources) > 0 {
		args = append(args, sources)
		filters += fmt.Sprintf(" AND d.source_type = ANY($%d)", len(args))
	}
	if len(q.ExcludeSourceTypes) > 0 {
		args = append(args, q.ExcludeSourceTypes)
		filters += fmt.Sprintf(" AND d.source_type <> ALL($%d)", len(args))
	}
	var occurred, retention string
	args, _, occurred = appendOccurredRangeFilters(args, q.OccurredFrom, q.OccurredTo)
	args, _, retention = appendRetentionFilter(args, q.ExcludeRetention)
	return args, filters + " " + occurred + " " + retention
}
