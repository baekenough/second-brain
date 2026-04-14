---
title: python-best-practices
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/python-best-practices/SKILL.md
related:
  - "[[lang-python-expert]]"
  - "[[guides/python]]"
  - "[[skills/fastapi-best-practices]]"
  - "[[skills/django-best-practices]]"
---

# python-best-practices

Pythonic patterns from PEP 8 and PEP 20 (The Zen of Python): naming, type hints, error handling, testing, and code quality tooling.

## Overview

`python-best-practices` encodes PEP 8 style and The Zen of Python as concrete coding rules. The philosophy: explicit over implicit, simple over complex, readability counts. These principles translate into: use type hints everywhere (Python 3.10+ union syntax `X | Y`), prefer `pathlib.Path` over string paths, use dataclasses or Pydantic for data structures, and let Ruff handle all formatting/linting.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Consumed by**: [lang-python-expert](../agents/lang-python-expert.md)

## Core Rules

- **Naming**: `snake_case` functions/variables, `UpperCamelCase` classes, `SCREAMING_SNAKE_CASE` constants
- **Type hints**: required on all public functions; use `X | Y` not `Union[X, Y]` (Python 3.10+)
- **Errors**: specific exceptions only; never bare `except:` or `except Exception:` without re-raise
- **Context managers**: `with` for all file/resource management
- **F-strings**: preferred over `%` or `.format()`
- **`pathlib.Path`**: always over `os.path` string manipulation
- **Tooling**: Ruff (format + lint), mypy (type checking), pytest (testing)

## Never

- Mutable default arguments (`def f(x=[])`)
- `import *`
- Wildcard exception handling

## Relationships

- **Agent**: [[lang-python-expert]] applies these patterns
- **FastAPI extension**: [[skills/fastapi-best-practices]] for async API patterns
- **Guide**: [[guides/python]] for extended reference

## Sources

- `.claude/skills/python-best-practices/SKILL.md`
