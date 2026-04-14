---
title: wiki
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/wiki/SKILL.md
related:
  - "[[wiki-curator]]"
  - "[[skills/wiki-rag]]"
  - "[[r022]]"
  - "[[r017]]"
---

# wiki

Generate and maintain a persistent, interlinked codebase knowledge base — LLM-built incremental markdown wiki inspired by Karpathy's LLM Wiki pattern.

## Overview

`wiki` (slash command: `/omcustom:wiki`) builds and maintains the `wiki/` directory incrementally: only pages whose sources changed since the last run are rewritten. The wiki grows richer over time and becomes the fastest path to codebase understanding for both humans and LLMs.

All wiki write operations are delegated to [[wiki-curator]] per R010 and R022. The wiki skill coordinates the workflow; the curator agent executes the writes.

## Key Details

- **Scope**: core | **User-invocable**: true | **Effort**: high | **Version**: 1.0.0
- **Arguments**: `[ingest|query|lint] [args...]`
- Slash command: `/omcustom:wiki`

## Commands

| Command | Action |
|---------|--------|
| `/omcustom:wiki` | Full generation / incremental update |
| `/omcustom:wiki ingest <path>` | Ingest specific file or directory |
| `/omcustom:wiki query <question>` | Natural language query |
| `/omcustom:wiki lint` | Health check — orphans, broken refs, stale pages |

## Wiki Structure

`wiki/index.yaml` (catalog), `wiki/log.duckdb` (operation log), `wiki/agents/`, `wiki/skills/`, `wiki/rules/`, `wiki/guides/`, `wiki/architecture/`.

## Relationships

- **Writer**: [[wiki-curator]] executes all wiki file writes
- **Query**: [[skills/wiki-rag]] queries the generated wiki
- **Sync rule**: [[r022]] defines when wiki must be updated

## Sources

- `.claude/skills/wiki/SKILL.md`
