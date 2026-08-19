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

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pgvector/pgvector-go"
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
//  1. EXCLUDED cannot be referenced in the RETURNING clause (only valid inside
//     ON CONFLICT DO UPDATE SET/WHERE).
//  2. `documents.col` in RETURNING reflects the post-update value, so a naive
//     `documents.content IS DISTINCT FROM EXCLUDED.content` would always be
//     false after the UPDATE overwrites the column.
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
// "recent" means NEAREST TO NOW FIRST, and which SQL clause expresses that
// depends on the event-time window. This function does NOT decide that: both
// the whitelist test and the direction come from model.SearchQuery
// (SortsByRecency / RecencyAscending), which is the single definition of the
// rule and carries its full rationale.
//
// It has to be shared rather than restated because this clause is not the only
// place the order is applied: internal/search re-establishes the same order in
// Go over result sets it assembled itself (store results fused with chunk-lane
// candidates, which never passed through this ORDER BY). Two copies of the rule
// would order those two result shapes differently, and nothing in the response
// distinguishes them.
//
// The historical DESC branch keeps its COALESCE onto collected_at — the same
// strategy as ListRecent / ListBySource, so "latest gmail" returns the most
// recently sent email rather than the most recently ingested one.
//
// The ASC branch does not COALESCE. Whenever either bound is set the lanes
// already exclude occurred_at IS NULL (see appendOccurredRangeFilters), so on
// this branch occurred_at is non-NULL by construction and coalescing would only
// re-admit collected_at as a sort key for rows that cannot be in the result.
//
// Injection safety is unchanged and must stay that way: `sort` is compared
// against the exact literal "recent" and never reaches the output, the window
// is read for its direction only (its values are bound as parameters
// elsewhere), and every returned clause remains a compile-time constant modulo
// the caller-supplied alias. A non-whitelisted Sort still collapses to
// "score DESC" regardless of the window.
func sortOrder(query model.SearchQuery, now time.Time, tableAlias string) string {
	if !query.SortsByRecency() {
		return "score DESC"
	}

	prefix := ""
	if tableAlias != "" {
		prefix = tableAlias + "."
	}

	if query.RecencyAscending(now) {
		return fmt.Sprintf("%soccurred_at ASC", prefix)
	}
	return fmt.Sprintf("COALESCE(%soccurred_at, %scollected_at) DESC", prefix, prefix)
}

// fulltextSearch uses PostgreSQL ts_rank against the pre-computed tsvector column.
// pg_bigm LIKE matching is added as an OR condition so that Korean queries lacking
// morphological tsvector coverage are still retrieved via 2-gram index.
func (s *DocumentStore) fulltextSearch(ctx context.Context, query model.SearchQuery) ([]*model.SearchResult, error) {
	q, args := buildFulltextSearchQuery(query)

	rows, err := s.pg.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("fulltext search: %w", err)
	}
	defer rows.Close()

	return collectResults(rows, "fulltext")
}

// buildFulltextSearchQuery renders the non-vector search statement and its
// positional arguments. Split out of fulltextSearch (which only executes it) so
// the WHERE predicates — status, source type, and the occurred_at window — can
// be asserted without a database, the same way buildEntityCTE is.
func buildFulltextSearchQuery(query model.SearchQuery) (string, []interface{}) {
	args := []interface{}{query.Query, query.Limit}

	statusFilter := "AND status = 'active'"
	if query.IncludeDeleted {
		statusFilter = ""
	}

	// Include filter: set membership over the effective include set (see
	// model.SearchQuery.IncludeSourceTypes), bound as ONE parameter. The list
	// is never interpolated — every fragment in this file is assembled with
	// fmt.Sprintf, and that mechanism has already caused one production
	// incident here.
	sourceFilter := ""
	if include := query.IncludeSourceTypes(); len(include) > 0 {
		sourceFilter = fmt.Sprintf("AND source_type = ANY($%d)", len(args)+1)
		args = append(args, include)
	}

	excludeFilter := ""
	if len(query.ExcludeSourceTypes) > 0 {
		excludeFilter = fmt.Sprintf("AND source_type <> ALL($%d)", len(args)+1)
		args = append(args, query.ExcludeSourceTypes)
	}

	// Event-time window, applied in the WHERE clause for the same reason as in
	// hybridSearch: this statement is capped by LIMIT $2, so the window has to
	// constrain what is selected, not what is returned after selection.
	var occurredFilter string
	args, occurredFilter, _ = appendOccurredRangeFilters(args, query.OccurredFrom, query.OccurredTo)

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
		%s
		ORDER BY %s
		LIMIT $2`, statusFilter, sourceFilter, excludeFilter, occurredFilter, sortOrder(query, time.Now(), ""))

	return q, args
}

// appendOccurredRangeFilters binds the caller's event-time window to args and
// returns the matching WHERE fragments in both the unqualified form (for the
// CTEs that scan `documents` directly) and the `d.`-qualified form (for the
// entity lane, which joins it). Both forms reference the SAME positional
// parameters, so the two must always be produced by this one call.
//
// Three properties are deliberate and load-bearing:
//
//   - The bounds are bound as parameters, never interpolated. Every other
//     fragment in this file is assembled with fmt.Sprintf; a timestamp spliced
//     into the string would be an injection surface and a parse hazard.
//   - The comparison is against occurred_at alone, not
//     COALESCE(occurred_at, collected_at). A large share of the corpus has a
//     NULL occurred_at, and coalescing would reinterpret "ingested during the
//     window" as "happened during the window" — precisely the noise the window
//     exists to remove. Rows with a NULL occurred_at are therefore excluded,
//     which the SQL gets for free: a comparison against NULL is never true.
//   - The window is half-open, [from, to). Consecutive windows tile without
//     overlap and a document at exactly `to` belongs only to the next one.
//
// Either bound may be nil; both nil returns empty fragments and args unchanged.
func appendOccurredRangeFilters(args []interface{}, from, to *time.Time) (newArgs []interface{}, plain, qualified string) {
	var plainParts, qualifiedParts []string

	if from != nil {
		p := len(args) + 1
		plainParts = append(plainParts, fmt.Sprintf("AND occurred_at >= $%d", p))
		qualifiedParts = append(qualifiedParts, fmt.Sprintf("AND d.occurred_at >= $%d", p))
		args = append(args, *from)
	}
	if to != nil {
		p := len(args) + 1
		plainParts = append(plainParts, fmt.Sprintf("AND occurred_at < $%d", p))
		qualifiedParts = append(qualifiedParts, fmt.Sprintf("AND d.occurred_at < $%d", p))
		args = append(args, *to)
	}

	const sep = "\n\t\t\t"
	return args, strings.Join(plainParts, sep), strings.Join(qualifiedParts, sep)
}

// buildOccurredRangeIDQuery renders the statement that narrows a candidate set
// of document IDs to those whose occurred_at falls inside [from, to), and the
// bound arguments for the window. The ID list itself is bound as $1 by the
// caller, so the returned args start at $2.
//
// Split out for the same reason as the other builders in this file: the WHERE
// clause is the whole content of the function, and it is the only thing worth
// asserting without a database.
//
// It reuses appendOccurredRangeFilters so the window semantics are defined
// exactly once. That matters more here than anywhere else: this statement
// verifies candidates that a DIFFERENT lane produced. If its notion of the
// window drifted from the lanes' — half-open vs closed, or a COALESCE onto
// collected_at — the two halves of one result set would be answering different
// questions, and the difference would be invisible in the response.
func buildOccurredRangeIDQuery(from, to *time.Time) (string, []interface{}) {
	// One placeholder is already consumed by the ID list ($1).
	args, plain, _ := appendOccurredRangeFilters([]interface{}{nil}, from, to)
	return fmt.Sprintf(`
		SELECT id
		FROM documents
		WHERE id = ANY($1)
		%s`, plain), args[1:]
}

// FilterIDsByOccurredRange returns the subset of ids whose documents.occurred_at
// falls inside the half-open window [from, to). Rows with a NULL occurred_at
// never survive a window — the SQL comparison gives that for free, and it is
// the same rule every retrieval lane applies.
//
// This exists so that the search service's chunk lanes can honour an event-time
// window. Chunk rows carry no occurred_at, so before this the only safe
// behaviour was to switch those lanes off whenever a window was set, which
// deleted chunk-level recall from every temporal query. Verifying the candidate
// IDs against the documents table costs one primary-key lookup per windowed
// request (at most `limit` ids after per-document deduplication) and keeps both
// properties: chunk recall and a window the result provably satisfies.
//
// An empty ids slice short-circuits without touching the database.
func (s *DocumentStore) FilterIDsByOccurredRange(ctx context.Context, ids []uuid.UUID, from, to *time.Time) (map[uuid.UUID]struct{}, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]struct{}{}, nil
	}

	q, boundArgs := buildOccurredRangeIDQuery(from, to)
	args := make([]interface{}, 0, len(boundArgs)+1)
	args = append(args, ids)
	args = append(args, boundArgs...)

	rows, err := s.pg.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("filter ids by occurred range: %w", err)
	}
	defer rows.Close()

	out := make(map[uuid.UUID]struct{}, len(ids))
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("filter ids by occurred range scan: %w", err)
		}
		out[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("filter ids by occurred range iter: %w", err)
	}
	return out, nil
}

// hybridSearch combines full-text and vector search ranks via RRF.
func (s *DocumentStore) hybridSearch(ctx context.Context, query model.SearchQuery) ([]*model.SearchResult, error) {
	w, err := s.resolveWeights(ctx, query)
	if err != nil {
		return nil, err
	}

	q, args := buildHybridSearchQuery(query, w)

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

// resolveWeights turns the caller-supplied (possibly zero) weights into the
// effective per-lane weights, applying the two gates that need runtime state:
// the summary-vec coverage gate (a DB query) and the entity-lane env gate.
// Kept separate from buildHybridSearchQuery so that query construction stays a
// pure function of (query, weights) and is testable without a database.
//
// The error return is currently always nil — a failing coverage query degrades
// to "lane disabled" rather than failing the search — but is kept so a future
// hard failure has somewhere to go.
func (s *DocumentStore) resolveWeights(ctx context.Context, query model.SearchQuery) (model.SearchWeights, error) {
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
	if w.EntityWeight == 0 {
		if entityExtractionEnabled() {
			w.EntityWeight = model.DefaultEntityWeight
		}
		// else: leave 0 — entity CTE contributes 0 to RRF score (no-op).
	} else if w.EntityWeight < 0 {
		w.EntityWeight = 0 // explicit disable
	}

	return w, nil
}

// buildHybridSearchQuery renders the five-lane RRF statement and its positional
// arguments for the given query and already-resolved weights.
//
// Split out of hybridSearch so every lane's WHERE predicates are assertable
// without a database. That matters most for filters that constrain the
// candidate pool: each lane is capped by LIMIT $3, so a filter missing from one
// lane does not merely widen that lane — it lets out-of-scope documents consume
// candidate slots and push in-scope documents out of the fused result entirely.
func buildHybridSearchQuery(query model.SearchQuery, w model.SearchWeights) (string, []interface{}) {
	args := []interface{}{
		query.Query,
		pgvector.NewVector(query.Embedding),
		query.Limit * 2, // fetch more candidates before RRF merge
	}

	// Each filter is built twice: once unqualified for the four CTEs that scan
	// `documents` directly, and once qualified for the entity CTE, which joins
	// `documents` rather than scanning it. Same positional parameters, same
	// semantics — only the column prefix differs. Keeping the pairs adjacent is
	// deliberate: the entity lane previously applied NONE of these filters and
	// the outer join re-applied nothing, so a document reachable only through
	// the entity lane bypassed both the soft-delete filter and every source
	// type exclusion, insight documents included.
	statusFilter, entityStatusFilter := "AND status = 'active'", "AND d.status = 'active'"
	if query.IncludeDeleted {
		statusFilter, entityStatusFilter = "", ""
	}

	// Include filter: set membership over the effective include set (see
	// model.SearchQuery.IncludeSourceTypes). One bound parameter, referenced by
	// all five lanes — a lane that omitted it would not merely widen itself, it
	// would spend candidate slots (LIMIT $3) on documents the caller excluded.
	sourceFilter, entitySourceFilter := "", ""
	if include := query.IncludeSourceTypes(); len(include) > 0 {
		p := len(args) + 1
		sourceFilter = fmt.Sprintf("AND source_type = ANY($%d)", p)
		entitySourceFilter = fmt.Sprintf("AND d.source_type = ANY($%d)", p)
		args = append(args, include)
	}

	excludeFilter, entityExcludeFilter := "", ""
	if len(query.ExcludeSourceTypes) > 0 {
		p := len(args) + 1
		excludeFilter = fmt.Sprintf("AND source_type <> ALL($%d)", p)
		entityExcludeFilter = fmt.Sprintf("AND d.source_type <> ALL($%d)", p)
		args = append(args, query.ExcludeSourceTypes)
	}

	// Event-time window. Like the source filters, this constrains the candidate
	// pool INSIDE every lane rather than reordering the fused result: each lane
	// is capped by LIMIT $3, so an out-of-window document that reaches a lane
	// consumes a candidate slot an in-window document needed. Sorting by
	// recency (sortOrder) cannot substitute — it only permutes what the lanes
	// already selected.
	var occurredFilter, entityOccurredFilter string
	args, occurredFilter, entityOccurredFilter = appendOccurredRangeFilters(args, query.OccurredFrom, query.OccurredTo)

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
	// Coverage gate (#63) and entity gate (#139) are resolved by resolveWeights
	// before this function is called; w is the effective per-lane weighting.
	//
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
	entityCTE := emptyEntityCTE
	if w.EntityWeight > 0 {
		entityCTE = buildEntityCTE(entityFilterParam, entityStatusFilter, entitySourceFilter, entityExcludeFilter, entityOccurredFilter)
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
			%s
			LIMIT $3
		),
		%s,
		rrf AS (
			SELECT
				COALESCE(fts.id, vec.id, bigm.id, summvec.id, entity.id) AS id,
				%s AS score
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
		statusFilter, sourceFilter, excludeFilter, occurredFilter, // fts
		statusFilter, sourceFilter, excludeFilter, occurredFilter, // vec
		statusFilter, sourceFilter, excludeFilter, occurredFilter, // bigm
		statusFilter, sourceFilter, excludeFilter, occurredFilter, // summvec
		entityCTE, // entity lane carries the same filters in d.-qualified form
		buildRRFScoreExpr(w),
		sortOrder(query, time.Now(), "d"))

	return q, args
}

// buildRRFScoreExpr renders the weighted-sum SQL expression that fuses the
// five per-lane ranks (fts, vec, bigm, summvec, entity) into a single RRF
// relevance score.
//
// Every weight/k value is explicitly cast to ::float8. Go's "%g" verb formats
// a whole-number float64 without a decimal point (1.0 -> "1", 60.0 -> "60"),
// and the default weights (FTS/Vec/Bigm=1.0, RRFK=60.0) all format this way.
// Without the cast, PostgreSQL parses "1/(60 + fts.rank)" as `integer`
// division: since rank is always >= 1, the denominator is always >= 61,
// strictly greater than the numerator, so the result truncates to 0 for every
// row. That collapsed the fts/vec/bigm lanes' contribution to `score` to zero
// on every hybridSearch call, leaving the final ORDER BY with no real
// relevance signal for the vast majority of the corpus.
func buildRRFScoreExpr(w model.SearchWeights) string {
	return fmt.Sprintf(
		`COALESCE(%g::float8/(%g::float8 + fts.rank),     0)
				+ COALESCE(%g::float8/(%g::float8 + vec.rank),     0)
				+ COALESCE(%g::float8/(%g::float8 + bigm.rank),    0)
				+ COALESCE(%g::float8/(%g::float8 + summvec.rank), 0)
				+ COALESCE(%g::float8/(%g::float8 + entity.rank),  0)`,
		w.FTSWeight, w.RRFK,
		w.VecWeight, w.RRFK,
		w.BigmWeight, w.RRFK,
		w.SummaryVec, w.RRFK,
		w.EntityWeight, w.RRFK,
	)
}

// emptyEntityCTE is the entity lane when its RRF weight is zero: a
// trivially-empty CTE, so the FULL OUTER JOIN below stays syntactically valid
// and contributes nothing to the score.
const emptyEntityCTE = `entity AS (SELECT NULL::uuid AS id, NULL::bigint AS rank WHERE false)`

// buildEntityCTE renders the entity RRF lane (#139).
//
// It joins `documents` purely so the status/source filters can be applied
// INSIDE the lane. Applying them here rather than at the outer join matters:
// the CTE is capped by `LIMIT $3`, so filtering afterwards would let excluded
// documents consume candidate slots and silently shrink the entity lane's
// contribution.
//
// The filter fragments must be `d.`-qualified — they are the entityStatusFilter
// / entitySourceFilter / entityExcludeFilter variants built in hybridSearch,
// not the unqualified ones used by the fts/vec/bigm/summvec CTEs. They bind the
// same positional parameters.
//
// Split out of hybridSearch so the filters can be asserted without a database:
// the lane's previous unfiltered form let soft-deleted and excluded documents
// (including insights) into results whenever they were reachable by entity
// name alone, and nothing in the test suite could observe it.
func buildEntityCTE(entityFilterParam, statusFilter, sourceFilter, excludeFilter, occurredRangeFilter string) string {
	return fmt.Sprintf(`entity AS (
			SELECT de.document_id AS id,
			       row_number() OVER (ORDER BY COUNT(*) DESC, de.document_id ASC) AS rank
			FROM document_entities de
			JOIN entities e ON e.id = de.entity_id
			JOIN documents d ON d.id = de.document_id
			WHERE e.normalized_name LIKE '%%%%' || %s || '%%%%'
			%s
			%s
			%s
			%s
			GROUP BY de.document_id
			LIMIT $3
		)`, entityFilterParam, statusFilter, sourceFilter, excludeFilter, occurredRangeFilter)
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
	Total    int                            `json:"total"`
	BySource map[string]DocumentSourceStats `json:"by_source_type"`
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
	MostRecentCollectedAt *time.Time            `json:"most_recent_collected_at"`
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
			src                     string
			cnt                     int64
			meanLen, p50Len, p95Len float64
			maxLen                  int64
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

// listWithoutEntitiesQuery backs ListWithoutEntities.
//
// The source_type filter is load-bearing, not cosmetic: without it the
// EntityWorker picks up every model-derived insight document and spends an LLM
// call extracting entities from an inference. Those entity links then feed the
// entity RRF lane in hybridSearch, which is how an unlabelled inference earns
// its way back into retrieval — the exact loop the insight-exclusion guard in
// search.Service exists to close. Exclusion is by source_type rather than by
// metadata so it cannot be undone by an enrichment-status edit.
const listWithoutEntitiesQuery = `
		SELECT id, source_type, source_id, title, content, metadata, embedding,
		       status, deleted_at, occurred_at, collected_at, created_at, updated_at,
		       title_summary, bullet_summary, summary_embedding
		FROM documents
		WHERE status = 'active'
		  AND entities_processed_at IS NULL
		  AND source_type <> 'insight'
		ORDER BY collected_at ASC
		LIMIT $1`

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
//
// Insight documents are excluded too; see listWithoutEntitiesQuery.
func (s *DocumentStore) ListWithoutEntities(ctx context.Context, limit int) ([]*model.Document, error) {
	rows, err := s.pg.pool.Query(ctx, listWithoutEntitiesQuery, limit)
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

// ListPendingForExtraction returns up to limit active documents from the 4
// active sources (gmail, sms, call-log, call-transcript) whose occurred_at
// falls within the last 30 days AND whose relations_extracted_at is NULL
// AND whose extraction backoff (if any) has elapsed (spec §5.1, migration
// 023). The 30-day bound is evaluated live at every call — NOT a one-time
// snapshot — so newly collected documents (which always have recent
// occurred_at) are naturally picked up on later ticks without any extra
// state, satisfying spec §5.1's "이후에는 신규 유입 문서를 증분 처리한다".
//
// Deliberately keyed on relations_extracted_at, NOT entities_processed_at:
// that column belongs to the pre-existing, currently-active EntityWorker
// (ENTITY_EXTRACTION_ENABLED=true in production), which extracts a
// different artifact via a different LLM call. See migration 023's header
// comment for the full rationale.
func (s *DocumentStore) ListPendingForExtraction(ctx context.Context, limit int) ([]*model.Document, error) {
	const q = `
		SELECT id, source_type, source_id, title, content, metadata, embedding,
		       status, deleted_at, occurred_at, collected_at, created_at, updated_at,
		       title_summary, bullet_summary, summary_embedding
		FROM documents
		WHERE status = 'active'
		  AND relations_extracted_at IS NULL
		  AND source_type IN ('gmail', 'sms', 'call-log', 'call-transcript')
		  AND occurred_at >= now() - interval '30 days'
		  AND (metadata->>'extraction_next_retry_at' IS NULL
		       OR (metadata->>'extraction_next_retry_at')::timestamptz <= now())
		ORDER BY occurred_at ASC
		LIMIT $1`

	rows, err := s.pg.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending for extraction: %w", err)
	}
	defer rows.Close()

	return collectDocuments(rows)
}

// MarkRelationsExtracted sets relations_extracted_at to now() on successful
// extraction. After this call the document is never returned by
// ListPendingForExtraction again (migration 023's partial index scopes on
// this column being NULL).
func (s *DocumentStore) MarkRelationsExtracted(ctx context.Context, documentID uuid.UUID) error {
	const q = `UPDATE documents SET relations_extracted_at = now() WHERE id = $1 AND relations_extracted_at IS NULL`
	if _, err := s.pg.pool.Exec(ctx, q, documentID); err != nil {
		return fmt.Errorf("mark relations extracted %s: %w", documentID, err)
	}
	return nil
}

// MarkExtractionAttemptFailed records a non-terminal extraction failure
// (attempts 1-2 of a 3-attempt cap — mirrors NoteEnrichmentWorker's retry
// model, adopted deliberately over EntityWorker's unbounded-retry model;
// see plan Global Constraints). relations_extracted_at is left NULL so the
// document is retried once nextRetryAt elapses.
func (s *DocumentStore) MarkExtractionAttemptFailed(ctx context.Context, documentID uuid.UUID, attempts int, reason string, nextRetryAt time.Time) error {
	const q = `
		UPDATE documents
		SET metadata = metadata || jsonb_build_object(
		        'extraction_attempts', $2::int,
		        'extraction_last_error', $3::text,
		        'extraction_next_retry_at', $4::timestamptz
		    ),
		    updated_at = now()
		WHERE id = $1`
	if _, err := s.pg.pool.Exec(ctx, q, documentID, attempts, reason, nextRetryAt); err != nil {
		return fmt.Errorf("mark extraction attempt failed %s: %w", documentID, err)
	}
	return nil
}

// MarkExtractionTerminal records the 3rd (terminal) extraction failure:
// relations_extracted_at is set so ListPendingForExtraction stops returning
// the document (a permanently malformed LLM output must not retry forever
// — this is the exact failure mode this plan's JSON-decoder defense and
// retry cap both exist to bound). There is currently no manual-retry
// endpoint for extraction (unlike POST /api/v1/notes/{id}/retry-enrichment)
// — out of scope for Part A; add one in Part C if operational need arises.
func (s *DocumentStore) MarkExtractionTerminal(ctx context.Context, documentID uuid.UUID, reason string) error {
	const q = `
		UPDATE documents
		SET relations_extracted_at = now(),
		    metadata = metadata || jsonb_build_object('extraction_last_error', $2::text),
		    updated_at = now()
		WHERE id = $1`
	if _, err := s.pg.pool.Exec(ctx, q, documentID, reason); err != nil {
		return fmt.Errorf("mark extraction terminal %s: %w", documentID, err)
	}
	return nil
}

// ListPendingNotes returns up to limit active model.SourceNote documents
// whose Metadata.enrichment_status is "pending" and whose
// enrichment_next_retry_at backoff (if any) has elapsed. Used by
// NoteEnrichmentWorker (spec §6.3). insight documents are never returned —
// source_type is hard-scoped to 'note', enforcing spec §3.2 gate 1
// (insight-of-insight is structurally impossible via this query).
func (s *DocumentStore) ListPendingNotes(ctx context.Context, limit int) ([]*model.Document, error) {
	const q = `
		SELECT id, source_type, source_id, title, content, metadata, embedding,
		       status, deleted_at, occurred_at, collected_at, created_at, updated_at,
		       title_summary, bullet_summary, summary_embedding
		FROM documents
		WHERE source_type = 'note'
		  AND status = 'active'
		  AND metadata->>'enrichment_status' = 'pending'
		  AND (metadata->>'enrichment_next_retry_at' IS NULL
		       OR (metadata->>'enrichment_next_retry_at')::timestamptz <= now())
		ORDER BY collected_at ASC
		LIMIT $1`

	rows, err := s.pg.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending notes: %w", err)
	}
	defer rows.Close()

	return collectDocuments(rows)
}

// MarkNoteEnriched merges the Organized-layer fields (spec §3) into a note's
// Metadata and marks enrichment_status "done". title, when non-empty,
// overwrites the document's Title column — this is how POST /api/v1/notes'
// server-left-empty title (spec §6.1) gets filled in. Content is never
// touched by this method or any caller of it (spec §3.1 write-once
// guarantee) — there is no content parameter to accidentally pass one.
func (s *DocumentStore) MarkNoteEnriched(ctx context.Context, documentID uuid.UUID, title string, metadata map[string]any) error {
	meta, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("mark note enriched: marshal metadata: %w", err)
	}
	const q = `
		UPDATE documents
		SET title = CASE WHEN $2 <> '' THEN $2 ELSE title END,
		    metadata = metadata || $3::jsonb,
		    updated_at = now()
		WHERE id = $1`
	if _, err := s.pg.pool.Exec(ctx, q, documentID, title, meta); err != nil {
		return fmt.Errorf("mark note enriched %s: %w", documentID, err)
	}
	return nil
}

// MarkNoteEnrichmentAttemptFailed records a non-terminal enrichment failure
// (attempts 1-2 of the 3-attempt cap, spec §6.3): increments the attempt
// counter, records the failure reason, and schedules the next eligible
// retry time. enrichment_status is left "pending" so ListPendingNotes will
// pick the note back up once nextRetryAt elapses.
func (s *DocumentStore) MarkNoteEnrichmentAttemptFailed(ctx context.Context, documentID uuid.UUID, attempts int, reason string, nextRetryAt time.Time) error {
	const q = `
		UPDATE documents
		SET metadata = metadata || jsonb_build_object(
		        'enrichment_attempts', $2::int,
		        'enrichment_last_error', $3::text,
		        'enrichment_next_retry_at', $4::timestamptz
		    ),
		    updated_at = now()
		WHERE id = $1`
	if _, err := s.pg.pool.Exec(ctx, q, documentID, attempts, reason, nextRetryAt); err != nil {
		return fmt.Errorf("mark note enrichment attempt failed %s: %w", documentID, err)
	}
	return nil
}

// MarkNoteEnrichmentTerminal records the 3rd (terminal) enrichment failure
// (spec §6.3): sets enrichment_status "failed" so ListPendingNotes stops
// returning the note. Only POST /api/v1/notes/{id}/retry-enrichment
// (ResetNoteEnrichment) can revive it.
func (s *DocumentStore) MarkNoteEnrichmentTerminal(ctx context.Context, documentID uuid.UUID, reason string) error {
	const q = `
		UPDATE documents
		SET metadata = metadata || jsonb_build_object(
		        'enrichment_status', 'failed',
		        'enrichment_last_error', $2::text
		    ),
		    updated_at = now()
		WHERE id = $1`
	if _, err := s.pg.pool.Exec(ctx, q, documentID, reason); err != nil {
		return fmt.Errorf("mark note enrichment terminal %s: %w", documentID, err)
	}
	return nil
}

// ResetNoteEnrichment reverts a terminal "failed" note back to "pending"
// with a zeroed attempt counter, for manual retry via
// POST /api/v1/notes/{id}/retry-enrichment (spec §6.3, §9.2). Returns
// found=false (no error) when documentID does not exist, is not a note, or
// is not currently in the terminal "failed" state — the caller (Task 7
// retryEnrichmentHandler) maps found=false to 409 Conflict.
func (s *DocumentStore) ResetNoteEnrichment(ctx context.Context, documentID uuid.UUID) (bool, error) {
	const q = `
		UPDATE documents
		SET metadata = metadata || jsonb_build_object(
		        'enrichment_status', 'pending',
		        'enrichment_attempts', 0,
		        'enrichment_last_error', NULL,
		        'enrichment_next_retry_at', NULL
		    ),
		    updated_at = now()
		WHERE id = $1
		  AND source_type = 'note'
		  AND metadata->>'enrichment_status' = 'failed'
		RETURNING id`
	var returnedID uuid.UUID
	err := s.pg.pool.QueryRow(ctx, q, documentID).Scan(&returnedID)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("reset note enrichment %s: %w", documentID, err)
	}
	return true, nil
}

// SoftDeleteByID soft-deletes a single model.SourceNote document (spec §6.5
// delete path). Scoped to source_type='note' so this method cannot be used
// to soft-delete arbitrary documents of other source types. Returns nil
// without effect when the id does not exist or the document is not an
// active note — callers that need to distinguish "not found" from
// "not a note" should GetByID first (Task 7 deleteNoteHandler does this).
func (s *DocumentStore) SoftDeleteByID(ctx context.Context, id uuid.UUID) error {
	const q = `
		UPDATE documents
		SET status = 'deleted', deleted_at = now()
		WHERE id = $1 AND source_type = 'note' AND status = 'active'`
	if _, err := s.pg.pool.Exec(ctx, q, id); err != nil {
		return fmt.Errorf("soft delete note %s: %w", id, err)
	}
	return nil
}

// SoftDeleteInsightsByNoteID cascades a note's soft-delete to every
// model.SourceInsight document derived from it (spec §6.5 permanent
// policy): "근거가 사라진 추론은 반증 불가능하고 감사 불가능하다". Matches
// on the JSONB path Metadata.provenance.source_note_id, which is how
// insight documents record their origin note (Task 6 EnrichNote). Returns
// the number of insight documents soft-deleted.
func (s *DocumentStore) SoftDeleteInsightsByNoteID(ctx context.Context, noteID uuid.UUID) (int, error) {
	const q = `
		UPDATE documents
		SET status = 'deleted', deleted_at = now()
		WHERE source_type = 'insight'
		  AND status = 'active'
		  AND metadata->'provenance'->>'source_note_id' = $1
		RETURNING id`
	rows, err := s.pg.pool.Query(ctx, q, noteID.String())
	if err != nil {
		return 0, fmt.Errorf("soft delete insights for note %s: %w", noteID, err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("soft delete insights for note %s: iterate: %w", noteID, err)
	}
	return count, nil
}

// listUnsummarizedQuery backs ListUnsummarized.
//
// The source_type filter is load-bearing, not cosmetic: without it the
// SummarizerWorker picks up every model-derived insight document and pays for
// an LLM summary of an LLM inference — a document that is already a single
// sentence. That is unbudgeted spend per note, on output nobody reads.
const listUnsummarizedQuery = `
		SELECT id, source_type, source_id, title, content, metadata, embedding,
		       status, deleted_at, occurred_at, collected_at, created_at, updated_at,
		       title_summary, bullet_summary, summary_embedding
		FROM documents
		WHERE title_summary IS NULL
		  AND status = 'active'
		  AND source_type <> 'insight'
		ORDER BY collected_at ASC
		LIMIT $1`

// ListUnsummarized returns up to limit active documents whose title_summary
// column is NULL, ordered by collected_at ASC (oldest first) so backfill
// progresses forward in time.
//
// Soft-deleted documents are excluded; there is no value in summarizing them.
// Insight documents are excluded too; see listUnsummarizedQuery.
func (s *DocumentStore) ListUnsummarized(ctx context.Context, limit int) ([]*model.Document, error) {
	rows, err := s.pg.pool.Query(ctx, listUnsummarizedQuery, limit)
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
		doc       model.Document
		metaJSON  []byte
		vec       *pgvector.Vector
		titleSum  pgtype.Text
		bulletSum pgtype.Text
		summVec   *pgvector.Vector
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
