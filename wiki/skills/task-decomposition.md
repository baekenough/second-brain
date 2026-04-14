---
title: task-decomposition
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/task-decomposition/SKILL.md
related:
  - "[[skills/dag-orchestration]]"
  - "[[skills/worker-reviewer-pipeline]]"
  - "[[r009]]"
  - "[[r018]]"
---

# task-decomposition

Auto-decompose large tasks into DAG-compatible parallel subtasks — the planning frontend before `dag-orchestration` execution.

## Overview

`task-decomposition` analyzes task complexity and selects the appropriate workflow pattern before execution begins. This prevents the orchestrator from naively sequencing parallelizable work. It uses `context: fork` for isolated planning.

The trigger thresholds define "large": >30 min, >3 files, >2 domains, >2 agent types. Any one threshold met recommends decomposition.

## Key Details

- **Scope**: core | **User-invocable**: false | **Context**: fork

## Pattern Selection

| Pattern | When |
|---------|------|
| Sequential | Each step depends on previous |
| Parallel | Independent subtasks, no shared state |
| Evaluator-Optimizer | Quality-critical, iterative refinement needed |
| Orchestrator | Complex multi-step with dynamic routing |

## Trigger Thresholds

| Trigger | Threshold |
|---------|-----------|
| Estimated duration | > 30 min |
| Files affected | > 3 |
| Domains involved | > 2 |
| Agent types needed | > 2 |

## Relationships

- **Execution**: [[skills/dag-orchestration]] executes the decomposed DAG
- **Quality gate**: [[skills/worker-reviewer-pipeline]] for evaluator-optimizer pattern
- **Parallelism**: [[r009]] for parallel subtask execution

## Sources

- `.claude/skills/task-decomposition/SKILL.md`
