---
title: db-alembic-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/db-alembic-expert.md
related:
  - "[[db-postgres-expert]]"
  - "[[be-fastapi-expert]]"
  - "[[qa-engineer]]"
  - "[[skills/alembic-best-practices]]"
  - "[[skills/postgres-best-practices]]"
---

# db-alembic-expert

Alembic migration lifecycle specialist ensuring safe, zero-downtime SQLAlchemy database migrations with expand-contract patterns and CI integration.

## Overview

`db-alembic-expert` manages the complete Alembic migration lifecycle from autogenerate through review, safety validation, and CI integration. Its central design principle is *never auto-fix column renames* — Alembic cannot distinguish rename from drop+add, so the agent always flags this for explicit user confirmation.

The agent uses model escalation (sonnet → opus after 2 failures) for complex schema decisions and enforces strict safety rules: no embedded credentials, CONCURRENTLY for large indexes, named constraints.

## Key Details

- **Model**: sonnet (escalates to opus) | **Effort**: high | **Memory**: project
- **Domain**: backend | **Skills**: alembic-best-practices, postgres-best-practices
- **Escalation**: sonnet → opus (threshold: 2 failures)

### Key Capabilities

1. Post-autogenerate safety review (dangerous pattern detection)
2. Expand-Contract pattern design for zero-downtime migrations
3. async env.py configuration (asyncpg, multi-tenant)
4. pytest-alembic test setup
5. alembic-utils for PostgreSQL objects (views, functions, RLS)
6. CI integration with `alembic check` and Squawk linter

### Safety Rules (Non-Negotiable)

- Column renames require explicit user confirmation
- `pass` in `downgrade()` must be justified
- No credentials in alembic.ini / env.py
- Large table indexes require `CONCURRENTLY`

## Relationships

- **Database**: [[db-postgres-expert]] for PostgreSQL DDL nuances
- **Application**: [[be-fastapi-expert]] for async engine and lifespan integration
- **Testing**: [[qa-engineer]] for migration test strategy

## Sources

- `.claude/agents/db-alembic-expert.md`
