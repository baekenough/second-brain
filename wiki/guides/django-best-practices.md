---
title: "Guide: Django Best Practices"
type: guide
updated: 2026-04-12
sources:
  - guides/django-best-practices/README.md
related:
  - "[[be-django-expert]]"
  - "[[lang-python-expert]]"
  - "[[db-postgres-expert]]"
  - "[[skills/django-best-practices]]"
---

# Guide: Django Best Practices

Reference documentation for production Django development — app structure, models, views, DRF, authentication, and deployment.

## Overview

The Django best practices guide provides reference documentation for `be-django-expert` and the `django-best-practices` skill. It covers official Django documentation patterns and community-accepted best practices for production-ready Python web applications.

## Key Topics

- **App Structure**: App segmentation, project layout, settings management (base/dev/prod)
- **Models**: Custom managers and querysets, database constraints, migration patterns
- **Views**: Class-based vs function-based decision tree, permission decorators, mixins
- **Django REST Framework**: Serializers, viewsets, routers, authentication classes
- **Admin**: Customization patterns for internal tooling, `list_display`, custom actions
- **Security**: CSRF protection, XSS prevention, HTTPS enforcement, SECRET_KEY management
- **Performance**: N+1 prevention with `select_related`/`prefetch_related`, bulk operations, query optimization

## Relationships

- **Agent**: [[be-django-expert]] primary consumer
- **Skill**: [[skills/django-best-practices]] implements patterns
- **Language**: [[lang-python-expert]] for Python best practices beneath Django
- **Database**: [[db-postgres-expert]] for PostgreSQL-specific optimization

## Sources

- `guides/django-best-practices/README.md`
