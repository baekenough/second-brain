---
title: lang-python-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/lang-python-expert.md
related:
  - "[[be-fastapi-expert]]"
  - "[[be-django-expert]]"
  - "[[de-airflow-expert]]"
  - "[[skills/python-best-practices]]"
---

# lang-python-expert

Expert Python developer for Pythonic, PEP 8-compliant code applying The Zen of Python principles.

## Overview

`lang-python-expert` is the language-layer Python agent — writing clean, idiomatic Python code, reviewing for PEP 8 compliance, and designing clean module structures. It differs from [[be-fastapi-expert]] and [[be-django-expert]] which operate at the framework layer.

"Pythonic" code means more than style: proper use of comprehensions over map/filter, context managers for resource cleanup, generators for lazy sequences, and the standard library before external dependencies.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: project
- **Domain**: backend | **Skill**: python-best-practices | **Guide**: `guides/python/`

### Capabilities

- PEP 8 compliance and The Zen of Python principles
- Module and package design with proper `__init__.py` structure
- Type annotations (PEP 484, 526, 563) with mypy compatibility
- Exception hierarchy design and proper exception handling
- Performance optimization: profiling, generators, slots
- Clean API design: ABC, protocols, dataclasses, NamedTuple

## Relationships

- **FastAPI framework**: [[be-fastapi-expert]] for async Python API services
- **Django framework**: [[be-django-expert]] for Django web applications
- **Data engineering**: [[de-airflow-expert]] for Python DAG authoring

## Sources

- `.claude/agents/lang-python-expert.md`
