---
title: supabase-postgres-best-practices
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/supabase-postgres-best-practices/SKILL.md
related:
  - "[[db-supabase-expert]]"
  - "[[skills/postgres-best-practices]]"
  - "[[guides/supabase-postgres]]"
---

# supabase-postgres-best-practices

Supabase-specific PostgreSQL patterns: query performance, RLS security, connection management via PgBouncer, schema design, and Supabase-native features.

## Overview

`supabase-postgres-best-practices` extends [[skills/postgres-best-practices]] with Supabase-specific concerns. The critical categories (in priority order): query performance (indexes, EXPLAIN ANALYZE), connection management (Supabase uses PgBouncer transaction mode — avoid session-level commands), RLS security (enable on all tables, use `auth.uid()` predicates), and schema design (prefix tables by domain, use `uuid_generate_v4()` for PKs).

Source: adapted from `supabase/agent-skills`.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Consumed by**: [db-supabase-expert](../agents/db-supabase-expert.md)

## Priority-Ordered Rules

| Priority | Category |
|----------|---------|
| 1 (CRITICAL) | Query Performance — indexes, partial indexes, `EXISTS` over `IN` |
| 2 (CRITICAL) | Connection Management — PgBouncer transaction mode, no session-level SET |
| 3 (CRITICAL) | Security & RLS — enable on all tables, `auth.uid()` in policies |
| 4 (HIGH) | Schema Design — domain prefixes, uuid PKs, `TIMESTAMPTZ` |
| 5 (MEDIUM-HIGH) | Concurrency & Locking — `SELECT ... FOR UPDATE SKIP LOCKED` for queues |

## Supabase-Specific

`auth.uid()` and `auth.jwt()` for RLS policies. `auth.users` join patterns. Edge Functions for server-side logic. Realtime subscriptions via `supabase_realtime` schema.

## Relationships

- **Base**: [[skills/postgres-best-practices]] for general PostgreSQL patterns
- **Guide**: [[guides/supabase-postgres]] for extended Supabase reference

## Sources

- `.claude/skills/supabase-postgres-best-practices/SKILL.md`
