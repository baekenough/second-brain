package store

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	pgxstd "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvecpgx "github.com/pgvector/pgvector-go/pgx"
)

// Postgres wraps a pgx connection pool with store operations.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres opens a pgx pool, registers pgvector types, and enables the
// pgvector extension. The caller must call Close when done.
func NewPostgres(ctx context.Context, databaseURL string) (*Postgres, error) {
	// Create a temporary single connection to install the pgvector extension
	// before the pool is created. AfterConnect calls pgvecpgx.RegisterTypes,
	// which requires the extension to already exist. If we relied on the pool's
	// first connection (triggered by Ping) to create the extension, it would be
	// a chicken-and-egg problem: AfterConnect fires before Exec can run.
	tmpConn, err := pgxstd.Connect(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open temporary connection: %w", err)
	}
	if _, err := tmpConn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		_ = tmpConn.Close(ctx)
		return nil, fmt.Errorf("create vector extension: %w", err)
	}
	if err := tmpConn.Close(ctx); err != nil {
		return nil, fmt.Errorf("close temporary connection: %w", err)
	}

	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}

	// Register pgvector types for every new connection.
	// The extension is guaranteed to exist at this point.
	cfg.AfterConnect = func(ctx context.Context, conn *pgxstd.Conn) error {
		return pgvecpgx.RegisterTypes(ctx, conn)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	slog.Info("database connected")
	return &Postgres{pool: pool}, nil
}

// migrationAdvisoryLockKey is a stable 64-bit integer used as the PostgreSQL
// advisory lock key that serialises concurrent RunMigrations calls.  The value
// is arbitrary but must be unique within the application and constant across
// releases so that all instances agree on the same lock.
//
// The mnemonic: 0x5B_4149_4E is "BRAIN" in ASCII (0x42=B, 0x52=R, 0x41=A, 0x49=I, 0x4E=N).
// Chosen to be memorable and unlikely to collide with any library-owned advisory lock.
const migrationAdvisoryLockKey = int64(0x5B4149494E000001)

// RunMigrations executes all *.sql files inside the given directory in
// lexicographic order. Files already applied are safe to re-run because each
// migration is idempotent (CREATE TABLE IF NOT EXISTS, etc.).
//
// Concurrent-startup serialisation: before executing any migration file,
// RunMigrations acquires a PostgreSQL session-level advisory lock
// (pg_advisory_lock) on migrationAdvisoryLockKey. When multiple service
// instances (server, collector, eval-runner) start simultaneously each one
// blocks on pg_advisory_lock until the holder finishes and releases via
// pg_advisory_unlock. This prevents the 40P01 deadlock that previously
// occurred when concurrent instances raced on DDL statements in the same
// migration file.
//
// embeddingDim is the configured vector dimension (from EMBEDDING_DIM env var,
// default 1536). When a migration file is named "011_configurable_embedding_dim.sql",
// RunMigrations acquires a dedicated connection, opens a transaction, and runs
//
//	SET LOCAL app.embedding_dim = <embeddingDim>
//
// immediately before executing that file's SQL — all on the same connection and
// within the same transaction. SET LOCAL ensures the GUC is visible only to that
// transaction, which is exactly what migration 011 reads via current_setting().
// Any other migration file is executed via the pool as before.
func (pg *Postgres) RunMigrations(ctx context.Context, migrationsDir string, embeddingDim int) error {
	// Acquire a dedicated connection for the advisory lock.  The lock is
	// session-level (not transaction-level) so it survives individual migration
	// statements and is released when the connection is returned to the pool or
	// explicitly unlocked below.
	lockConn, err := pg.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire lock connection: %w", err)
	}
	defer lockConn.Release()

	slog.Info("migration: acquiring advisory lock", "key", migrationAdvisoryLockKey)
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationAdvisoryLockKey); err != nil {
		return fmt.Errorf("acquire migration advisory lock: %w", err)
	}
	slog.Info("migration: advisory lock acquired")

	// Always release the advisory lock, even on error.  pg_advisory_unlock on
	// a connection that is about to be returned to the pool (or closed) would
	// release automatically, but explicit unlock is cleaner and more readable.
	//
	// context.Background() is used intentionally: the caller's ctx may be
	// cancelled (e.g. SIGTERM) by the time the defer fires. Using a cancelled
	// context would cause pg_advisory_unlock to fail, leaking the session-level
	// lock until the connection is closed by the pool. Background context ensures
	// the unlock always succeeds even when the parent context is done.
	defer func() {
		unlockCtx := context.Background()
		if _, unlockErr := lockConn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", migrationAdvisoryLockKey); unlockErr != nil {
			slog.Warn("migration: failed to release advisory lock", "error", unlockErr)
		} else {
			slog.Info("migration: advisory lock released")
		}
	}()

	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return fmt.Errorf("read migrations dir %q: %w", migrationsDir, err)
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, filepath.Join(migrationsDir, e.Name()))
		}
	}
	sort.Strings(files)

	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read migration %q: %w", f, err)
		}

		base := filepath.Base(f)
		if needsEmbeddingDimGUC(base) && embeddingDim > 0 {
			// Acquire a single connection so that SET LOCAL and the migration SQL
			// execute on the same underlying PostgreSQL backend process.
			if err := pg.runMigrationWithEmbeddingDim(ctx, string(sql), embeddingDim); err != nil {
				return fmt.Errorf("execute migration %q: %w", f, err)
			}
		} else {
			if _, err := pg.pool.Exec(ctx, string(sql)); err != nil {
				return fmt.Errorf("execute migration %q: %w", f, err)
			}
		}
		slog.Info("migration applied", "file", base)
	}
	return nil
}

// needsEmbeddingDimGUC reports whether a migration file requires the
// app.embedding_dim GUC to be set before execution. Currently this covers
// migration 011 (documents.embedding reshape) and migration 015
// (chunks.embedding creation/reshape).
func needsEmbeddingDimGUC(base string) bool {
	return base == "011_configurable_embedding_dim.sql" ||
		base == "015_chunk_embeddings.sql"
}

// runMigrationWithEmbeddingDim runs a GUC-dependent migration on a dedicated
// connection inside a transaction. SET LOCAL scopes app.embedding_dim to the
// transaction, which guarantees that current_setting('app.embedding_dim')
// inside the DO block reads the configured value — not NULL — regardless of
// pool connection reuse.
//
// This replaces the narrower runMigration011 and is used for any migration
// that reads app.embedding_dim (currently 011 and 015).
func (pg *Postgres) runMigrationWithEmbeddingDim(ctx context.Context, sql string, embeddingDim int) error {
	conn, err := pg.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// SET LOCAL is transaction-scoped: the GUC reverts automatically on
	// commit/rollback, which is safe for a session-local parameter.
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL app.embedding_dim = %d", embeddingDim)); err != nil {
		return fmt.Errorf("set app.embedding_dim: %w", err)
	}

	if _, err := tx.Exec(ctx, sql); err != nil {
		return fmt.Errorf("execute sql: %w", err)
	}

	return tx.Commit(ctx)
}

// Pool returns the underlying pgx pool for use by store implementations.
func (pg *Postgres) Pool() *pgxpool.Pool { return pg.pool }

// Close releases all pool connections.
func (pg *Postgres) Close() { pg.pool.Close() }
