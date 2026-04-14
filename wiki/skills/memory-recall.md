---
title: memory-recall
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/memory-recall/SKILL.md
related:
  - "[[skills/memory-save]]"
  - "[[skills/memory-management]]"
  - "[[sys-memory-keeper]]"
  - "[[r011]]"
---

# memory-recall

Search and retrieve memories from claude-mem using semantic search with optional recency and date filtering.

## Overview

`memory-recall` (slash command: `/memory-recall`) queries the claude-mem Chroma database to retrieve relevant session memories. It prefixes queries with a project tag for scoped search. Results are sorted by relevance score and optionally filtered by date.

The primary use case is restoring context after compaction: recall recent decisions, open items, or specific topic memories before continuing work.

## Key Details

- **Scope**: core | **User-invocable**: true
- **Arguments**: `<query> [--recent] [--limit <n>] [--date <YYYY-MM-DD>] [--verbose]`
- Slash command: `/memory-recall`

## Options

| Flag | Effect |
|------|--------|
| `--recent` | Most recent memories (no query needed) |
| `--limit N` | Max results (default: 5) |
| `--date` | Filter by specific date |
| `--verbose` | Show full memory content |

## Relationships

- **Save counterpart**: [[skills/memory-save]] for storing memories
- **Architecture**: [[r011]] for memory scope and failure policy

## Sources

- `.claude/skills/memory-recall/SKILL.md`
