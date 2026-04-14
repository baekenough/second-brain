---
title: "Guide: dbt"
type: guide
updated: 2026-04-12
sources:
  - guides/dbt/README.md
related:
  - "[[de-dbt-expert]]"
  - "[[de-airflow-expert]]"
  - "[[de-snowflake-expert]]"
  - "[[skills/dbt-best-practices]]"
---

# Guide: dbt

Reference documentation for dbt analytics engineering — three-layer modeling structure, materializations, testing, and documentation.

## Overview

The dbt guide provides reference documentation for `de-dbt-expert` and the `dbt-best-practices` skill. It follows dbt Labs official patterns for analytics engineering workflows, covering model organization, testing strategies, and Jinja macro patterns.

## Key Topics

- **Model Layers**: staging (`stg_`), intermediate (`int_`), marts (`fct_`, `dim_`) — three-layer separation
- **Materializations**: view, ephemeral, table, incremental (incremental_strategy options)
- **Testing**: built-in tests (unique, not_null, relationships, accepted_values), custom tests
- **Jinja Macros**: DRY SQL patterns, conditional logic, cross-database compatibility
- **Sources**: source definitions, freshness checks, source testing
- **Documentation**: YAML descriptions, doc blocks, dbt docs site generation
- **Snapshots**: slowly changing dimension (SCD) type 2 patterns

## Relationships

- **Agent**: [[de-dbt-expert]] primary consumer
- **Skill**: [[skills/dbt-best-practices]] implements patterns
- **Orchestration**: [[de-airflow-expert]] for Airflow-orchestrated dbt runs
- **Warehouse**: [[de-snowflake-expert]] for Snowflake dbt targets

## Sources

- `guides/dbt/README.md`
