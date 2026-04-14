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
func (pg *Postgres) RunMigrations(ctx context.Context, migrationsDir string) error {
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
		if _, err := pg.pool.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("execute migration %q: %w", f, err)
		}
		slog.Info("migration applied", "file", filepath.Base(f))
	}
	return nil
}

// Pool returns the underlying pgx pool for use by store implementations.
func (pg *Postgres) Pool() *pgxpool.Pool { return pg.pool }

// Close releases all pool connections.
func (pg *Postgres) Close() { pg.pool.Close() }
