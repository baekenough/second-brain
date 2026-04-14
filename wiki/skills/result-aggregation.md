---
title: result-aggregation
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/result-aggregation/SKILL.md
related:
  - "[[skills/research]]"
  - "[[r013]]"
  - "[[r009]]"
---

# result-aggregation

Format and aggregate results from multiple parallel agent executions into concise, scannable batch output.

## Overview

`result-aggregation` provides the standard output format for parallel agent results, especially in ecomode (R013). It consolidates multiple agent outcomes into a structured batch summary with status icons and per-agent summaries, replacing verbose individual agent outputs with a single scannable block.

## Key Details

- **Scope**: core | **User-invocable**: false

## Standard Batch Format

```
[Batch Complete] {completed}/{total}
├── {agent}: ✓ {summary}
├── {agent}: ✗ {summary}
└── {agent}: ⚠ {summary}
```

## When to Use

- After parallel agent execution completes
- When ecomode is active (R013)
- For batch operation summaries
- When reporting multi-agent task results

## Relationships

- **Research**: [[skills/research]] produces 10-team output that this skill aggregates
- **Ecomode**: [[r013]] activates compressed aggregation format
- **Parallel**: [[r009]] defines the parallel execution pattern

## Sources

- `.claude/skills/result-aggregation/SKILL.md`
