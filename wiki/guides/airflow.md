---
title: "Guide: Apache Airflow"
type: guide
updated: 2026-04-12
sources:
  - guides/airflow/README.md
related:
  - "[[de-airflow-expert]]"
  - "[[de-dbt-expert]]"
  - "[[de-spark-expert]]"
  - "[[de-pipeline-expert]]"
  - "[[skills/airflow-best-practices]]"
---

# Guide: Apache Airflow

Reference documentation for Apache Airflow DAG development, scheduling patterns, testing, and secret backend integration.

## Overview

The Airflow guide provides reference documentation for the `de-airflow-expert` agent and `airflow-best-practices` skill. It covers official best practices from the Apache Airflow documentation, focusing on production DAG authoring patterns.

## Key Topics

- **DAG Authoring**: Top-level code avoidance (critical for DAG parsing performance at scale), TaskFlow API vs classic operators
- **Scheduling**: Cron expressions, timetables, data-aware scheduling triggers
- **Testing**: Unit testing DAGs with pytest without a running Airflow instance
- **Secret Management**: Connection/Variable management, secret backend integration (Vault, AWS SSM, GCP Secret Manager)
- **Performance**: DAG parsing optimization, task distribution, worker configuration

## Primary Audiences

- `de-airflow-expert` agent (primary consumer)
- `airflow-best-practices` skill (pattern implementation)
- `de-pipeline-expert` for cross-tool orchestration patterns

## Relationships

- **Agent**: [[de-airflow-expert]] consults this guide for specifics
- **Skill**: [[skills/airflow-best-practices]] implements patterns from this guide
- **Connected tools**: [[de-dbt-expert]], [[de-spark-expert]] for Airflow-orchestrated workloads
- **Architecture**: [[de-pipeline-expert]] for multi-tool design decisions

## Sources

- `guides/airflow/README.md`
