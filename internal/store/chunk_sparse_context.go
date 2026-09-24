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

// FetchSparseContextCandidates returns up to batchSize chunks that still
// need a chunk_sparse_context row for version, ordered by c.id, each joined
// to its parent document's header-relevant fields and current fingerprint.
// The caller (internal/sparsectx) builds sparse_text from these via
// internal/chunkctx.BuildSparseText and writes them back with
// UpsertSparseContextBatch — in a SEPARATE call, deliberately: building
// sparse_text needs no database access, and keeping it out of this method
// keeps store free of internal/chunkctx (search/store -> chunkctx is fine;
// this file just does not need to import it).
//
// "Still needs a row" is an anti-join against chunk_sparse_context, NOT a
// c.id > checkpoint filter (#270 deep-verify HIGH): a chunk whose INSERT
// commits with a LOWER id than one already scanned by an earlier batch — a
// real race, since chunks.id (BIGSERIAL) is assigned in statement-call
// order, not commit order, so two concurrent inserters can commit
// out-of-order — must still be found by the NEXT call. A checkpoint
// (`WHERE c.id > $checkpoint`) can permanently skip that chunk once the
// checkpoint has advanced past its id; the anti-join cannot, because it asks
// "does chunk_sparse_context already have what I need for THIS chunk", never
// "have I already looked past this id".
//
//   - sweep=false (a plain pass): a chunk qualifies if it has NO row at all
//     for version — the common case, picking up newly chunked documents.
//     A chunk whose row exists but has gone stale (title/metadata edit)
//     is NOT re-queued here; SearchSparseContextFiltered already falls back
//     to raw content matching for it in the meantime (chunk_sparse_search.go).
//   - sweep=true: a chunk also qualifies if its row's fingerprint no longer
//     matches the document's CURRENT fingerprint (chunkSparseFingerprintSQL,
//     evaluated fresh here) — i.e. it exists but is stale. This is how
//     --sweep "catches up" documents edited since the last full pass,
//     without needing to re-verify every already-fresh row on every call.
//
// This makes the method naturally idempotent and safe to call repeatedly
// with no coordination: rows already written (and still fresh, for the
// sweep=false case) simply stop matching the anti-join and are never
// returned again — resumability falls out of that property for free, with
// no checkpoint read required before the first call of a pass.
//
// afterChunkID is an OPTIONAL pagination hint (`AND c.id > afterChunkID`),
// NOT a correctness mechanism — pass 0 to consider every chunk. It exists
// for exactly one caller: internal/sparsectx.Run's --dry-run path, which
// never writes anything, so nothing else can advance the anti-join between
// its batches; Run tracks a local, in-memory cursor there purely to page
// through a read-only preview. The real (writing) path in Run always passes
// 0 — using a non-zero afterChunkID there would reintroduce exactly the
// skip bug the anti-join fixes (#270 deep-verify HIGH): a chunk whose commit
// lands, out of id order, behind a cursor a PRIOR real batch already
// advanced past would never be looked at again. A dry-run "skipping" such a
// chunk in its preview has no such consequence — it writes nothing, so the
// very next (real or dry-run) invocation recomputes candidates from
// scratch via the anti-join with a fresh cursor of its own.
func (s *ChunkStore) FetchSparseContextCandidates(ctx context.Context, version string, sweep bool, afterChunkID int64, batchSize int) ([]SparseContextCandidate, error) {
	if batchSize <= 0 {
		batchSize = 500
	}
	staleCheck := ""
	if sweep {
		// Sweep also excludes a row whose fingerprint is CURRENTLY fresh —
		// so only fresh rows count as "already has what it needs"; a stale
		// row (present but outdated) still qualifies as a candidate.
		staleCheck = ` AND sc.fingerprint = ` + chunkSparseFingerprintSQL
	}
	q := `
		SELECT c.id, c.content, ` + chunkSparseFingerprintSQL + `,
			d.source_type, d.title, d.occurred_at, d.metadata
		FROM chunks c
		JOIN documents d ON d.id = c.document_id
		WHERE c.id > $1
		  AND NOT EXISTS (
			SELECT 1 FROM chunk_sparse_context sc
			WHERE sc.chunk_id = c.id AND sc.context_version = $2` + staleCheck + `
		)
		ORDER BY c.id
		LIMIT $3`

	rows, err := s.pg.pool.Query(ctx, q, afterChunkID, version, batchSize)
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
// transaction and records the batch's highest chunk_id in
// chunk_sparse_backfill_state in the SAME transaction, purely as an
// observability/progress marker for operators (e.g. "how far has any pass
// ever written up to") — correctness no longer depends on this value.
// FetchSparseContextCandidates' anti-join (chunk_sparse_context.go) decides
// what still needs work from the data itself, not from this checkpoint, so a
// process killed mid-batch (leaving the checkpoint behind, ahead of, or
// exactly at what committed) can never cause the NEXT call to skip a chunk.
//
// The upsert is idempotent by design: ON CONFLICT only writes when the
// fingerprint or sparse_text actually differs from what is already stored,
// so re-running a batch whose documents have not changed reports 0 rows
// written (see the WHERE clause below) even though the checkpoint UPDATE
// still runs.
//
// checkpoint is the highest chunk_id in rows — the caller (internal/sparsectx)
// is responsible for passing candidates in ascending c.id order
// (FetchSparseContextCandidates already returns them that way).
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
// or 0 if no pass has ever run (or completed a batch) for it. Informational
// only (see UpsertSparseContextBatch's doc comment) — internal/sparsectx.Run
// no longer reads this before deciding what to fetch next;
// FetchSparseContextCandidates' anti-join answers that from the data
// directly. Exposed for operators who want to check backfill progress.
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

// ResetSparseBackfillCheckpoint restarts version's checkpoint marker at 0.
// --sweep (cmd/sparsectx) no longer needs this to force a full re-scan —
// FetchSparseContextCandidates(ctx, version, sweep=true, ...) already
// re-examines stale rows regardless of this value — kept only so the
// informational checkpoint does not read as "already past everything" after
// an operator explicitly asks for a sweep.
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
