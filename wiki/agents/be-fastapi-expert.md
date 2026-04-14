---
title: be-fastapi-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/be-fastapi-expert.md
related:
  - "[[lang-python-expert]]"
  - "[[db-alembic-expert]]"
  - "[[db-postgres-expert]]"
  - "[[be-django-expert]]"
  - "[[skills/fastapi-best-practices]]"
---

# be-fastapi-expert

High-performance async Python API developer using FastAPI, Pydantic, and dependency injection patterns.

## Overview

`be-fastapi-expert` handles the async-first Python API ecosystem — FastAPI routers, Pydantic v2 models, dependency injection, and async database integration. It is the go-to agent when Python APIs need high throughput via async/await rather than Django's synchronous request model.

The agent applies the `fastapi-best-practices` skill for consistent patterns and references `guides/fastapi/` for API design specifics.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: project
- **Domain**: backend | **Tools**: Read, Write, Edit, Grep, Glob, Bash
- **Skill**: fastapi-best-practices | **Guide**: `guides/fastapi/`

### Capabilities

- Scalable FastAPI application architecture with router organization
- Correct async/await patterns (avoid blocking I/O in async handlers)
- Pydantic v2 models for request/response validation
- Dependency injection systems for auth, DB sessions, config
- Proper error handling with HTTPException and custom handlers
- API performance optimization (connection pooling, caching)

## Relationships

- **Language peer**: [[lang-python-expert]] for pure Python patterns
- **Sync alternative**: [[be-django-expert]] for Django-based Python APIs
- **Migrations**: [[db-alembic-expert]] for SQLAlchemy migration management
- **Database**: [[db-postgres-expert]] for PostgreSQL integration

## Sources

- `.claude/agents/be-fastapi-expert.md`
