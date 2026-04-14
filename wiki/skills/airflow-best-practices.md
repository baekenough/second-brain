---
title: airflow-best-practices
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/airflow-best-practices/SKILL.md
related:
  - "[[de-airflow-expert]]"
  - "[[guides/airflow]]"
  - "[[de-pipeline-expert]]"
---

# airflow-best-practices

Core Airflow development patterns — top-level code avoidance, TaskFlow API, scheduling, testing, and secret backend integration.

## Overview

`airflow-best-practices` provides the implementation-level instructions that `de-airflow-expert` applies. It enforces the most critical Airflow rule: no top-level code in DAG files (which causes DAG parsing failures at scale). It also covers TaskFlow API patterns, scheduling strategies, and connection/variable management.

## Key Details

- **Scope**: core | **Used by**: de-airflow-expert | **Guide**: `guides/airflow/`

## Relationships

- **Agent**: [[de-airflow-expert]] applies this skill
- **Guide**: [airflow guide](../guides/airflow.md) provides reference documentation
- **Architecture**: [[de-pipeline-expert]] for cross-tool pipeline decisions

## Sources

- `.claude/skills/airflow-best-practices/SKILL.md`
