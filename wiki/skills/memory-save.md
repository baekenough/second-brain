---
title: memory-save
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/memory-save/SKILL.md
related:
  - "[[skills/memory-recall]]"
  - "[[skills/memory-management]]"
  - "[[sys-memory-keeper]]"
  - "[[r011]]"
---

# memory-save

Save current session context to claude-mem for persistence across context compaction.

## Overview

`memory-save` (slash command: `/memory-save`) collects the current session's tasks, decisions, open items, and optionally code changes, then stores them in the claude-mem Chroma database with project and session ID tags. This enables recall in future sessions after context compaction.

The skill is designed to be called at session end or at key checkpoints. It is non-blocking — save failures are reported but do not interrupt the session.

## Key Details

- **Scope**: core | **User-invocable**: true
- **Arguments**: `[--tags <tags>] [--include-code] [--summary <text>] [--verbose]`
- Slash command: `/memory-save`
- `disable-model-invocation: true` (script-driven, not model-driven)

## What Gets Saved

- Tasks completed in this session
- Key decisions made
- Open items and next steps
- Code changes (with `--include-code`)

## Relationships

- **Recall counterpart**: [[skills/memory-recall]] for retrieving saved memories
- **Architecture**: [[r011]] for memory scope and session-end flow

## Sources

- `.claude/skills/memory-save/SKILL.md`
