---
title: lists
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/lists/SKILL.md
related:
  - "[[skills/help]]"
  - "[[skills/status]]"
---

# lists

Harness skill that displays all available slash commands, optionally filtered by category.

## Overview

`lists` (slash command: `/omcustom:lists`) renders the full command catalog of the oh-my-customcode system, grouped into System, Manager, and Dev categories. With `--verbose` it outputs a formatted table with descriptions. With `--category` it filters to a specific group. This is a read-only display skill with no side effects.

## Key Details

- **Scope**: harness | **User-invocable**: true
- **Arguments**: `[--category <category>] [--verbose]`
- Slash command: `/omcustom:lists`

## Categories

| Category | Commands |
|----------|---------|
| System | lists, status, help |
| Manager | create-agent, update-docs, update-external, audit-agents, fix-refs |
| Dev | dev-review, dev-refactor |

## Relationships

- **Detailed help**: [[skills/help]] for per-command usage
- **Runtime state**: [[skills/status]] for system health

## Sources

- `.claude/skills/lists/SKILL.md`
