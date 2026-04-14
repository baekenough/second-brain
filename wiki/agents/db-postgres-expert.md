---
title: db-postgres-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/db-postgres-expert.md
related:
  - "[[db-supabase-expert]]"
  - "[[db-alembic-expert]]"
  - "[[db-redis-expert]]"
  - "[[skills/postgres-best-practices]]"
  - "[[be-go-backend-expert]]"
---

# db-postgres-expert

Expert PostgreSQL DBA for pure PostgreSQL environments — indexing, partitioning, replication, query tuning, and extension management without Supabase dependency.

## Overview

`db-postgres-expert` is the pure-PostgreSQL specialist, covering the full DBA domain: index strategy selection, table partitioning schemes, streaming/logical replication, vacuum management, and query plan analysis with EXPLAIN ANALYZE. It diverges from [[db-supabase-expert]] by focusing on PostgreSQL-specific internals rather than Supabase's hosted platform.

The agent's user-scoped memory persists cross-project PostgreSQL patterns that apply broadly.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: user (cross-project persistence)
- **Domain**: backend | **Skill**: postgres-best-practices | **Guide**: `guides/postgres/`

### Capability Areas

- **Indexing**: B-tree, GIN, GiST, BRIN, partial indexes, covering indexes
- **Partitioning**: range, list, hash, declarative partitioning
- **Replication**: streaming, logical replication, HA setups
- **Query Tuning**: EXPLAIN ANALYZE, pg_stat_statements, lock contention
- **PG SQL**: CTEs, window functions, LATERAL joins, JSONB, UPSERT, arrays
- **Extensions**: pg_trgm, PostGIS, pgvector, pg_cron, TimescaleDB
- **Vacuum**: autovacuum tuning, bloat management

## Relationships

- **Supabase layer**: [[db-supabase-expert]] for hosted Supabase projects
- **Migrations**: [[db-alembic-expert]] for SQLAlchemy-managed migrations
- **Caching layer**: [[db-redis-expert]] for complementary caching strategies
- **Skill**: [postgres-best-practices](../skills/postgres-best-practices.md)

## Sources

- `.claude/agents/db-postgres-expert.md`
