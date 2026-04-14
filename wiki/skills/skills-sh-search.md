---
title: skills-sh-search
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/skills-sh-search/SKILL.md
related:
  - "[[mgr-creator]]"
  - "[[skills/create-agent]]"
---

# skills-sh-search

Search the skills.sh marketplace for reusable AI agent skills and install them when no matching internal skill exists.

## Overview

`skills-sh-search` provides a bridge to the external skills marketplace ([skills.sh](https://skills.sh)) when internal skills don't cover a needed capability. It searches by capability query, presents options, and optionally installs directly into the project's `.claude/skills/` directory or globally to `~/.claude/skills/`.

Requires Node.js and npx in PATH, and network access to the skills.sh registry.

## Key Details

- **Scope**: core | **User-invocable**: true
- **Arguments**: `<query> [--install] [--global] [--list] [--check]`
- Slash command: `/skills-sh-search`
- **Sources**: skills.sh (default), agentskills, or all

## Options

| Flag | Action |
|------|--------|
| `--install` | Install selected skill after search |
| `--global` | Install to `~/.claude/skills/` |
| `--list` | List installed skills.sh skills |
| `--check` | Check for updates |

## Relationships

- **Fallback**: Use when no internal skill matches the need
- **Alternatives**: [[mgr-creator]] for creating custom skills from scratch

## Sources

- `.claude/skills/skills-sh-search/SKILL.md`
