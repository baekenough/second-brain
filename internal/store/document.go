package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
	"github.com/baekenough/second-brain/internal/model"
)

// DocumentStore provides document persistence and search operations.
type DocumentStore struct {
	pg *Postgres
}

// NewDocumentStore returns a DocumentStore backed by the given Postgres instance.
func NewDocumentStore(pg *Postgres) *DocumentStore {
	return &DocumentStore{pg: pg}
}

// Upsert inserts a document or updates it when (source_type, source_id) already exists.
// On conflict the status is reset to 'active' (handles re-appearance of previously deleted files).
func (s *DocumentStore) Upsert(ctx context.Context, doc *model.Document) error {
	meta, err := json.Marshal(doc.Metadata)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	var embeddingArg interface{}
	if len(doc.Embedding) > 0 {
		embeddingArg = pgvector.NewVector(doc.Embedding)
	}

	const q = `
		INSERT INTO documents
			(source_type, source_id, title, content, metadata, embedding, collected_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (source_type, source_id) DO UPDATE SET
			title        = EXCLUDED.title,
			content      = EXCLUDED.content,
			metadata     = EXCLUDED.metadata,
			embedding    = COALESCE(EXCLUDED.embedding, documents.embedding),
			collected_at = EXCLUDED.collected_at,
			status       = 'active',
			deleted_at   = NULL,
			updated_at   = now()
		RETURNING id, created_at, updated_at`

	row := s.pg.pool.QueryRow(ctx, q,
		doc.SourceType,
		doc.SourceID,
		doc.Title,
		doc.Content,
		meta,
		embeddingArg,
		doc.CollectedAt,
	)
	return row.Scan(&doc.ID, &doc.CreatedAt, &doc.UpdatedAt)
}

// GetByID retrieves a single document by primary key.
func (s *DocumentStore) GetByID(ctx context.Context, id uuid.UUID) (*model.Document, error) {
	const q = `
		SELECT id, source_type, source_id, title, content, metadata, embedding,
		       status, deleted_at, collected_at, created_at, updated_at
		FROM documents WHERE id = $1`

	row := s.pg.pool.QueryRow(ctx, q, id)
	doc, err := scanDocument(row)
	if err != nil {
		return nil, fmt.Errorf("get document %s: %w", id, err)
	}
	return doc, nil
}

// ListBySource returns active documents of a given source type, ordered by collected_at DESC.
// When src is empty, all active documents are returned regardless of source type.
func (s *DocumentStore) ListBySource(ctx context.Context, src model.SourceType, limit, offset int) ([]*model.Document, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if src == "" {
		const q = `
			SELECT id, source_type, source_id, title, content, metadata, embedding,
			       status, deleted_at, collected_at, created_at, updated_at
			FROM documents
			WHERE status = 'active'
			ORDER BY collected_at DESC
			LIMIT $1 OFFSET $2`
		rows, err = s.pg.pool.Query(ctx, q, limit, offset)
		if err != nil {
			return nil, fmt.Errorf("list documents: %w", err)
		}
	} else {
		const q = `
			SELECT id, source_type, source_id, title, content, metadata, embedding,
			       status, deleted_at, collected_at, created_at, updated_at
			FROM documents
			WHERE source_type = $1
			  AND status = 'active'
			ORDER BY collected_at DESC
			LIMIT $2 OFFSET $3`
		rows, err = s.pg.pool.Query(ctx, q, src, limit, offset)
		if err != nil {
			return nil, fmt.Errorf("list by source %q: %w", src, err)
		}
	}
	defer rows.Close()

	return collectDocuments(rows)
}

// ListRecent returns active documents ordered by collected_at DESC, with
// optional include/exclude source type filters.
//
// When includeSrc is non-empty, only documents of that source type are returned.
// excludeSrcs lists source types to omit from results; it is applied after
// includeSrc and may be empty.
func (s *DocumentStore) ListRecent(ctx context.Context, includeSrc model.SourceType, excludeSrcs []model.SourceType, limit, offset int) ([]*model.Document, error) {
	args := []interface{}{}

	var whereClauses []string
	whereClauses = append(whereClauses, "status = 'active'")

	if includeSrc != "" {
		args = append(args, includeSrc)
		whereClauses = append(whereClauses, fmt.Sprintf("source_type = $%d", len(args)))
	}

	if len(excludeSrcs) > 0 {
		args = append(args, excludeSrcs)
		whereClauses = append(whereClauses, fmt.Sprintf("source_type <> ALL($%d)", len(args)))
	}

	args = append(args, limit, offset)
	limitIdx := len(args) - 1
	offsetIdx := len(args)

	where := ""
	for i, clause := range whereClauses {
		if i == 0 {
			where = "WHERE " + clause
		} else {
			where += "\n  AND " + clause
		}
	}

	q := fmt.Sprintf(`
		SELECT id, source_type, source_id, title, content, metadata, embedding,
		       status, deleted_at, collected_at, created_at, updated_at
		FROM documents
		%s
		ORDER BY collected_at DESC
		LIMIT $%d OFFSET $%d`, where, limitIdx, offsetIdx)

	rows, err := s.pg.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list recent documents: %w", err)
	}
	defer rows.Close()

	return collectDocuments(rows)
}

// Search performs hybrid search using full-text and, when an embedding is
// provided, vector cosine similarity. Results are combined via Reciprocal
// Rank Fusion (RRF).
func (s *DocumentStore) Search(ctx context.Context, query model.SearchQuery) ([]*model.SearchResult, error) {
	if query.Limit <= 0 {
		query.Limit = 20
	}

	if len(query.Embedding) > 0 {
		return s.hybridSearch(ctx, query)
	}
	return s.fulltextSearch(ctx, query)
}

// sortOrder returns the ORDER BY clause for search queries.
// Only the whitelisted literal "recent" changes the order; all other values
// (including "" and "relevance") fall back to relevance (score DESC).
// This whitelist comparison prevents SQL injection.
func sortOrder(sort string) string {
	if sort == "recent" {
		return "collected_at DESC"
	}
	return "score DESC"
}

// fulltextSearch uses PostgreSQL ts_rank against the pre-computed tsvector column.
// pg_bigm LIKE matching is added as an OR condition so that Korean queries lacking
// morphological tsvector coverage are still retrieved via 2-gram index.
func (s *DocumentStore) fulltextSearch(ctx context.Context, query model.SearchQuery) ([]*model.SearchResult, error) {
	args := []interface{}{query.Query, query.Limit}

	statusFilter := "AND status = 'active'"
	if query.IncludeDeleted {
		statusFilter = ""
	}

	sourceFilter := ""
	if query.SourceType != nil {
		sourceFilter = fmt.Sprintf("AND source_type = $%d", len(args)+1)
		args = append(args, *query.SourceType)
	}

	excludeFilter := ""
	if len(query.ExcludeSourceTypes) > 0 {
		excludeFilter = fmt.Sprintf("AND source_type <> ALL($%d)", len(args)+1)
		args = append(args, query.ExcludeSourceTypes)
	}

	// The LIKE pattern uses SQL string concatenation ('%%' || $1 || '%%') so that
	// pg_bigm's gin_bigm_ops index is used automatically without embedding literal
	// percent signs in the Go format string (which would require '%%%%').
	q := fmt.Sprintf(`
		SELECT id, source_type, source_id, title, content, metadata, embedding,
		       status, deleted_at, collected_at, created_at, updated_at,
		       GREATEST(
		           ts_rank(tsv, plainto_tsquery('simple', $1)),
		           ts_rank(tsv, plainto_tsquery('english', $1))
		       ) AS score
		FROM documents
		WHERE (tsv @@ plainto_tsquery('simple', $1)
		   OR tsv @@ plainto_tsquery('english', $1)
		   OR content LIKE '%%' || $1 || '%%'
		   OR title   LIKE '%%' || $1 || '%%')
		%s
		%s
		%s
		ORDER BY %s
		LIMIT $2`, statusFilter, sourceFilter, excludeFilter, sortOrder(query.Sort))

	rows, err := s.pg.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("fulltext search: %w", err)
	}
	defer rows.Close()

	return collectResults(rows, "fulltext")
}

// hybridSearch combines full-text and vector search ranks via RRF.
func (s *DocumentStore) hybridSearch(ctx context.Context, query model.SearchQuery) ([]*model.SearchResult, error) {
	args := []interface{}{
		query.Query,
		pgvector.NewVector(query.Embedding),
		query.Limit * 2, // fetch more candidates before RRF merge
	}

	statusFilter := "AND status = 'active'"
	if query.IncludeDeleted {
		statusFilter = ""
	}

	sourceFilter := ""
	if query.SourceType != nil {
		sourceFilter = fmt.Sprintf("AND source_type = $%d", len(args)+1)
		args = append(args, *query.SourceType)
	}

	excludeFilter := ""
	if len(query.ExcludeSourceTypes) > 0 {
		excludeFilter = fmt.Sprintf("AND source_type <> ALL($%d)", len(args)+1)
		args = append(args, query.ExcludeSourceTypes)
	}

	// RRF formula: w / (k + rank), where w is the per-signal weight and k
	// prevents very high scores for top-ranked results (standard k=60).
	// Three CTEs (fts, vec, bigm) are merged via FULL OUTER JOIN. Each CTE shares
	// the same statusFilter/sourceFilter/excludeFilter snippets; args are appended
	// once and referenced by the same positional parameters in all three CTEs.
	// bigm uses pg_bigm's gin_bigm_ops index via LIKE '%%' || $1 || '%%'.
	// SQL '%%%%' in fmt.Sprintf produces a literal '%%' which pg_bigm needs.
	// Weight parameters are injected as Go-formatted literals (not SQL params)
	// because they are floats under our control, never from user input.
	w := query.Weights.Defaults()
	q := fmt.Sprintf(`
		WITH fts AS (
			SELECT id,
			       row_number() OVER (ORDER BY GREATEST(
			           ts_rank(tsv, plainto_tsquery('simple', $1)),
			           ts_rank(tsv, plainto_tsquery('english', $1))
			       ) DESC) AS rank
			FROM documents
			WHERE (tsv @@ plainto_tsquery('simple', $1)
			   OR tsv @@ plainto_tsquery('english', $1))
			%s
			%s
			%s
			LIMIT $3
		),
		vec AS (
			SELECT id,
			       row_number() OVER (ORDER BY embedding <=> $2 ASC) AS rank
			FROM documents
			WHERE embedding IS NOT NULL
			%s
			%s
			%s
			LIMIT $3
		),
		bigm AS (
			SELECT id,
			       row_number() OVER () AS rank
			FROM documents
			WHERE (content LIKE '%%%%' || $1 || '%%%%'
			    OR title   LIKE '%%%%' || $1 || '%%%%')
			%s
			%s
			%s
			LIMIT $3
		),
		rrf AS (
			SELECT
				COALESCE(fts.id, vec.id, bigm.id) AS id,
				COALESCE(%g/(%g + fts.rank),  0)
				+ COALESCE(%g/(%g + vec.rank),  0)
				+ COALESCE(%g/(%g + bigm.rank), 0) AS score
			FROM fts
			FULL OUTER JOIN vec  ON fts.id = vec.id
			FULL OUTER JOIN bigm ON COALESCE(fts.id, vec.id) = bigm.id
		)
		SELECT d.id, d.source_type, d.source_id, d.title, d.content, d.metadata,
		       d.embedding, d.status, d.deleted_at, d.collected_at, d.created_at, d.updated_at,
		       rrf.score
		FROM rrf
		JOIN documents d ON d.id = rrf.id
		ORDER BY %s
		LIMIT $3`,
		statusFilter, sourceFilter, excludeFilter,
		statusFilter, sourceFilter, excludeFilter,
		statusFilter, sourceFilter, excludeFilter,
		w.FTSWeight, w.RRFK,
		w.VecWeight, w.RRFK,
		w.BigmWeight, w.RRFK,
		sortOrder(query.Sort))

	rows, err := s.pg.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("hybrid search: %w", err)
	}
	defer rows.Close()

	results, err := collectResults(rows, "hybrid")
	if err != nil {
		return nil, err
	}
	if len(results) > query.Limit {
		results = results[:query.Limit]
	}
	return results, nil
}

// RecordCollectionLog writes a collection event to the collection_log table.
func (s *DocumentStore) RecordCollectionLog(ctx context.Context, sourceType model.SourceType, started time.Time, count int, collErr error) error {
	var errStr *string
	if collErr != nil {
		msg := collErr.Error()
		errStr = &msg
	}
	_, err := s.pg.pool.Exec(ctx, `
		INSERT INTO collection_log (source_type, started_at, finished_at, documents_count, error)
		VALUES ($1, $2, now(), $3, $4)`,
		sourceType, started, count, errStr,
	)
	return err
}

// LastCollectedAt returns the last collection watermark for the given
// (instance_id, source_type) pair, or the fallback when no row exists yet.
//
// Per-instance state decouples collectors that share a source_type (e.g.,
// filesystem scans on laptop, ubuntu1, ubuntu2) so one instance's recent scan
// cannot suppress older files seen by another instance.
func (s *DocumentStore) LastCollectedAt(ctx context.Context, instanceID string, src model.SourceType, fallback time.Time) time.Time {
	var t time.Time
	err := s.pg.pool.QueryRow(ctx,
		`SELECT last_collected_at FROM collector_state
		 WHERE instance_id = $1 AND source_type = $2`,
		instanceID, src,
	).Scan(&t)
	if err != nil || t.IsZero() {
		return fallback
	}
	return t
}

// UpdateCollectorState upserts the watermark for (instance_id, source_type).
// Callers should only invoke this on a successful collection cycle so that
// failed runs are retried from the previous watermark on the next tick.
func (s *DocumentStore) UpdateCollectorState(ctx context.Context, instanceID string, src model.SourceType, lastCollectedAt time.Time) error {
	_, err := s.pg.pool.Exec(ctx, `
		INSERT INTO collector_state (instance_id, source_type, last_collected_at, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (instance_id, source_type) DO UPDATE SET
			last_collected_at = EXCLUDED.last_collected_at,
			updated_at        = now()`,
		instanceID, src, lastCollectedAt,
	)
	if err != nil {
		return fmt.Errorf("update collector state (%s/%s): %w", instanceID, src, err)
	}
	return nil
}

// MarkDeleted marks documents as deleted for source IDs not present in activeIDs.
// Only documents with status 'active' are updated. Returns the number of rows updated.
func (s *DocumentStore) MarkDeleted(ctx context.Context, sourceType model.SourceType, activeIDs []string) (int, error) {
	tag, err := s.pg.pool.Exec(ctx, `
		UPDATE documents
		SET status = 'deleted', deleted_at = now()
		WHERE source_type = $1
		  AND status = 'active'
		  AND source_id != ALL($2)`,
		sourceType, activeIDs,
	)
	if err != nil {
		return 0, fmt.Errorf("mark deleted for %s: %w", sourceType, err)
	}
	return int(tag.RowsAffected()), nil
}

// CountBySource returns the number of active documents grouped by source_type.
// Deleted documents are excluded. The returned map is keyed by source_type string.
func (s *DocumentStore) CountBySource(ctx context.Context) (map[string]int, error) {
	const q = `
		SELECT source_type, COUNT(*)::bigint
		FROM documents
		WHERE status = 'active'
		GROUP BY source_type`

	rows, err := s.pg.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("count by source: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int, 4)
	for rows.Next() {
		var src string
		var n int64
		if err := rows.Scan(&src, &n); err != nil {
			return nil, fmt.Errorf("count by source scan: %w", err)
		}
		out[src] = int(n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("count by source iter: %w", err)
	}
	return out, nil
}

// --- baseline stats ---

// ContentLengthStats holds percentile and aggregate statistics for content length.
type ContentLengthStats struct {
	Mean float64 `json:"mean"`
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
	Max  int64   `json:"max"`
}

// DocumentSourceStats holds per-source aggregate document metrics.
type DocumentSourceStats struct {
	Count         int                `json:"count"`
	ContentLength ContentLengthStats `json:"content_length"`
}

// BaselineDocumentStats aggregates document-level baseline metrics.
type BaselineDocumentStats struct {
	Total      int                            `json:"total"`
	BySource   map[string]DocumentSourceStats `json:"by_source_type"`
}

// BaselineChunkStats aggregates chunk-level baseline metrics.
type BaselineChunkStats struct {
	Total                int64   `json:"total"`
	AvgChunksPerDocument float64 `json:"avg_chunks_per_document"`
	AvgChunkSizeBytes    float64 `json:"avg_chunk_size_bytes"`
}

// BaselineFailureStats aggregates extraction failure metrics.
type BaselineFailureStats struct {
	Open       int64          `json:"open"`
	DeadLetter int64          `json:"dead_letter"`
	BySource   map[string]int `json:"by_source_type"`
}

// BaselineCollectionStats holds the most recent collection timestamps per source.
type BaselineCollectionStats struct {
	MostRecentCollectedAt *time.Time         `json:"most_recent_collected_at"`
	BySource              map[string]*time.Time `json:"by_source_type"`
}

// BaselineStats is the top-level structure returned by the baseline stats query.
type BaselineStats struct {
	Documents          BaselineDocumentStats   `json:"documents"`
	Chunks             BaselineChunkStats      `json:"chunks"`
	ExtractionFailures BaselineFailureStats    `json:"extraction_failures"`
	Collection         BaselineCollectionStats `json:"collection"`
}

// QueryBaselineStats executes four independent queries and assembles BaselineStats.
// Each query is kept separate for readability and to avoid a single monstrous CTE.
func (s *DocumentStore) QueryBaselineStats(ctx context.Context) (*BaselineStats, error) {
	docStats, err := s.queryDocumentStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("baseline stats documents: %w", err)
	}

	chunkStats, err := s.queryChunkStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("baseline stats chunks: %w", err)
	}

	failureStats, err := s.queryFailureStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("baseline stats failures: %w", err)
	}

	collectionStats, err := s.queryCollectionStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("baseline stats collection: %w", err)
	}

	return &BaselineStats{
		Documents:          docStats,
		Chunks:             chunkStats,
		ExtractionFailures: failureStats,
		Collection:         collectionStats,
	}, nil
}

// queryDocumentStats returns per-source document counts and content-length percentiles.
func (s *DocumentStore) queryDocumentStats(ctx context.Context) (BaselineDocumentStats, error) {
	const q = `
		SELECT
			source_type,
			COUNT(*)::bigint                                                        AS cnt,
			AVG(LENGTH(content))::double precision                                  AS mean_len,
			PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY LENGTH(content))::double precision AS p50_len,
			PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY LENGTH(content))::double precision AS p95_len,
			MAX(LENGTH(content))::bigint                                            AS max_len
		FROM documents
		WHERE status = 'active'
		GROUP BY source_type`

	rows, err := s.pg.pool.Query(ctx, q)
	if err != nil {
		return BaselineDocumentStats{}, fmt.Errorf("document stats query: %w", err)
	}
	defer rows.Close()

	bySource := make(map[string]DocumentSourceStats)
	total := 0
	for rows.Next() {
		var (
			src                      string
			cnt                      int64
			meanLen, p50Len, p95Len  float64
			maxLen                   int64
		)
		if err := rows.Scan(&src, &cnt, &meanLen, &p50Len, &p95Len, &maxLen); err != nil {
			return BaselineDocumentStats{}, fmt.Errorf("document stats scan: %w", err)
		}
		bySource[src] = DocumentSourceStats{
			Count: int(cnt),
			ContentLength: ContentLengthStats{
				Mean: meanLen,
				P50:  p50Len,
				P95:  p95Len,
				Max:  maxLen,
			},
		}
		total += int(cnt)
	}
	if err := rows.Err(); err != nil {
		return BaselineDocumentStats{}, fmt.Errorf("document stats iter: %w", err)
	}

	return BaselineDocumentStats{Total: total, BySource: bySource}, nil
}

// queryChunkStats returns aggregate chunk metrics across all documents.
func (s *DocumentStore) queryChunkStats(ctx context.Context) (BaselineChunkStats, error) {
	const q = `
		SELECT
			COUNT(*)::bigint                               AS total_chunks,
			COALESCE(AVG(byte_size), 0)::double precision AS avg_chunk_size,
			COALESCE(
				COUNT(*)::double precision / NULLIF(COUNT(DISTINCT document_id), 0),
				0
			)                                              AS avg_per_doc
		FROM chunks`

	var stats BaselineChunkStats
	if err := s.pg.pool.QueryRow(ctx, q).Scan(
		&stats.Total,
		&stats.AvgChunkSizeBytes,
		&stats.AvgChunksPerDocument,
	); err != nil {
		return BaselineChunkStats{}, fmt.Errorf("chunk stats query: %w", err)
	}
	return stats, nil
}

// queryFailureStats returns open and dead-letter extraction failure counts per source.
func (s *DocumentStore) queryFailureStats(ctx context.Context) (BaselineFailureStats, error) {
	const q = `
		SELECT
			source_type,
			COUNT(*) FILTER (WHERE NOT dead_letter)::bigint AS open_cnt,
			COUNT(*) FILTER (WHERE dead_letter)::bigint     AS dead_cnt
		FROM extraction_failures
		GROUP BY source_type`

	rows, err := s.pg.pool.Query(ctx, q)
	if err != nil {
		return BaselineFailureStats{}, fmt.Errorf("failure stats query: %w", err)
	}
	defer rows.Close()

	bySource := make(map[string]int)
	var totalOpen, totalDead int64
	for rows.Next() {
		var (
			src              string
			openCnt, deadCnt int64
		)
		if err := rows.Scan(&src, &openCnt, &deadCnt); err != nil {
			return BaselineFailureStats{}, fmt.Errorf("failure stats scan: %w", err)
		}
		bySource[src] = int(openCnt + deadCnt)
		totalOpen += openCnt
		totalDead += deadCnt
	}
	if err := rows.Err(); err != nil {
		return BaselineFailureStats{}, fmt.Errorf("failure stats iter: %w", err)
	}

	return BaselineFailureStats{
		Open:       totalOpen,
		DeadLetter: totalDead,
		BySource:   bySource,
	}, nil
}

// queryCollectionStats returns the most recent collected_at per source type.
func (s *DocumentStore) queryCollectionStats(ctx context.Context) (BaselineCollectionStats, error) {
	const q = `
		SELECT source_type, MAX(collected_at)
		FROM documents
		WHERE status = 'active'
		GROUP BY source_type`

	rows, err := s.pg.pool.Query(ctx, q)
	if err != nil {
		return BaselineCollectionStats{}, fmt.Errorf("collection stats query: %w", err)
	}
	defer rows.Close()

	bySource := make(map[string]*time.Time)
	var mostRecent *time.Time
	for rows.Next() {
		var (
			src string
			ts  time.Time
		)
		if err := rows.Scan(&src, &ts); err != nil {
			return BaselineCollectionStats{}, fmt.Errorf("collection stats scan: %w", err)
		}
		t := ts // local copy for pointer
		bySource[src] = &t
		if mostRecent == nil || ts.After(*mostRecent) {
			mostRecent = &t
		}
	}
	if err := rows.Err(); err != nil {
		return BaselineCollectionStats{}, fmt.Errorf("collection stats iter: %w", err)
	}

	return BaselineCollectionStats{
		MostRecentCollectedAt: mostRecent,
		BySource:              bySource,
	}, nil
}

// ListActiveSourceIDs returns all source_ids for active documents of a given source type.
func (s *DocumentStore) ListActiveSourceIDs(ctx context.Context, sourceType model.SourceType) ([]string, error) {
	rows, err := s.pg.pool.Query(ctx, `
		SELECT source_id FROM documents
		WHERE source_type = $1 AND status = 'active'`,
		sourceType,
	)
	if err != nil {
		return nil, fmt.Errorf("list active source IDs for %s: %w", sourceType, err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// --- scan helpers ---

type scannable interface {
	Scan(dest ...interface{}) error
}

func scanDocument(row scannable) (*model.Document, error) {
	var (
		doc      model.Document
		metaJSON []byte
		vec      *pgvector.Vector
	)
	err := row.Scan(
		&doc.ID, &doc.SourceType, &doc.SourceID,
		&doc.Title, &doc.Content, &metaJSON, &vec,
		&doc.Status, &doc.DeletedAt,
		&doc.CollectedAt, &doc.CreatedAt, &doc.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(metaJSON, &doc.Metadata); err != nil {
		doc.Metadata = map[string]any{}
	}
	if vec != nil {
		doc.Embedding = vec.Slice()
	}
	return &doc, nil
}

func collectDocuments(rows pgx.Rows) ([]*model.Document, error) {
	var docs []*model.Document
	for rows.Next() {
		doc, err := scanDocument(rows)
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	return docs, rows.Err()
}

func collectResults(rows pgx.Rows, matchType string) ([]*model.SearchResult, error) {
	var results []*model.SearchResult
	for rows.Next() {
		var (
			r        model.SearchResult
			metaJSON []byte
			vec      *pgvector.Vector
		)
		err := rows.Scan(
			&r.ID, &r.SourceType, &r.SourceID,
			&r.Title, &r.Content, &metaJSON, &vec,
			&r.Status, &r.DeletedAt,
			&r.CollectedAt, &r.CreatedAt, &r.UpdatedAt,
			&r.Score,
		)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(metaJSON, &r.Metadata); err != nil {
			r.Metadata = map[string]any{}
		}
		if vec != nil {
			r.Embedding = vec.Slice()
		}
		r.MatchType = matchType
		results = append(results, &r)
	}
	return results, rows.Err()
}
