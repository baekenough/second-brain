package recordingbackfill

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// postgresUniqueViolation is the PostgreSQL SQLSTATE code for a unique
// constraint violation (23505). documents has a UNIQUE(source_type, source_id)
// constraint (see the ON CONFLICT clauses in internal/store/document.go), so
// renaming source_id into a value that already exists surfaces as this code.
const postgresUniqueViolation = "23505"

// PostgresSourceIDStore is the production SourceIDStore, backed directly by
// the shared pgx pool exposed via internal/store.Postgres.Pool(). Direct SQL
// is used here — rather than adding a new method to internal/store.DocumentStore
// — so that this one-time backfill tool does not require changes to files
// owned by other in-flight work.
type PostgresSourceIDStore struct {
	Pool *pgxpool.Pool
}

// NewPostgresSourceIDStore returns a PostgresSourceIDStore backed by pool.
func NewPostgresSourceIDStore(pool *pgxpool.Pool) *PostgresSourceIDStore {
	return &PostgresSourceIDStore{Pool: pool}
}

// RenameCallTranscriptSourceID renames the source_id of the call-transcript
// document currently stored under oldSourceID to newSourceID. Both active and
// soft-deleted rows are eligible (no status filter) — the SourceID identity
// must stay correct regardless of the document's current lifecycle state.
//
// Returns:
//   - RenameApplied  — exactly one row was found and renamed.
//   - RenameNoOp     — no row matched oldSourceID (already renamed by a prior
//     run, or never ingested). Not an error; the caller proceeds with the
//     filesystem mutation.
//   - RenameConflict — the rename would violate the UNIQUE(source_type,
//     source_id) constraint because a document already exists under
//     newSourceID. The caller must leave the file untouched.
func (s *PostgresSourceIDStore) RenameCallTranscriptSourceID(ctx context.Context, oldSourceID, newSourceID string) (RenameResult, error) {
	const q = `
		UPDATE documents
		SET source_id = $2, updated_at = now()
		WHERE source_type = 'call-transcript' AND source_id = $1`

	tag, err := s.Pool.Exec(ctx, q, oldSourceID, newSourceID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == postgresUniqueViolation {
			return RenameConflict, nil
		}
		// oldSourceID/newSourceID are logged/wrapped in their PII-safe form
		// (see redactSourceIDForLog): a call-transcript SourceID
		// ("transcript:"+relPath) derived from a pre-#164 filename still
		// carries the raw phone number, and this error may bubble up to
		// caller-level logging (see recordingbackfill.applyPlan).
		return RenameNoOp, fmt.Errorf("rename call-transcript source_id %q -> %q: %w",
			redactSourceIDForLog(oldSourceID), redactSourceIDForLog(newSourceID), err)
	}
	if tag.RowsAffected() == 0 {
		return RenameNoOp, nil
	}
	return RenameApplied, nil
}
