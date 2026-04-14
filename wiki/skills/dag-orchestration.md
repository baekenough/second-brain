---
title: dag-orchestration
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/dag-orchestration/SKILL.md
related:
  - "[[r009]]"
  - "[[r018]]"
  - "[[skills/task-decomposition]]"
  - "[[skills/pipeline-guards]]"
---

# dag-orchestration

YAML-based DAG workflow engine with topological execution and failure strategies.

## Overview

`dag-orchestration` provides a YAML-based DAG (Directed Acyclic Graph) workflow engine for orchestrating complex multi-step agent workflows. It executes tasks in topological order, handling dependencies, parallel branches, and failure recovery strategies. Uses `context: fork` for isolated orchestration execution.

## Key Details

- **Scope**: core | **context**: fork | **User-invocable**: false
- Defines YAML workflow format for agent task graphs

## Relationships

- **Parallelism**: [[r009]] — DAG execution parallelizes independent branches
- **Agent Teams**: [[r018]] — DAG may coordinate Agent Teams for qualifying workflows
- **Decomposition**: [task-decomposition](task-decomposition.md) for breaking large tasks into DAG-compatible subtasks
- **Guards**: [pipeline-guards](pipeline-guards.md) for safety constraints on DAG execution

## Sources

- `.claude/skills/dag-orchestration/SKILL.md`
