---
title: "Guide: Supabase PostgreSQL"
type: guide
updated: 2026-04-12
sources:
  - guides/supabase-postgres/index.yaml
related:
  - "[[db-supabase-expert]]"
  - "[[db-postgres-expert]]"
  - "[[fe-vercel-agent]]"
  - "[[skills/supabase-postgres-best-practices]]"
---

# Guide: Supabase PostgreSQL

Reference documentation for Supabase — RLS policies, schema design, connection pooling, and platform-specific PostgreSQL patterns.

## Overview

The Supabase PostgreSQL guide provides reference documentation for `db-supabase-expert` and the `supabase-postgres-best-practices` skill. It covers Supabase's platform-specific features layered on top of PostgreSQL: Row-Level Security, Supabase CLI migrations, and PgBouncer configuration.

## Key Topics

- **Row-Level Security (RLS)**: Policy creation for multi-tenant apps, `auth.uid()` integration, performance considerations
- **Schema Design**: Supabase conventions, public schema patterns, `auth` and `storage` schemas
- **Migrations**: Supabase CLI migration workflow, `supabase db push`, local development
- **Connection Pooling**: PgBouncer configuration via Supabase dashboard, connection limits
- **Realtime**: Postgres changes subscription, channel management
- **Storage**: File upload patterns, bucket policies, CDN integration
- **Monitoring**: `pg_stat_statements`, lock monitoring, slow query analysis via Supabase dashboard

## Relationships

- **Agent**: [[db-supabase-expert]] primary consumer
- **Pure PostgreSQL**: [[db-postgres-expert]] for non-Supabase PostgreSQL work
- **Frontend**: [[fe-vercel-agent]] for Supabase client-side JavaScript integration
- **Skill**: [[skills/supabase-postgres-best-practices]] implements patterns

## Sources

- `guides/supabase-postgres/index.yaml`
