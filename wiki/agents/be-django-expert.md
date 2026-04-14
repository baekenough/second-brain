---
title: be-django-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/be-django-expert.md
related:
  - "[[lang-python-expert]]"
  - "[[db-postgres-expert]]"
  - "[[db-alembic-expert]]"
  - "[[be-fastapi-expert]]"
  - "[[skills/django-best-practices]]"
  - "[[r010]]"
---

# be-django-expert

Production-ready Django developer specializing in Python web applications, Django REST Framework, and admin customization.

## Overview

`be-django-expert` handles Django-specific backend development — the full stack of models, views, templates, authentication, and deployment. It applies the `django-best-practices` skill for consistent patterns and references `guides/django-best-practices/` for specifics.

The agent enforces proper separation between Django app structure and business logic, preventing the common pitfall of fat views or models that mix concerns.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: project
- **Domain**: backend | **Tools**: Read, Write, Edit, Grep, Glob, Bash
- **Skill**: django-best-practices | **Guide**: `guides/django-best-practices/`

### Capabilities

- Scalable Django app structure with proper app segmentation
- Custom managers, querysets, and database constraints
- Class-based and function-based views with permission decorators
- Django REST Framework (DRF) serializers, viewsets, routers
- Django admin customization for internal tooling
- N+1 prevention, bulk operations, query optimization
- Secure deployment configuration

## Relationships

- **Language peer**: [[lang-python-expert]] (pure Python patterns)
- **Database**: [[db-postgres-expert]] for PostgreSQL tuning, [[db-alembic-expert]] for migrations
- **Alternative framework**: [[be-fastapi-expert]] for async APIs
- **Routing**: reached via [dev-lead-routing](../skills/dev-lead-routing.md)

## Sources

- `.claude/agents/be-django-expert.md`
