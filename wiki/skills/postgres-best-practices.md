---
title: postgres-best-practices
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/postgres-best-practices/SKILL.md
related:
  - "[[db-postgres-expert]]"
  - "[[guides/postgres]]"
  - "[[db-supabase-expert]]"
  - "[[skills/supabase-postgres-best-practices]]"
---

# postgres-best-practices

PostgreSQL patterns for query optimization, indexing strategy, schema design, connection pooling, and high availability.

## Overview

`postgres-best-practices` covers the critical PostgreSQL decisions that affect production performance. The starting point for any query issue is `EXPLAIN ANALYZE` — understanding seq scans, nested loops, row estimate accuracy, and buffer hit ratios before adding indexes.

Index type selection is the most impactful decision: B-tree for general use, GIN for JSONB/arrays/full-text, GiST for geometry/ranges, BRIN for large sequential tables. Always use `CREATE INDEX CONCURRENTLY` in production to avoid table locks.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Consumed by**: [db-postgres-expert](../agents/db-postgres-expert.md)

## Critical Rules

- `EXPLAIN ANALYZE` before adding any index
- `CREATE INDEX CONCURRENTLY` in production (no table lock)
- Covering indexes (`INCLUDE`) to avoid heap fetches
- Partial indexes for frequently filtered subsets
- `VACUUM ANALYZE` regularly; autovacuum tuning for high-write tables
- Connection pooling: PgBouncer in transaction mode for high concurrency
- `max_connections` set conservatively (PgBouncer handles the actual connections)

## Schema Design

Prefer `BIGINT` PKs (serial deprecated in favor of `GENERATED ALWAYS AS IDENTITY`). Use `TIMESTAMPTZ` not `TIMESTAMP`. JSONB over JSON for queryable semi-structured data.

## Relationships

- **Agent**: [[db-postgres-expert]] applies these patterns
- **Supabase**: [[skills/supabase-postgres-best-practices]] for Supabase-specific patterns
- **Guide**: [[guides/postgres]] for extended reference

## Sources

- `.claude/skills/postgres-best-practices/SKILL.md`
