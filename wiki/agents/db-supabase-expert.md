---
title: db-supabase-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/db-supabase-expert.md
related:
  - "[[db-postgres-expert]]"
  - "[[db-alembic-expert]]"
  - "[[fe-vercel-agent]]"
  - "[[skills/supabase-postgres-best-practices]]"
---

# db-supabase-expert

Supabase and PostgreSQL expert for schema design, RLS policies, connection pooling, and performance monitoring on Supabase-hosted databases.

## Overview

`db-supabase-expert` handles the intersection of Supabase's platform features and PostgreSQL internals. Where [[db-postgres-expert]] focuses on pure PostgreSQL DBA work, this agent understands Supabase-specific layers: Row-Level Security (RLS) for multi-tenant apps, PgBouncer connection pooling config, and Supabase migration tooling.

RLS policy design is a core specialty — getting the predicate expressions right to enforce tenant isolation without performance regression.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: user
- **Domain**: backend | **Skill**: supabase-postgres-best-practices | **Guide**: `guides/supabase-postgres/`

### Capabilities

- Schema design with proper normalization and indexing
- Row-Level Security (RLS) policies for multi-tenant applications
- EXPLAIN-based query optimization
- PgBouncer connection pooling and scaling configuration
- Migration strategies (Supabase CLI migrations)
- Monitoring: pg_stat_statements, lock contention, slow query analysis

## Relationships

- **Pure PostgreSQL**: [[db-postgres-expert]] for non-Supabase PostgreSQL work
- **Migrations**: [[db-alembic-expert]] for SQLAlchemy-based migration management
- **Frontend integration**: [[fe-vercel-agent]] for Supabase client-side integration

## Sources

- `.claude/agents/db-supabase-expert.md`
