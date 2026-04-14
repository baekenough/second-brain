---
title: alembic-best-practices
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/alembic-best-practices/SKILL.md
related:
  - "[[db-alembic-expert]]"
  - "[[skills/postgres-best-practices]]"
  - "[[guides/alembic]]"
---

# alembic-best-practices

Alembic migration safety patterns — dangerous operation detection, expand-contract design, naming conventions, and pytest-alembic test setup.

## Overview

`alembic-best-practices` provides implementation-level instructions for `db-alembic-expert`. It defines the safety rules (no auto-fix column renames, no credentials in alembic.ini, CONCURRENTLY for large indexes) and the expand-contract pattern for zero-downtime migrations.

## Key Details

- **Scope**: core | **Used by**: db-alembic-expert | **Guide**: `guides/alembic/`

## Relationships

- **Agent**: [[db-alembic-expert]] applies this skill
- **PostgreSQL**: [postgres-best-practices](postgres-best-practices.md) complementary skill
- **Guide**: [alembic guide](../guides/alembic.md)

## Sources

- `.claude/skills/alembic-best-practices/SKILL.md`
