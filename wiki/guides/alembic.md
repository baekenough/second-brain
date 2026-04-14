---
title: "Guide: Alembic"
type: guide
updated: 2026-04-12
sources:
  - guides/alembic/README.md
related:
  - "[[db-alembic-expert]]"
  - "[[db-postgres-expert]]"
  - "[[be-fastapi-expert]]"
  - "[[skills/alembic-best-practices]]"
---

# Guide: Alembic

Reference documentation for Alembic database migration framework (SQLAlchemy), covering current stable 1.18.x patterns.

## Overview

The Alembic guide provides reference documentation consumed by `db-alembic-expert` and the `alembic-best-practices` skill. It documents migration generation patterns, env.py configuration variants, dangerous operation detection, and CI integration strategies for zero-downtime deployments.

## Key Topics

- **Migration Generation**: `alembic revision --autogenerate` workflows and post-generation safety review
- **Expand-Contract Pattern**: Zero-downtime schema changes across three deployment phases
- **env.py Variants**: Sync, async (asyncpg), multi-tenant, and multi-database configurations
- **Dangerous Patterns**: Rename-as-drop+add detection, anonymous constraints, lock-risky DDL
- **pytest-alembic**: Built-in tests (single_head_revision, model_match) and custom data checks
- **alembic-utils**: PostgreSQL-specific objects (views, functions, RLS) as Replaceable Objects
- **CI Integration**: `alembic check` for pending detection, Squawk for lock-risk analysis

## Relationships

- **Agent**: [[db-alembic-expert]] primary consumer
- **Skill**: [[skills/alembic-best-practices]] implements safety patterns
- **Database**: [[db-postgres-expert]] for PostgreSQL DDL nuances
- **Application**: [[be-fastapi-expert]] for async engine configuration

## Sources

- `guides/alembic/README.md`
