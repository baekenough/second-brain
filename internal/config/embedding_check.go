// Package config — embedding_check.go
//
// The embedding dimension is wired into migration 011 via store.RunMigrations:
//
//	pg.RunMigrations(ctx, migrationsDir, cfg.EmbeddingDim)
//
// RunMigrations detects "011_configurable_embedding_dim.sql" by filename and
// executes it inside a transaction on a dedicated connection with
//
//	SET LOCAL app.embedding_dim = <cfg.EmbeddingDim>
//
// immediately before the DO block.  SET LOCAL is transaction-scoped, so the
// GUC is guaranteed to be visible to current_setting('app.embedding_dim')
// within that same transaction regardless of pool connection reuse.
//
// This file is intentionally kept as documentation.  No code is required here
// because the wiring lives in store.(*Postgres).RunMigrations.
package config
