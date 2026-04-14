---
title: dev-refactor
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/dev-refactor/SKILL.md
related:
  - "[[skills/dev-review]]"
  - "[[skills/dev-lead-routing]]"
  - "[[lang-golang-expert]]"
---

# dev-refactor

Refactor code for better structure, naming, and patterns using language-specific expert agents.

## Overview

`dev-refactor` provides a structured refactoring workflow that delegates to the appropriate language expert via dev-lead-routing. It can target specific files, directories, or language files, with optional spec mode (`--spec`) for spec-driven refactoring.

## Key Details

- **Scope**: core | **User-invocable**: true
- **Arguments**: `<file-or-directory> [--lang <language>] [--spec]`
- Slash command: `/dev-refactor`

## Relationships

- **Review pair**: [dev-review](dev-review.md) for pre/post-refactor code review
- **Routing**: [dev-lead-routing](dev-lead-routing.md) selects language expert
- **Language expert**: e.g., [[lang-golang-expert]] executes the actual refactoring

## Sources

- `.claude/skills/dev-refactor/SKILL.md`
