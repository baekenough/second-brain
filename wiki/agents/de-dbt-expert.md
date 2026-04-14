---
title: de-dbt-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/de-dbt-expert.md
related:
  - "[[de-airflow-expert]]"
  - "[[de-snowflake-expert]]"
  - "[[db-postgres-expert]]"
  - "[[skills/dbt-best-practices]]"
  - "[[de-pipeline-expert]]"
---

# de-dbt-expert

dbt analytics engineer for SQL modeling, testing, documentation, and data transformation following dbt Labs best practices.

## Overview

`de-dbt-expert` handles the analytics engineering layer — transforming raw data into clean, tested, documented models. It enforces the three-layer model structure (staging → intermediate → marts) with proper materialization choices and schema tests.

The agent's strength is preventing the common dbt anti-pattern of putting business logic directly in marts without intermediate models, which creates untestable, duplicated SQL.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: project
- **Domain**: data-engineering | **Skill**: dbt-best-practices | **Guide**: `guides/dbt/`

### Layer Structure

| Prefix | Layer | Purpose |
|--------|-------|---------|
| `stg_` | Staging | Raw source cleaning, type casting |
| `int_` | Intermediate | Business logic joins, calculations |
| `fct_` / `dim_` | Marts | Final analytical models |

### Capabilities

- Materializations: view, ephemeral, table, incremental
- Schema tests: unique, not_null, relationships, accepted_values
- Jinja macros for DRY SQL patterns
- Sources, seeds, snapshots, model documentation

## Relationships

- **Orchestration**: [[de-airflow-expert]] for Airflow-dbt integration
- **Warehouse**: [[de-snowflake-expert]] for Snowflake-specific optimizations
- **Database**: [[db-postgres-expert]] for PostgreSQL dbt targets
- **Architecture**: [[de-pipeline-expert]] for multi-tool pipeline design

## Sources

- `.claude/agents/de-dbt-expert.md`
