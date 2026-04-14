---
title: sys-memory-keeper
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/sys-memory-keeper.md
related:
  - "[[sys-naggy]]"
  - "[[r011]]"
  - "[[skills/memory-management]]"
  - "[[skills/memory-save]]"
  - "[[skills/memory-recall]]"
---

# sys-memory-keeper

Session memory management specialist for native auto-memory persistence, session summaries, behavior extraction, and confidence decay management.

## Overview

`sys-memory-keeper` is the memory subsystem agent. At session end (triggered by orchestrator signal), it: collects session summary, extracts behavioral patterns (communication/workflow/domain preferences), updates MEMORY.md with confidence scoring, and aggregates agent performance metrics.

A critical design constraint: MCP tools (claude-mem, episodic-memory) are orchestrator-scoped and not available to subagents. sys-memory-keeper handles native auto-memory (MEMORY.md), and the *orchestrator* handles MCP saves after receiving the formatted summary.

## Key Details

- **Model**: sonnet | **Effort**: medium | **Memory**: project
- **maxTurns**: 15 | **Skills**: memory-management, memory-save, memory-recall
- **Limitations**: cannot modify source code, cannot execute tests

### Session-End Operations

1. Collect completed tasks, key decisions, open items
2. Extract user behavior patterns (communication, workflow, domain preferences)
3. Update MEMORY.md with session learnings + confidence scores
4. Aggregate agent performance metrics (success rates, model distribution)
5. Extract user model data (skill preferences, correction patterns, expertise profile)
6. Return formatted summary to orchestrator for MCP persistence

### Confidence Decay

Unverified memory entries decay: 30+ days → demote level, 60+ days → demote again, 90+ days → flag STALE. `[permanent]` entries exempt.

## Relationships

- **Task tracking**: [[sys-naggy]] for TODO/reminder management
- **Orchestrator**: receives session-end signal, returns summary for MCP saves
- **Rules**: [[r011]] (memory integration), confidence decay and dual-system save protocols

## Sources

- `.claude/agents/sys-memory-keeper.md`
