---
title: pipeline-guards
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/pipeline-guards/SKILL.md
related:
  - "[[skills/dag-orchestration]]"
  - "[[skills/worker-reviewer-pipeline]]"
  - "[[skills/evaluator-optimizer]]"
  - "[[r009]]"
---

# pipeline-guards

System-wide safety constraints and quality gates for all pipeline and iterative workflow execution — prevents infinite loops, enforces timeouts, limits parallel agents.

## Overview

`pipeline-guards` defines the hard limits that all pipeline skills (dag-orchestration, worker-reviewer-pipeline, evaluator-optimizer) must respect. These are two-level constraints: skill-level soft limits (warn and cap) and hook-level hard blocks (prevent execution). The skill uses `context: fork` for isolated enforcement.

## Key Details

- **Scope**: core | **User-invocable**: false | **Context**: fork
- **System-wide**: applies to all pipeline, DAG, and iterative skills

## Guard Limits

| Guard | Default | Hard Cap |
|-------|---------|---------|
| Max iterations | 3 | 5 |
| Max DAG nodes | 20 | 30 |
| Max parallel agents | 4 | 5 |
| Timeout per node | 300s | 600s |
| Timeout per pipeline | 900s | 1800s |
| Max retry count | 2 | 3 |
| Max PR improvement items | 20 | 50 |

## Enforcement Levels

- **Level 1 (Skill)**: Check before execution → warn user → use hard cap value
- **Level 2 (Hook)**: PreToolUse hook blocks tool call if hard cap exceeded

## Relationships

- **DAG**: [[skills/dag-orchestration]] respects node and timeout limits
- **Iterative**: [[skills/worker-reviewer-pipeline]] and [[skills/evaluator-optimizer]] respect iteration limits
- **Parallelism**: [[r009]] for the max parallel agents policy

## Sources

- `.claude/skills/pipeline-guards/SKILL.md`
