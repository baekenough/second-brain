package store

import (
	"context"
	"fmt"

	"github.com/baekenough/second-brain/internal/model"
)

// SearchSparseContextFiltered is the fuse_ctx lane's SQL
// (#270 phase B, SEARCH_CHUNK_SPARSE=fuse_ctx). It reads chunk_sparse_context
// (migration 040) for a specific context_version, and — for chunks that have
// no FRESH row there — falls back to raw chunk content, the exact same
// predicate SearchFTSFiltered above uses. "Fresh" means
// chunk_sparse_context.fingerprint still equals the document's CURRENT
// fingerprint (chunkSparseFingerprintSQL, evaluated fresh on d in this
// query) — a title rename, a metadata merge, or a PII-redaction rewrite
// invalidates the fingerprint immediately, so a stale (pre-edit) header can
// never be the thing that matches a query; the chunk just falls back to its
// own raw content until the backfill worker recomputes it.
//
// It ALWAYS returns c.content (never sparse_text) — sparse_text exists only
// to be matched against, exactly like SearchFTSFiltered's use of
// content_tsv/content.
//
// filters applies the SAME document eligibility (source/window/retention)
// chunkEligibilitySQL already defines for every other chunk lane, applied
// identically inside BOTH branches below (both alias documents as `d`).
//
// LIMIT is applied INSIDE each CTE (#270 deep-verify MEDIUM), not once on
// the UNION ALL of both — fresh.rank (over sparse_tsv, header+body) and
// raw.rank (over content_tsv, body only) come from two different tsvector
// populations and are not calibrated against each other; a raw-content
// lane with many high-frequency matches can outnumber and outrank every
// fresh match, and a single post-union LIMIT would then drop fresh rows
// entirely — even though a fresh match (a name findable only via the
// header) is exactly the result this lane exists to surface. Limiting each
// branch independently first guarantees up to `limit` of EACH kind survive
// into the combined result; the caller (fuseChunkSparseCtx,
// internal/search/chunk_sparse_lane.go) already over-fetches (limit*3) and
// does its own per-document aggregation/truncation in Go, so returning up
// to 2*limit rows here (rather than a single hard cap) is intentional, not
// a leak.
func (s *ChunkStore) SearchSparseContextFiltered(ctx context.Context, filter model.SearchQuery, limit int, version string) ([]ChunkSearchResult, error) {
	query := filter.Query
	if limit <= 0 {
		limit = 20
	}
	if version == "" {
		return nil, fmt.Errorf("chunk sparse context search: empty context_version")
	}

	args, filters := chunkEligibilitySQL([]interface{}{query, limit, version}, filter)
	q := `
		WITH fresh AS (
			SELECT
				c.id, c.document_id, c.chunk_index, c.content, c.byte_size, c.created_at,
				GREATEST(
					ts_rank(sc.sparse_tsv, plainto_tsquery('simple', $1)),
					CASE WHEN sc.sparse_text LIKE '%%' || $1 || '%%' THEN 0.01 ELSE 0 END
				) AS rank,
				d.title          AS document_title,
				d.source_type    AS document_source,
				d.status         AS document_status,
				d.occurred_at    AS document_occurred_at,
				d.collected_at   AS document_collected_at,
				d.metadata       AS document_metadata
			FROM chunk_sparse_context sc
			JOIN chunks c ON c.id = sc.chunk_id
			JOIN documents d ON d.id = c.document_id
			WHERE sc.context_version = $3
			  AND sc.fingerprint = ` + chunkSparseFingerprintSQL + `
			  AND (sc.sparse_tsv @@ plainto_tsquery('simple', $1) OR sc.sparse_text LIKE '%%' || $1 || '%%')
			  AND d.status = 'active' ` + filters + `
			ORDER BY rank DESC
			LIMIT $2
		),
		raw AS (
			SELECT
				c.id, c.document_id, c.chunk_index, c.content, c.byte_size, c.created_at,
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
			WHERE NOT EXISTS (
				SELECT 1 FROM chunk_sparse_context sc2
				WHERE sc2.chunk_id = c.id
				  AND sc2.context_version = $3
				  AND sc2.fingerprint = ` + chunkSparseFingerprintSQL + `
			)
			AND (c.content_tsv @@ plainto_tsquery('simple', $1) OR c.content LIKE '%%' || $1 || '%%')
			AND d.status = 'active' ` + filters + `
			ORDER BY rank DESC
			LIMIT $2
		)
		SELECT * FROM fresh
		UNION ALL
		SELECT * FROM raw
		ORDER BY rank DESC`

	rows, err := s.pg.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("chunk sparse context search: %w", err)
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
			return nil, fmt.Errorf("chunk sparse context search scan: %w", err)
		}
		r.DocumentMetadata = decodeChunkDocumentMetadata(metaJSON)
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chunk sparse context search iter: %w", err)
	}
	return results, nil
}
