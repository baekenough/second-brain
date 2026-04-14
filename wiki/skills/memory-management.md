---
title: memory-management
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/memory-management/SKILL.md
related:
  - "[[sys-memory-keeper]]"
  - "[[skills/memory-save]]"
  - "[[skills/memory-recall]]"
  - "[[r011]]"
---

# memory-management

Core memory persistence operations using claude-mem for session context survival across context compaction events.

## Overview

`memory-management` provides the underlying save/recall/prune operations that `sys-memory-keeper` uses. It defines the three-operation pattern: save (collect → format → store in claude-mem with project tag and session ID), recall (build query → search → format results), and prune (identify outdated entries → remove).

This is the internal skill; users interact via the user-invocable [[skills/memory-save]] and [[skills/memory-recall]] counterparts.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Consumed by**: [sys-memory-keeper](../agents/sys-memory-keeper.md)
- **Storage**: claude-mem (Chroma-based MCP)

## Operations

| Operation | Action |
|-----------|--------|
| Save | Collect context → format with project+session tags → store |
| Recall | Semantic query → retrieve → sort by relevance |
| Prune | Identify stale/low-relevance entries → remove |

## Relationships

- **User-facing**: [[skills/memory-save]] and [[skills/memory-recall]] for direct invocation
- **Rule**: [[r011]] for memory architecture and failure policy

## Sources

- `.claude/skills/memory-management/SKILL.md`
