---
title: de-airflow-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/de-airflow-expert.md
related:
  - "[[de-dbt-expert]]"
  - "[[de-pipeline-expert]]"
  - "[[de-spark-expert]]"
  - "[[skills/airflow-best-practices]]"
  - "[[r009]]"
---

# de-airflow-expert

Apache Airflow developer for production-ready DAG authoring, scheduling, testing, and secret backend integration.

## Overview

`de-airflow-expert` handles Airflow DAG development following official best practices — especially the critical rule against top-level code in DAG files (which causes DAG parsing failures at scale). It supports both the classic operator API and the modern TaskFlow API.

The agent's scope is trigger/schedule pattern design, task dependency graphs, connection/variable management, and unit testing DAGs without a running Airflow instance.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: project
- **Domain**: data-engineering | **Skill**: airflow-best-practices | **Guide**: `guides/airflow/`

### Capabilities

- DAG authoring: top-level code avoidance, idempotent tasks
- TaskFlow API (`@task` decorators) and classic operators
- Scheduling: cron expressions, timetables, data-aware scheduling
- DAG unit testing (pytest + Airflow test utilities)
- Connection/Variable management and secret backend integration (Vault, AWS SSM)
- DAG parsing performance optimization

## Relationships

- **Transformation layer**: [[de-dbt-expert]] for SQL transformations orchestrated by Airflow
- **Compute**: [[de-spark-expert]] for Spark jobs triggered via SparkSubmitOperator
- **Architecture**: [[de-pipeline-expert]] for cross-tool pipeline design decisions
- **Parallel execution**: [[r009]] when building multiple DAGs simultaneously

## Sources

- `.claude/agents/de-airflow-expert.md`
