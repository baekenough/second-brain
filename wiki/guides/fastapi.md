---
title: "Guide: FastAPI"
type: guide
updated: 2026-04-12
sources:
  - guides/fastapi/best-practices.md
related:
  - "[[be-fastapi-expert]]"
  - "[[lang-python-expert]]"
  - "[[db-alembic-expert]]"
  - "[[db-postgres-expert]]"
  - "[[skills/fastapi-best-practices]]"
---

# Guide: FastAPI

Reference documentation for FastAPI — high-performance async Python API framework with Pydantic v2 and dependency injection.

## Overview

The FastAPI guide provides reference documentation for `be-fastapi-expert` and the `fastapi-best-practices` skill. It covers async/await correctness, Pydantic v2 model patterns, dependency injection design, and production deployment configurations.

## Key Topics

- **Async Patterns**: Correct async/await usage, avoiding blocking I/O in async handlers
- **Pydantic v2 Models**: `BaseModel`, `Field`, validators, discriminated unions
- **Dependency Injection**: `Depends()` for auth, DB sessions, config, request scoping
- **Router Organization**: APIRouter, prefix, tags, include_router patterns
- **Error Handling**: HTTPException, custom exception handlers, validation error formatting
- **Performance**: Connection pooling (asyncpg), background tasks, response caching
- **Security**: OAuth2 with JWT, API key authentication, CORS configuration

## Relationships

- **Agent**: [[be-fastapi-expert]] primary consumer
- **Skill**: [[skills/fastapi-best-practices]] implements patterns
- **Language**: [[lang-python-expert]] for Python patterns beneath FastAPI
- **Migrations**: [[db-alembic-expert]] for async SQLAlchemy integration

## Sources

- `guides/fastapi/best-practices.md`
