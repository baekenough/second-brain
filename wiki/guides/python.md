---
title: "Guide: Python"
type: guide
updated: 2026-04-12
sources:
  - guides/python/index.yaml
related:
  - "[[lang-python-expert]]"
  - "[[be-fastapi-expert]]"
  - "[[be-django-expert]]"
  - "[[skills/python-best-practices]]"
---

# Guide: Python

Reference documentation for Python — PEP 8, Zen of Python, type annotations, async patterns, and standard library.

## Overview

The Python guide provides reference documentation for `lang-python-expert` and the `python-best-practices` skill. It compiles PEP 8, PEP 484 (type hints), PEP 563 (postponed evaluation), the Zen of Python, and official Python documentation for idiomatic Python code.

## Key Topics

- **PEP 8**: Style guide — naming, indentation, line length, imports, whitespace
- **Zen of Python**: 19 design principles guiding API and module design decisions
- **Type Annotations**: PEP 484, `typing` module, `dataclass`, `TypedDict`, `Protocol`
- **Async Patterns**: `asyncio`, `async/await`, `aiohttp`, event loop management
- **Comprehensions**: List, dict, set, generator expressions — when to prefer vs loops
- **Standard Library**: `pathlib`, `collections`, `itertools`, `functools`, `contextlib`
- **Performance**: `__slots__`, generators for memory efficiency, `cProfile` profiling

## Relationships

- **Agent**: [[lang-python-expert]] primary consumer
- **FastAPI**: [[be-fastapi-expert]] builds async APIs on Python patterns
- **Django**: [[be-django-expert]] builds web apps on Python patterns
- **Skill**: [[skills/python-best-practices]] implements PEP 8 patterns

## Sources

- `guides/python/index.yaml`
