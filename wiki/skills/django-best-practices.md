---
title: django-best-practices
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/django-best-practices/SKILL.md
related:
  - "[[be-django-expert]]"
  - "[[lang-python-expert]]"
  - "[[guides/django-best-practices]]"
  - "[[skills/python-best-practices]]"
  - "[[db-postgres-expert]]"
---

# django-best-practices

Production Django patterns covering models, views, security, DRF, testing, and deployment.

## Overview

`django-best-practices` encodes Django conventions that prevent common production issues: custom User model requirement (impossible to swap mid-project), split settings (base/development/production), N+1 query prevention via `select_related`/`prefetch_related`, and the security checklist (`python manage.py check --deploy`).

The skill emphasizes thin views — business logic belongs in `services.py`, not view handlers. CBVs are preferred for standard CRUD; FBVs for custom workflows.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Consumed by**: [be-django-expert](../agents/be-django-expert.md)

## Key Rules

- **Custom User model**: Always create before any other model — impossible to change mid-project
- **Settings split**: `config/settings/{base,development,production}.py`
- **Never**: `fields = '__all__'` in ModelForm or ModelSerializer
- **N+1**: `select_related` for FK, `prefetch_related` for M2M
- **Testing**: pytest-django + factory_boy (avoid fixtures)
- **ORM only**: Parameterized queries if raw SQL is unavoidable
- **HTTPS**: `SECURE_SSL_REDIRECT`, `SESSION_COOKIE_SECURE`, `CSRF_COOKIE_SECURE`

## DRF Conventions

ModelSerializer for CRUD, ViewSet with DefaultRouter, `djangorestframework-simplejwt` for JWT, object-level permissions via `has_object_permission()`.

## Deployment Checklist

Multi-stage: gunicorn (WSGI) or uvicorn+gunicorn (ASGI), whitenoise or CDN for static, PostgreSQL (never SQLite in production), migrations in CI/CD pipeline before server restart.

## Relationships

- **Agent**: [[be-django-expert]] applies these patterns
- **Database**: [[db-postgres-expert]] for production DB setup
- **Guide**: [[guides/django-best-practices]] for extended reference

## Sources

- `.claude/skills/django-best-practices/SKILL.md`
