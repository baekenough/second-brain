---
title: dev-review
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/dev-review/SKILL.md
related:
  - "[[skills/dev-refactor]]"
  - "[[skills/dev-lead-routing]]"
  - "[[skills/go-best-practices]]"
  - "[[skills/python-best-practices]]"
  - "[[skills/typescript-best-practices]]"
  - "[[lang-golang-expert]]"
  - "[[lang-python-expert]]"
  - "[[lang-typescript-expert]]"
---

# dev-review

Code review skill that delegates to language-specific expert agents and applies pre-flight guards before starting.

## Overview

`dev-review` orchestrates code review by first running pre-flight guards to detect cases where review is inappropriate (auto-generated code, formatting-only changes, single syntax errors), then selecting the correct language expert via file extension detection. Results are structured by finding category (Style, Error Handling, Performance, Security).

The skill is designed to avoid false positives — it asks the user to confirm before reviewing generated code (`.pb.go`, `*_generated.*`, etc.) and informs when a linter would be faster.

## Key Details

- **Scope**: core | **User-invocable**: true
- **Arguments**: `<file-or-directory> [--lang <language>] [--focus <area>] [--verbose]`
- Slash command: `/dev-review`

## Pre-flight Guards

| Guard | Level | Condition |
|-------|-------|-----------|
| Auto-generated code | WARN | `DO NOT EDIT`, `@generated`, `.pb.go` detected |
| Formatting-only changes | INFO | `git diff -w` is empty but regular diff has changes |
| Single syntax error | INFO | Single file + error/syntax keywords in request |
| Linter available | INFO | `.eslintrc*`, `.golangci.yml`, `pyproject.toml` found |

## Agent Selection

| Extension | Agent | Skill |
|-----------|-------|-------|
| `.go` | [lang-golang-expert](../agents/lang-golang-expert.md) | go-best-practices |
| `.py` | [lang-python-expert](../agents/lang-python-expert.md) | python-best-practices |
| `.ts/.tsx` | [lang-typescript-expert](../agents/lang-typescript-expert.md) | typescript-best-practices |
| `.rs` | lang-rust-expert | rust-best-practices |
| `.kt` | lang-kotlin-expert | kotlin-best-practices |
| `.java` | be-springboot-expert | springboot-best-practices |

## Relationships

- **Pair**: [[skills/dev-refactor]] for post-review refactoring
- **Routing**: [[skills/dev-lead-routing]] selects expert agents

## Sources

- `.claude/skills/dev-review/SKILL.md`
