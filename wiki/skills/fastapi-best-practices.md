---
title: fastapi-best-practices
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/fastapi-best-practices/SKILL.md
related:
  - "[[be-fastapi-expert]]"
  - "[[lang-python-expert]]"
  - "[[guides/fastapi]]"
  - "[[skills/python-best-practices]]"
  - "[[db-postgres-expert]]"
---

# fastapi-best-practices

FastAPI patterns for domain-based project structure, async routing, Pydantic validation, and dependency injection.

## Overview

`fastapi-best-practices` enforces domain-based project organization (`src/{module}/router.py`, `schemas.py`, `service.py`) and async-correct patterns. The central rule: use `async def` only for I/O-bound operations; regular `def` for sync I/O (FastAPI offloads these to a threadpool automatically). CPU-intensive work goes to separate processes (Celery), never to async handlers.

Dependencies (`Depends()`) are the primary mechanism for validation, authentication, and request-scoped caching — chained dependencies replace middleware for most cross-cutting concerns.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Consumed by**: [be-fastapi-expert](../agents/be-fastapi-expert.md)

## Core Rules

- **Never**: `time.sleep()` or blocking I/O in async functions
- **Always**: response models on every endpoint (`response_model=...`)
- **Always**: domain-specific exceptions + global exception handlers
- **Always**: async database drivers (asyncpg, SQLAlchemy async)
- **Settings**: split per module (`auth/config.py`, `database/config.py`), not a single global config
- **Testing**: `AsyncClient` from day one (`httpx.AsyncClient`)

## Async Patterns

| Operation | Handler |
|-----------|---------|
| Database/HTTP calls | `async def` + `await` |
| File/sync I/O | `def` (threadpool) |
| CPU-intensive | Separate process (Celery) |

## Relationships

- **Agent**: [[be-fastapi-expert]] applies these patterns
- **Database**: Alembic for migrations (SQL-first design)
- **Guide**: [[guides/fastapi]] for extended reference

## Sources

- `.claude/skills/fastapi-best-practices/SKILL.md`
