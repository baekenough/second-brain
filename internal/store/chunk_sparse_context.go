package store

import (
	"context"
	"fmt"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/jackc/pgx/v5"
)

// chunkSparseFingerprintSQL is the SQL expression that computes a document's
// fingerprint for chunk_sparse_context staleness checks (migration 040,
// #270 phase B). It is used verbatim in BOTH the backfill writer's SELECT
// below AND the fuse_ctx lane's WHERE clause
// (chunk_sparse_search.go) — neither side ever computes this value in Go, so
// the two can never disagree: both always ask PostgreSQL, on the CURRENT
// row, inside the query that reads it.
//
// Separator is the ASCII unit separator (0x1F), not a printable character
// like "|" — title/metadata text can legitimately contain "|", and a
// collision there would make two DIFFERENT documents fingerprint identically
// after an edit. 0x1F essentially never appears in collected text.
//
// metadata::text (not individual keys) is deliberate: ANY metadata change —
// not just the keys the header currently reads — invalidates the
// fingerprint. That is intentionally over-eager (a metadata edit that does
// not touch the header still forces a recompute), and safe: a document whose
// fingerprint does not match falls back to raw chunk-content matching
// (chunk_sparse_search.go) until the backfill worker recomputes it, so
// over-invalidation costs recall, never staleness.
const chunkSparseFingerprintSQL = `md5(d.source_type || E'\x1f' || coalesce(d.title, '') || E'\x1f' || coalesce(d.occurred_at::text, '') || E'\x1f' || coalesce(d.metadata::text, ''))`

// SparseContextCandidate is one chunk's raw materials for computing a
// chunk_sparse_context row, fetched by FetchSparseContextCandidates.
// Fingerprint comes from chunkSparseFingerprintSQL evaluated on the SAME
// row Doc was read from (same SELECT, same MVCC snapshot) — see that
// constant's doc comment for why this matters.
type SparseContextCandidate struct {
	ChunkID     int64
	Content     string
	Fingerprint string
	Doc         model.Document // SourceType, Title, OccurredAt, Metadata only
}

// FetchSparseContextCandidates returns up to batchSize chunks with
// c.id > afterChunkID, ordered by c.id, each joined to its parent document's
// header-relevant fields and current fingerprint. The caller
// (internal/sparsectx) builds sparse_text from these via
// internal/chunkctx.BuildSparseText and writes them back with
// UpsertSparseContextBatch — in a SEPARATE call, deliberately: building
// sparse_text needs no database access, and keeping it out of this method
// keeps store free of internal/chunkctx (search/store -> chunkctx is fine;
// this file just does not need to import it).
func (s *ChunkStore) FetchSparseContextCandidates(ctx context.Context, afterChunkID int64, batchSize int) ([]SparseContextCandidate, error) {
	if batchSize <= 0 {
		batchSize = 500
	}
	q := `
		SELECT c.id, c.content, ` + chunkSparseFingerprintSQL + `,
			d.source_type, d.title, d.occurred_at, d.metadata
		FROM chunks c
		JOIN documents d ON d.id = c.document_id
		WHERE c.id > $1
		ORDER BY c.id
		LIMIT $2`

	rows, err := s.pg.pool.Query(ctx, q, afterChunkID, batchSize)
	if err != nil {
		return nil, fmt.Errorf("fetch sparse context candidates: %w", err)
	}
	defer rows.Close()

	var out []SparseContextCandidate
	for rows.Next() {
		var c SparseContextCandidate
		var sourceType string
		var metaJSON []byte
		if err := rows.Scan(&c.ChunkID, &c.Content, &c.Fingerprint,
			&sourceType, &c.Doc.Title, &c.Doc.OccurredAt, &metaJSON); err != nil {
			return nil, fmt.Errorf("scan sparse context candidate: %w", err)
		}
		c.Doc.SourceType = model.SourceType(sourceType)
		c.Doc.Metadata = decodeChunkDocumentMetadata(metaJSON)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sparse context candidates: %w", err)
	}
	return out, nil
}

// SparseContextRow is one computed sparse_text ready for
// UpsertSparseContextBatch.
type SparseContextRow struct {
	ChunkID     int64
	Fingerprint string
	SparseText  string
}

// UpsertSparseContextBatch writes rows for context_version in ONE
// transaction and advances the batch's checkpoint (chunk_sparse_backfill_state)
// in the SAME transaction — a process killed mid-batch never leaves the
// checkpoint ahead of what was actually committed, so resuming from the
// checkpoint never skips a chunk.
//
// The upsert is idempotent by design: ON CONFLICT only writes when the
// fingerprint or sparse_text actually differs from what is already stored,
// so re-running a batch whose documents have not changed reports 0 rows
// written (see the WHERE clause below) even though the checkpoint UPDATE
// still runs.
//
// checkpoint is the last (highest) chunk_id in rows — the caller
// (internal/sparsectx) is responsible for passing candidates in ascending
// c.id order (FetchSparseContextCandidates already returns them that way).
func (s *ChunkStore) UpsertSparseContextBatch(ctx context.Context, version string, rows []SparseContextRow, checkpoint int64) (written int64, err error) {
	tx, err := s.pg.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("sparse context batch begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, r := range rows {
		tag, err := tx.Exec(ctx, `
			INSERT INTO chunk_sparse_context (chunk_id, context_version, fingerprint, sparse_text)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (chunk_id, context_version) DO UPDATE SET
				fingerprint = EXCLUDED.fingerprint,
				sparse_text = EXCLUDED.sparse_text,
				created_at  = now()
			WHERE chunk_sparse_context.fingerprint IS DISTINCT FROM EXCLUDED.fingerprint
			   OR chunk_sparse_context.sparse_text IS DISTINCT FROM EXCLUDED.sparse_text`,
			r.ChunkID, version, r.Fingerprint, r.SparseText,
		)
		if err != nil {
			return 0, fmt.Errorf("upsert chunk_sparse_context chunk_id=%d: %w", r.ChunkID, err)
		}
		written += tag.RowsAffected()
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO chunk_sparse_backfill_state (context_version, last_chunk_id, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (context_version) DO UPDATE SET
			last_chunk_id = GREATEST(chunk_sparse_backfill_state.last_chunk_id, EXCLUDED.last_chunk_id),
			updated_at    = now()`,
		version, checkpoint,
	); err != nil {
		return 0, fmt.Errorf("advance sparse context checkpoint: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("sparse context batch commit: %w", err)
	}
	return written, nil
}

// SparseBackfillCheckpoint returns the last chunk_id processed for version,
// or 0 if no pass has ever run (or completed a batch) for it.
func (s *ChunkStore) SparseBackfillCheckpoint(ctx context.Context, version string) (int64, error) {
	var last int64
	err := s.pg.pool.QueryRow(ctx,
		`SELECT last_chunk_id FROM chunk_sparse_backfill_state WHERE context_version = $1`,
		version,
	).Scan(&last)
	if err != nil {
		if err == pgx.ErrNoRows {
			return 0, nil
		}
		return 0, fmt.Errorf("read sparse context checkpoint: %w", err)
	}
	return last, nil
}

// ResetSparseBackfillCheckpoint restarts version's checkpoint from 0 —
// backing cmd/sparsectx's --sweep flag, which recomputes every chunk so
// fingerprints that went stale after the LAST pass finished are refreshed.
func (s *ChunkStore) ResetSparseBackfillCheckpoint(ctx context.Context, version string) error {
	_, err := s.pg.pool.Exec(ctx, `
		INSERT INTO chunk_sparse_backfill_state (context_version, last_chunk_id, pass_started_at, updated_at)
		VALUES ($1, 0, now(), now())
		ON CONFLICT (context_version) DO UPDATE SET
			last_chunk_id   = 0,
			pass_started_at = now(),
			updated_at      = now()`,
		version,
	)
	if err != nil {
		return fmt.Errorf("reset sparse context checkpoint: %w", err)
	}
	return nil
}
