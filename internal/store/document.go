package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pgvector/pgvector-go"
	"github.com/baekenough/second-brain/internal/model"
)

// summaryCoverageCache is a process-level cache for SummaryCoverageRatio results.
// It avoids a per-query COUNT(*) on large tables while keeping the gate responsive
// to backfill progress. TTL is 60 s by default; see summaryCoverageTTL.
const summaryCoverageTTL = 60 * time.Second

// DocumentStore provides document persistence and search operations.
type DocumentStore struct {
	pg *Postgres

	// coverage cache fields — protects SummaryCoverageRatio from per-query scans.
	coverageMu        sync.Mutex
	coverageRatio     float64
	coverageFetchedAt time.Time
}

// NewDocumentStore returns a DocumentStore backed by the given Postgres instance.
func NewDocumentStore(pg *Postgres) *DocumentStore {
	return &DocumentStore{pg: pg}
}

// callTranscriptDupCheckQuery is the existence check used by Upsert to detect
// call-transcript documents that share identical content under a different
// source_id. The query is a SELECT 1 probe (LIMIT 1) so it avoids a full scan.
//
// Parameters:
//
//	$1 — content (text)
//	$2 — source_id to exclude (the current document's own source_id)
//
// The query intentionally targets only status='active' rows so that a previously
// soft-deleted duplicate does not block re-insertion of a renamed recording.
const callTranscriptDupCheckQuery = `
	SELECT 1
	FROM documents
	WHERE source_type = 'call-transcript'
	  AND status      = 'active'
	  AND content     = $1
	  AND source_id  <> $2
	LIMIT 1`

// UpsertTracked is identical to Upsert but additionally returns a bool that
// indicates whether the document's content actually changed. Callers that
// perform post-upsert work (chunking, embedding) can use this to skip
// expensive operations when a document arrives unchanged.
//
// Change detection uses a WITH CTE that captures the pre-update content in the
// same MVCC snapshot as the INSERT … ON CONFLICT statement, then compares it
// against the incoming value in RETURNING. This avoids two PostgreSQL pitfalls:
//   1. EXCLUDED cannot be referenced in the RETURNING clause (only valid inside
//      ON CONFLICT DO UPDATE SET/WHERE).
//   2. `documents.col` in RETURNING reflects the post-update value, so a naive
//      `documents.content IS DISTINCT FROM EXCLUDED.content` would always be
//      false after the UPDATE overwrites the column.
//
// *DocumentStore satisfies the api.IngestMessagesUpserter interface via this method.
func (s *DocumentStore) UpsertTracked(ctx context.Context, doc *model.Document) (contentChanged bool, err error) {
	// Duplicate guard: call-transcript content dedup (issue #134).
	if doc.SourceType == model.SourceCallTranscript {
		var exists int
		qErr := s.pg.pool.QueryRow(ctx, callTranscriptDupCheckQuery,
			doc.Content,
			doc.SourceID,
		).Scan(&exists)
		switch {
		case qErr == nil:
			slog.Info("store: skipping duplicate call-transcript",
				"source_id", doc.SourceID,
				"content_len", len(doc.Content),
			)
			return false, ErrDuplicateTranscript
		case isNoRows(qErr):
			// No duplicate — proceed with the normal upsert.
		default:
			return false, fmt.Errorf("call-transcript dup check: %w", qErr)
		}
	}

	meta, err := json.Marshal(doc.Metadata)
	if err != nil {
		return false, fmt.Errorf("marshal metadata: %w", err)
	}

	var embeddingArg interface{}
	if len(doc.Embedding) > 0 {
		embeddingArg = pgvector.NewVector(doc.Embedding)
	}

	// Change detection via CTE pre-update snapshot.
	//
	// Why CTE and not RETURNING + EXCLUDED:
	//   PostgreSQL does not allow EXCLUDED references in the RETURNING clause
	//   (only valid inside ON CONFLICT DO UPDATE SET/WHERE). Additionally,
	//   RETURNING evaluates after the UPDATE is applied, so `documents.content`
	//   in RETURNING would already hold the new (post-update) value — making an
	//   IS DISTINCT FROM comparison there always false.
	//
	// Solution: the `prev` CTE runs a SELECT in the same statement snapshot
	// (PostgreSQL CTE semantics: all CTEs and the main DML share one MVCC
	// snapshot), capturing the pre-update content before the INSERT/UPDATE
	// fires. On INSERT the `prev` row is empty → COALESCE(old_content, '') →
	// compared against incoming $4, which will differ → content_changed=true.
	// On UPDATE with identical content: old=new → content_changed=false.
	// On UPDATE with changed content: old≠new → content_changed=true.
	// xmax::text::bigint=0 signals a fresh INSERT (belt-and-suspenders: a new
	// row cannot have unchanged content anyway).
	const q = `
		WITH prev AS (
			SELECT content AS old_content
			FROM documents
			WHERE source_type = $1 AND source_id = $2
		)
		INSERT INTO documents
			(source_type, source_id, title, content, metadata, embedding, occurred_at, collected_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (source_type, source_id) DO UPDATE SET
			title        = EXCLUDED.title,
			content      = EXCLUDED.content,
			metadata     = EXCLUDED.metadata,
			embedding    = COALESCE(EXCLUDED.embedding, documents.embedding),
			occurred_at  = COALESCE(EXCLUDED.occurred_at, documents.occurred_at),
			collected_at = EXCLUDED.collected_at,
			status       = 'active',
			deleted_at   = NULL,
			updated_at   = now()
		RETURNING id, created_at, updated_at,
		          (xmax::text::bigint = 0) AS was_insert,
		          COALESCE((SELECT old_content FROM prev), '') IS DISTINCT FROM $4 AS content_changed`

	var wasInsert bool
	row := s.pg.pool.QueryRow(ctx, q,
		doc.SourceType,
		doc.SourceID,
		doc.Title,
		doc.Content,
		meta,
		embeddingArg,
		doc.OccurredAt,
		doc.CollectedAt,
	)
	if err := row.Scan(&doc.ID, &doc.CreatedAt, &doc.UpdatedAt, &wasInsert, &contentChanged); err != nil {
		return false, err
	}
	// Fresh INSERT: content is always new. The CTE comparison also yields true
	// in this case (prev is empty → COALESCE '' IS DISTINCT FROM $4), but we
	// guard explicitly so any empty-string edge case cannot produce a false skip.
	if wasInsert {
		contentChanged = true
	}
	return contentChanged, nil
}

// Upsert inserts a document or updates it when (source_type, source_id) already exists.
// On conflict the status is reset to 'active' (handles re-appearance of previously deleted files).
//
// Duplicate call-transcript guard: for source_type='call-transcript' only, a
// cheap pre-insert existence check is performed. When an active document with
// identical content but a DIFFERENT source_id already exists, the upsert is
// skipped and ErrDuplicateTranscript is returned (the caller may safely ignore
// or log it). A same-source_id re-upsert is NOT affected: the ON CONFLICT path
// handles it normally even when the content is identical.
func (s *DocumentStore) Upsert(ctx context.Context, doc *model.Document) error {
	// Duplicate guard: call-transcript content dedup (issue #134).
	// Only applied when source_type is 'call-transcript'. Other source types are
	// unaffected. Same-source_id re-upserts bypass this check because the query
	// excludes the document's own source_id; the ON CONFLICT path below handles them.
	if doc.SourceType == model.SourceCallTranscript {
		var exists int
		err := s.pg.pool.QueryRow(ctx, callTranscriptDupCheckQuery,
			doc.Content,
			doc.SourceID,
		).Scan(&exists)
		switch {
		case err == nil:
			// A duplicate row was found — skip the insert.
			slog.Info("store: skipping duplicate call-transcript",
				"source_id", doc.SourceID,
				"content_len", len(doc.Content),
			)
			return ErrDuplicateTranscript
		case isNoRows(err):
			// No duplicate — proceed with the normal upsert.
		default:
			return fmt.Errorf("call-transcript dup check: %w", err)
		}
	}

	meta, err := json.Marshal(doc.Metadata)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	var embeddingArg interface{}
	if len(doc.Embedding) > 0 {
		embeddingArg = pgvector.NewVector(doc.Embedding)
	}

	// occurred_at is the original event time (email date, calendar start, etc.).
	// NULL is stored when the collector has no event-time concept; COALESCE
	// in ORDER BY clauses falls back to collected_at for those rows.
	// On conflict we update occurred_at only when the incoming value is non-NULL
	// so that a re-collection without a timestamp does not erase a previously
	// parsed value.
	const q = `
		INSERT INTO documents
			(source_type, source_id, title, content, metadata, embedding, occurred_at, collected_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (source_type, source_id) DO UPDATE SET
			title        = EXCLUDED.title,
			content      = EXCLUDED.content,
			metadata     = EXCLUDED.metadata,
			embedding    = COALESCE(EXCLUDED.embedding, documents.embedding),
			occurred_at  = COALESCE(EXCLUDED.occurred_at, documents.occurred_at),
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
		doc.OccurredAt,
		doc.CollectedAt,
	)
	return row.Scan(&doc.ID, &doc.CreatedAt, &doc.UpdatedAt)
}

// ErrDuplicateTranscript is returned by Upsert when a call-transcript document
// with identical content already exists under a different source_id.
// Callers in the collector pipeline should log and skip this document; they
// MUST NOT treat it as a fatal error that aborts the entire collection cycle.
var ErrDuplicateTranscript = fmt.Errorf("store: call-transcript with identical content already exists (different source_id)")

// isNoRows reports whether err is a pgx "no rows" sentinel. Extracted to a
// helper so the Upsert guard remains readable without importing pgx directly.
func isNoRows(err error) bool {
	return err == pgx.ErrNoRows
}

// GetByID retrieves a single document by primary key.
func (s *DocumentStore) GetByID(ctx context.Context, id uuid.UUID) (*model.Document, error) {
	const q = `
		SELECT id, source_type, source_id, title, content, metadata, embedding,
		       status, deleted_at, occurred_at, collected_at, created_at, updated_at,
		       title_summary, bullet_summary, summary_embedding
		FROM documents WHERE id = $1`

	row := s.pg.pool.QueryRow(ctx, q, id)
	doc, err := scanDocument(row)
	if err != nil {
		return nil, fmt.Errorf("get document %s: %w", id, err)
	}
	return doc, nil
}

// ListBySource returns active documents of a given source type, ordered by the
// original event time (occurred_at) when available, falling back to collected_at
// for rows that have no event-time concept. NULLS LAST ensures untagged rows
// appear after all event-timestamped rows.
// When src is empty, all active documents are returned regardless of source type.
func (s *DocumentStore) ListBySource(ctx context.Context, src model.SourceType, limit, offset int) ([]*model.Document, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if src == "" {
		const q = `
			SELECT id, source_type, source_id, title, content, metadata, embedding,
			       status, deleted_at, occurred_at, collected_at, created_at, updated_at,
			       title_summary, bullet_summary, summary_embedding
			FROM documents
			WHERE status = 'active'
			ORDER BY COALESCE(occurred_at, collected_at) DESC
			LIMIT $1 OFFSET $2`
		rows, err = s.pg.pool.Query(ctx, q, limit, offset)
		if err != nil {
			return nil, fmt.Errorf("list documents: %w", err)
		}
	} else {
		const q = `
			SELECT id, source_type, source_id, title, content, metadata, embedding,
			       status, deleted_at, occurred_at, collected_at, created_at, updated_at,
			       title_summary, bullet_summary, summary_embedding
			FROM documents
			WHERE source_type = $1
			  AND status = 'active'
			ORDER BY COALESCE(occurred_at, collected_at) DESC
			LIMIT $2 OFFSET $3`
		rows, err = s.pg.pool.Query(ctx, q, src, limit, offset)
		if err != nil {
			return nil, fmt.Errorf("list by source %q: %w", src, err)
		}
	}
	defer rows.Close()

	return collectDocuments(rows)
}

// ListRecent returns active documents ordered by the original event time
// (occurred_at) when available, falling back to collected_at for rows that
// have no event-time concept. This ensures "most recent" reflects when the
// underlying event actually happened (email sent, call placed, etc.) rather
// than when second-brain ingested the document.
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
		       status, deleted_at, occurred_at, collected_at, created_at, updated_at,
		       title_summary, bullet_summary, summary_embedding
		FROM documents
		%s
		ORDER BY COALESCE(occurred_at, collected_at) DESC
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
//
// tableAlias is the SQL table alias used in the calling query ("d" for hybrid
// search which aliases documents as d, "" for fulltext search which uses the
// bare column names). When tableAlias is non-empty a dot-prefix is added.
//
// For "recent", we order by the original event time (occurred_at) when
// available, falling back to collected_at — the same COALESCE strategy used
// in ListRecent / ListBySource — so that "latest gmail" returns the most
// recently sent email, not the most recently ingested one.
func sortOrder(sort string, tableAlias string) string {
	prefix := ""
	if tableAlias != "" {
		prefix = tableAlias + "."
	}
	if sort == "recent" {
		return fmt.Sprintf("COALESCE(%soccurred_at, %scollected_at) DESC", prefix, prefix)
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
		       status, deleted_at, occurred_at, collected_at, created_at, updated_at,
		       title_summary, bullet_summary, summary_embedding,
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
		LIMIT $2`, statusFilter, sourceFilter, excludeFilter, sortOrder(query.Sort, ""))

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
	// Four CTEs (fts, vec, bigm, summvec) are merged via FULL OUTER JOIN.
	// Each CTE shares the same statusFilter/sourceFilter/excludeFilter snippets;
	// args are appended once and referenced by the same positional parameters.
	// bigm uses pg_bigm's gin_bigm_ops index via LIKE '%%' || $1 || '%%'.
	// SQL '%%%%' in fmt.Sprintf produces a literal '%%' which pg_bigm needs.
	// bigm lane ranks by GREATEST(bigm_similarity(content,$1), bigm_similarity(title,$1))
	// so Korean partial-match is ordered by real relevance, not document length (#138).
	// summvec uses the same query embedding ($2) as vec for consistency.
	// Weight parameters are injected as Go-formatted literals (not SQL params)
	// because they are floats under our control, never from user input.
	//
	// Coverage gate (#63): SummaryVec is disabled when summary_embedding coverage
	// is below the configured threshold (default 80%).  Without the gate, the
	// +26.7% score ceiling for summarised documents causes systematic ranking
	// bias during backfill — un-summarised documents are demoted even when they
	// are more content-relevant.  Once the corpus reaches the threshold the
	// weight activates automatically via the cached ratio (TTL 60 s).
	// Callers may force-enable by setting query.Weights.SummaryVec explicitly.
	w := query.Weights.Defaults()
	// Coverage gate: apply only when the caller has not explicitly set a weight
	// and has not explicitly disabled the summary-vec lane (#63).
	// - DisableSummaryVec=true → Defaults() already set w.SummaryVec=0.0; skip gate.
	// - SummaryVec>0 (explicit) → caller bypasses gate; use the value as-is.
	// - SummaryVec==0 (unset)   → apply gate: enable when corpus coverage is sufficient.
	if !query.Weights.DisableSummaryVec && query.Weights.SummaryVec == 0 {
		// Coverage gate: "decide based on corpus coverage".  Defaults() preserved
		// zero, so we resolve the effective weight here.
		//
		// - Coverage >= threshold → enable at DefaultSummaryVecWeight (0.8).
		// - Coverage <  threshold → disable (0.0) to prevent systematic demotion
		//   of un-summarised documents during backfill.
		// - Coverage query error  → disable (safe fallback; avoids bias on error).
		coverage, coverageErr := s.SummaryCoverageRatio(ctx)
		if coverageErr == nil && coverage >= model.SummaryVecCoverageThreshold() {
			w.SummaryVec = model.DefaultSummaryVecWeight
		} else {
			w.SummaryVec = 0.0
		}
	}

	// Entity lane gate (#139): enable when ENTITY_EXTRACTION_ENABLED env var is set.
	// EntityWeight<0 means explicit disable by caller; zero means "auto".
	// The lane is a no-op when entities aren't in the DB — FULL OUTER JOIN is safe.
	entityEnabled := entityExtractionEnabled()
	if w.EntityWeight == 0 {
		if entityEnabled {
			w.EntityWeight = model.DefaultEntityWeight
		}
		// else: leave 0 — entity CTE contributes 0 to RRF score (no-op).
	} else if w.EntityWeight < 0 {
		w.EntityWeight = 0 // explicit disable
	}

	// Build a normalised entity query: lower-cased, whitespace-trimmed tokens
	// joined by OR for the entity name LIKE match. This enables partial-name
	// matching (e.g. "홍길동" matches entity with normalized_name='홍길동').
	// The entity param position depends on how many args are already bound:
	// $4 when no source filter is active, $5 or $6 when source/exclude filters
	// push it further. The actual placeholder is computed via len(args)+1.
	entityFilterParam := ""
	if w.EntityWeight > 0 {
		entityFilterParam = fmt.Sprintf("$%d", len(args)+1)
		args = append(args, strings.ToLower(strings.TrimSpace(query.Query)))
	}

	// entityCTE is the SQL fragment for the entity lane. When the lane is
	// disabled (weight=0), we use a trivially-empty CTE to avoid syntax errors.
	entityCTE := `entity AS (SELECT NULL::uuid AS id, NULL::bigint AS rank WHERE false)`
	if w.EntityWeight > 0 {
		entityCTE = fmt.Sprintf(`entity AS (
			SELECT de.document_id AS id,
			       row_number() OVER (ORDER BY COUNT(*) DESC, de.document_id ASC) AS rank
			FROM document_entities de
			JOIN entities e ON e.id = de.entity_id
			WHERE e.normalized_name LIKE '%%%%' || %s || '%%%%'
			GROUP BY de.document_id
			LIMIT $3
		)`, entityFilterParam)
	}

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
			       row_number() OVER (ORDER BY
			           GREATEST(
			               bigm_similarity(content, $1),
			               bigm_similarity(title,   $1)
			           ) DESC, id ASC) AS rank
			FROM documents
			WHERE (content LIKE '%%%%' || $1 || '%%%%'
			    OR title   LIKE '%%%%' || $1 || '%%%%')
			%s
			%s
			%s
			LIMIT $3
		),
		summvec AS (
			SELECT id,
			       row_number() OVER (ORDER BY summary_embedding <=> $2 ASC) AS rank
			FROM documents
			WHERE summary_embedding IS NOT NULL
			%s
			%s
			%s
			LIMIT $3
		),
		%s,
		rrf AS (
			SELECT
				COALESCE(fts.id, vec.id, bigm.id, summvec.id, entity.id) AS id,
				COALESCE(%g/(%g + fts.rank),     0)
				+ COALESCE(%g/(%g + vec.rank),     0)
				+ COALESCE(%g/(%g + bigm.rank),    0)
				+ COALESCE(%g/(%g + summvec.rank), 0)
				+ COALESCE(%g/(%g + entity.rank),  0) AS score
			FROM fts
			FULL OUTER JOIN vec     ON fts.id = vec.id
			FULL OUTER JOIN bigm    ON COALESCE(fts.id, vec.id) = bigm.id
			FULL OUTER JOIN summvec ON COALESCE(fts.id, vec.id, bigm.id) = summvec.id
			FULL OUTER JOIN entity  ON COALESCE(fts.id, vec.id, bigm.id, summvec.id) = entity.id
		)
		SELECT d.id, d.source_type, d.source_id, d.title, d.content, d.metadata,
		       d.embedding, d.status, d.deleted_at, d.occurred_at, d.collected_at, d.created_at, d.updated_at,
		       d.title_summary, d.bullet_summary, d.summary_embedding,
		       rrf.score
		FROM rrf
		JOIN documents d ON d.id = rrf.id
		ORDER BY %s
		LIMIT $3`,
		statusFilter, sourceFilter, excludeFilter,
		statusFilter, sourceFilter, excludeFilter,
		statusFilter, sourceFilter, excludeFilter,
		statusFilter, sourceFilter, excludeFilter,
		entityCTE,
		w.FTSWeight, w.RRFK,
		w.VecWeight, w.RRFK,
		w.BigmWeight, w.RRFK,
		w.SummaryVec, w.RRFK,
		w.EntityWeight, w.RRFK,
		sortOrder(query.Sort, "d"))

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

// entityExtractionEnabled reports whether entity extraction has been enabled
// via the ENTITY_EXTRACTION_ENABLED environment variable (true / 1 / yes).
// This mirrors the check in cmd/collector/main.go so that the entity RRF lane
// automatically activates when the feature is running in the same deployment.
func entityExtractionEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("ENTITY_EXTRACTION_ENABLED")))
	return v == "true" || v == "1" || v == "yes"
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
// filesystem scans on laptop, host1, host2) so one instance's recent scan
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

// markDeletedEmptyGuardComment documents the belt-and-suspenders empty-slice
// guard added to MarkDeleted. An empty activeIDs slice passed to the Postgres
// query `source_id != ALL('{}')` evaluates as vacuously TRUE, which would
// soft-delete every active document for the source type — a silent data-loss
// path (see GitHub issue #135). The early-return no-op below prevents this.
// Callers (scheduler Layer 2) also guard against this via error propagation and
// deletion-ratio sanity checks; this guard is an additional safety net.
const markDeletedEmptyGuardComment = "empty activeIDs early-return no-op guard"

// MarkDeleted marks documents as deleted for source IDs not present in activeIDs.
// Only documents with status 'active' are updated. Returns the number of rows updated.
//
// Belt-and-suspenders: if activeIDs is empty, this function returns (0, nil)
// immediately without touching the database. An empty slice fed to the Postgres
// expression `source_id != ALL('{}')` is vacuously TRUE — it would delete every
// active document for the source type. The legitimate callers that intend a full
// wipe (if any ever exist) must use a dedicated, explicitly-named method rather
// than relying on an empty-slice side-effect.
func (s *DocumentStore) MarkDeleted(ctx context.Context, sourceType model.SourceType, activeIDs []string) (int, error) {
	if len(activeIDs) == 0 {
		// Empty slice guard: do NOT execute the query. See markDeletedEmptyGuardComment.
		return 0, nil
	}

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

// CountActiveDocuments returns the total number of active (non-deleted) documents
// for the given source type. It is used by the scheduler's deletion-ratio sanity
// check (issue #135) to detect a suspicious bulk-deletion before calling MarkDeleted.
// Implements scheduler.ActiveDocumentCounter.
func (s *DocumentStore) CountActiveDocuments(ctx context.Context, sourceType model.SourceType) (int, error) {
	var n int
	err := s.pg.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM documents
		WHERE source_type = $1 AND status = 'active'`,
		sourceType,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count active documents for %s: %w", sourceType, err)
	}
	return n, nil
}

// CountBySource returns the number of active documents grouped by logical source.
// Deleted documents are excluded. The returned map is keyed by logical source string.
//
// Legacy documents stored under source_type='secretary' are re-bucketed by their
// metadata 'kind' field (e.g. "sms", "gmail", "call-log", "call-transcript",
// "calendar"). This means historical SMS documents count toward the "sms" bucket
// alongside post-cutover documents — there is no double-counting because the two
// sets are date-disjoint. All other source_type values are reported as-is.
func (s *DocumentStore) CountBySource(ctx context.Context) (map[string]int, error) {
	const q = `
		SELECT
		  CASE
		    WHEN source_type = 'secretary'
		      THEN COALESCE(NULLIF(metadata->>'kind', ''), 'secretary')
		    ELSE source_type
		  END AS logical_source,
		  COUNT(*)::bigint
		FROM documents
		WHERE status = 'active'
		GROUP BY logical_source`

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

// ListUnembedded returns up to limit active documents whose embedding column is
// NULL, ordered by collected_at ASC (oldest first) so backfill progresses
// forward in time.
//
// Soft-deleted documents are excluded because they are never served in search
// results and re-embedding them would waste API quota.
func (s *DocumentStore) ListUnembedded(ctx context.Context, limit int) ([]*model.Document, error) {
	const q = `
		SELECT id, source_type, source_id, title, content, metadata, embedding,
		       status, deleted_at, occurred_at, collected_at, created_at, updated_at,
		       title_summary, bullet_summary, summary_embedding
		FROM documents
		WHERE embedding IS NULL
		  AND status = 'active'
		ORDER BY collected_at ASC
		LIMIT $1`

	rows, err := s.pg.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("list unembedded: %w", err)
	}
	defer rows.Close()

	return collectDocuments(rows)
}

// ListWithoutEntities returns up to limit active documents whose
// entities_processed_at column is NULL, ordered by collected_at ASC (oldest
// first) so that entity-extraction backfill progresses forward in time.
//
// Once the EntityWorker attempts extraction for a document — whether or not
// any entities are found — it calls MarkEntitiesProcessed to set the column,
// preventing the document from being re-queued on subsequent ticks.
//
// Soft-deleted documents are excluded — there is no value in extracting
// entities from documents that are not served in search results.
func (s *DocumentStore) ListWithoutEntities(ctx context.Context, limit int) ([]*model.Document, error) {
	const q = `
		SELECT id, source_type, source_id, title, content, metadata, embedding,
		       status, deleted_at, occurred_at, collected_at, created_at, updated_at,
		       title_summary, bullet_summary, summary_embedding
		FROM documents
		WHERE status = 'active'
		  AND entities_processed_at IS NULL
		ORDER BY collected_at ASC
		LIMIT $1`

	rows, err := s.pg.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("list without entities: %w", err)
	}
	defer rows.Close()

	return collectDocuments(rows)
}

// MarkEntitiesProcessed sets entities_processed_at to now() for the given
// document. After this call the document will no longer be returned by
// ListWithoutEntities, preventing the EntityWorker from re-queuing it on
// every tick when entity extraction consistently returns zero results.
func (s *DocumentStore) MarkEntitiesProcessed(ctx context.Context, documentID uuid.UUID) error {
	const q = `UPDATE documents SET entities_processed_at = now() WHERE id = $1 AND entities_processed_at IS NULL`
	if _, err := s.pg.pool.Exec(ctx, q, documentID); err != nil {
		return fmt.Errorf("mark entities processed %s: %w", documentID, err)
	}
	return nil
}

// ListUnsummarized returns up to limit active documents whose title_summary
// column is NULL, ordered by collected_at ASC (oldest first) so backfill
// progresses forward in time.
//
// Soft-deleted documents are excluded; there is no value in summarizing them.
func (s *DocumentStore) ListUnsummarized(ctx context.Context, limit int) ([]*model.Document, error) {
	const q = `
		SELECT id, source_type, source_id, title, content, metadata, embedding,
		       status, deleted_at, occurred_at, collected_at, created_at, updated_at,
		       title_summary, bullet_summary, summary_embedding
		FROM documents
		WHERE title_summary IS NULL
		  AND status = 'active'
		ORDER BY collected_at ASC
		LIMIT $1`

	rows, err := s.pg.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("list unsummarized: %w", err)
	}
	defer rows.Close()

	return collectDocuments(rows)
}

// SummaryCoverageRatio returns the fraction of active documents that have a
// summary_embedding (non-NULL).  The result is cached for summaryCoverageTTL
// (60 s) to avoid a per-search COUNT(*) on large tables.
//
// Used by hybridSearch to gate the SummaryVec signal (#63): when coverage is
// below model.SummaryVecCoverageThreshold() the weight is set to 0 so that
// un-summarised documents are not systematically demoted during backfill.
func (s *DocumentStore) SummaryCoverageRatio(ctx context.Context) (float64, error) {
	s.coverageMu.Lock()
	defer s.coverageMu.Unlock()

	if time.Since(s.coverageFetchedAt) < summaryCoverageTTL {
		return s.coverageRatio, nil
	}

	var ratio float64
	err := s.pg.pool.QueryRow(ctx, `
		SELECT COALESCE(
			COUNT(*) FILTER (WHERE summary_embedding IS NOT NULL)::float
			/ NULLIF(COUNT(*), 0),
		0)
		FROM documents
		WHERE status = 'active'`).Scan(&ratio)
	if err != nil {
		return 0, fmt.Errorf("summary coverage ratio: %w", err)
	}

	s.coverageRatio = ratio
	s.coverageFetchedAt = time.Now()
	return ratio, nil
}

// UpdateSummary writes the LLM-generated summary fields for a single document
// identified by its primary key. Only title_summary, bullet_summary, and
// summary_embedding are touched; other fields remain unchanged.
//
// summaryEmbedding may be nil when the embedder is disabled or failed — in
// that case the column is set to NULL, leaving the document out of
// summary-vector search until a subsequent run embeds it.
//
// Idempotency guard: the UPDATE is a no-op when title_summary is already set
// (WHERE title_summary IS NULL).  This prevents a racing concurrent instance
// from overwriting a completed summary with its own version (#64).
func (s *DocumentStore) UpdateSummary(ctx context.Context, id uuid.UUID, titleSummary, bulletSummary string, summaryEmbedding []float32) error {
	var vecArg interface{}
	if len(summaryEmbedding) > 0 {
		vecArg = pgvector.NewVector(summaryEmbedding)
	}
	_, err := s.pg.pool.Exec(ctx, `
		UPDATE documents
		SET title_summary     = $1,
		    bullet_summary    = $2,
		    summary_embedding = $3,
		    updated_at        = now()
		WHERE id = $4
		  AND title_summary IS NULL`,
		titleSummary,
		bulletSummary,
		vecArg,
		id,
	)
	if err != nil {
		return fmt.Errorf("update summary %s: %w", id, err)
	}
	return nil
}

// UpdateEmbedding writes the given embedding vector for a single document
// identified by its primary key. Only the embedding column is touched so that
// other fields (title, content, collected_at …) remain unchanged.
func (s *DocumentStore) UpdateEmbedding(ctx context.Context, doc *model.Document) error {
	if len(doc.Embedding) == 0 {
		return fmt.Errorf("UpdateEmbedding: empty embedding for document %s", doc.ID)
	}
	_, err := s.pg.pool.Exec(ctx, `
		UPDATE documents
		SET embedding = $1, updated_at = now()
		WHERE id = $2`,
		pgvector.NewVector(doc.Embedding),
		doc.ID,
	)
	if err != nil {
		return fmt.Errorf("update embedding %s: %w", doc.ID, err)
	}
	return nil
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

// ActiveSourceIDSet returns a set of all source_ids that are currently active
// for the given source type. The returned map is keyed by source_id and is
// safe to use for O(1) membership tests. It is used by the filesystem collector
// to detect files that are new (not yet indexed) regardless of their mtime.
func (s *DocumentStore) ActiveSourceIDSet(ctx context.Context, sourceType model.SourceType) (map[string]struct{}, error) {
	rows, err := s.pg.pool.Query(ctx, `
		SELECT source_id FROM documents
		WHERE source_type = $1 AND status = 'active'`,
		sourceType,
	)
	if err != nil {
		return nil, fmt.Errorf("active source ID set for %s: %w", sourceType, err)
	}
	defer rows.Close()

	set := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		set[id] = struct{}{}
	}
	return set, rows.Err()
}

// --- scan helpers ---

type scannable interface {
	Scan(dest ...interface{}) error
}

func scanDocument(row scannable) (*model.Document, error) {
	var (
		doc          model.Document
		metaJSON     []byte
		vec          *pgvector.Vector
		titleSum     pgtype.Text
		bulletSum    pgtype.Text
		summVec      *pgvector.Vector
	)
	err := row.Scan(
		&doc.ID, &doc.SourceType, &doc.SourceID,
		&doc.Title, &doc.Content, &metaJSON, &vec,
		&doc.Status, &doc.DeletedAt,
		&doc.OccurredAt, &doc.CollectedAt, &doc.CreatedAt, &doc.UpdatedAt,
		&titleSum, &bulletSum, &summVec,
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
	// pgtype.Text: NULL columns arrive as Valid=false; avoid assigning zero string
	// from a non-pointer Scan which pgx v5 rejects for nullable text columns.
	if titleSum.Valid {
		doc.TitleSummary = titleSum.String
	}
	if bulletSum.Valid {
		doc.BulletSummary = bulletSum.String
	}
	if summVec != nil {
		doc.SummaryEmbedding = summVec.Slice()
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
			r         model.SearchResult
			metaJSON  []byte
			vec       *pgvector.Vector
			titleSum  pgtype.Text
			bulletSum pgtype.Text
			summVec   *pgvector.Vector
		)
		err := rows.Scan(
			&r.ID, &r.SourceType, &r.SourceID,
			&r.Title, &r.Content, &metaJSON, &vec,
			&r.Status, &r.DeletedAt,
			&r.OccurredAt, &r.CollectedAt, &r.CreatedAt, &r.UpdatedAt,
			&titleSum, &bulletSum, &summVec,
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
		if titleSum.Valid {
			r.TitleSummary = titleSum.String
		}
		if bulletSum.Valid {
			r.BulletSummary = bulletSum.String
		}
		if summVec != nil {
			r.SummaryEmbedding = summVec.Slice()
		}
		r.MatchType = matchType
		results = append(results, &r)
	}
	return results, rows.Err()
}
