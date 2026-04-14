---
title: pipeline
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/pipeline/SKILL.md
related:
  - "[[skills/dag-orchestration]]"
  - "[[skills/pipeline-guards]]"
  - "[[skills/pipeline-architecture-patterns]]"
  - "[[de-pipeline-expert]]"
---

# pipeline

Invoke and resume YAML-defined named pipelines — `/pipeline auto-dev` runs the full release pipeline; no args lists available pipelines.

## Overview

`pipeline` (slash command: `/pipeline`) is the external-sourced pipeline invocation skill (`baekenough/baekenough-skills`). It reads YAML pipeline definitions from `workflows/*.yaml`, supports named invocation and resume of halted pipelines. Each pipeline YAML defines its own steps, dependencies, and guards.

The `auto-dev` pipeline is the primary use case: a full release workflow from issue triage through PR creation.

## Key Details

- **Scope**: harness | **User-invocable**: true | **Effort**: high
- **Arguments**: `<pipeline-name> | resume | (no args to list)`
- Slash command: `/pipeline`
- **Source**: external (github: baekenough/baekenough-skills v1.0.0)

## Pipeline Files

- `workflows/*.yaml` — production pipelines
- `templates/workflows/*.yaml` — template examples

## Relationships

- **Orchestration**: [[skills/dag-orchestration]] for the DAG execution engine
- **Safety**: [[skills/pipeline-guards]] for iteration limits and timeout enforcement

## Sources

- `.claude/skills/pipeline/SKILL.md`
