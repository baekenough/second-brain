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

// RunMigrations executes all *.sql files inside the given directory in
// lexicographic order. Files already applied are safe to re-run because each
// migration is idempotent (CREATE TABLE IF NOT EXISTS, etc.).
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
