---
title: "Guide: PostgreSQL"
type: guide
updated: 2026-04-12
sources:
  - guides/postgres/README.md
related:
  - "[[db-postgres-expert]]"
  - "[[db-supabase-expert]]"
  - "[[db-alembic-expert]]"
  - "[[skills/postgres-best-practices]]"
---

# Guide: PostgreSQL

Reference documentation for PostgreSQL — indexing strategies, partitioning, replication, query optimization, and extensions.

## Overview

The PostgreSQL guide provides reference documentation for `db-postgres-expert` and the `postgres-best-practices` skill. It covers production PostgreSQL operations from index design through partition management, replication setup, and advanced extension usage.

## Key Topics

- **Indexing**: B-tree, GIN (full-text, JSONB), GiST (geometric, range), BRIN (sequential data), partial and covering indexes
- **Partitioning**: Range, list, hash partitioning — declarative syntax, partition pruning, attach/detach
- **Replication**: Streaming replication, logical replication (pglogical), Patroni HA
- **Query Optimization**: EXPLAIN ANALYZE output reading, pg_stat_statements, index hints, query planner stats
- **PG-Specific SQL**: CTEs, window functions, LATERAL joins, JSONB operators, UPSERT, arrays
- **Vacuum**: Autovacuum tuning, bloat management, `VACUUM ANALYZE` patterns
- **Extensions**: pg_trgm, PostGIS, pgvector, pg_cron, TimescaleDB configuration

## Relationships

- **Agent**: [[db-postgres-expert]] primary consumer
- **Migrations**: [[db-alembic-expert]] for SQLAlchemy migration patterns
- **Supabase**: [[db-supabase-expert]] for Supabase-hosted PostgreSQL
- **Skill**: [[skills/postgres-best-practices]] implements patterns

## Sources

- `guides/postgres/README.md`
