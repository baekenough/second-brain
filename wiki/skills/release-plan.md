---
title: release-plan
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/release-plan/SKILL.md
related:
  - "[[skills/professor-triage]]"
  - "[[skills/omcustom-release-notes]]"
  - "[[mgr-gitnerd]]"
---

# release-plan

Generate structured release plans from `verify-done` labeled GitHub issues, grouped by priority and estimated size.

## Overview

`release-plan` (slash command: `/release-plan`) takes issues that have been triaged by `professor-triage` (labeled `verify-done`) and generates a structured release plan document. It groups issues by priority (P0-P3) and estimated size (XS-XL), determines the appropriate version bump (major/minor/patch), and produces a plan document. Plan only — no implementation, no commits.

## Key Details

- **Scope**: harness | **User-invocable**: true | **Effort**: medium
- **Arguments**: `[--next major|minor|patch] [--dry-run]`
- Slash command: `/release-plan`
- **Input**: GitHub issues labeled `verify-done` (open state)

## Workflow

1. Collect open issues with `verify-done` label
2. Group by priority and size
3. Determine version bump type
4. Generate structured release plan document

## Relationships

- **Input**: [[skills/professor-triage]] produces the `verify-done` labeled issues
- **Release notes**: [[skills/omcustom-release-notes]] generates notes after release

## Sources

- `.claude/skills/release-plan/SKILL.md`
