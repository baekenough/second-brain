---
title: pipeline-architecture-patterns
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/pipeline-architecture-patterns/SKILL.md
related:
  - "[[de-pipeline-expert]]"
  - "[[de-airflow-expert]]"
  - "[[de-spark-expert]]"
  - "[[de-kafka-expert]]"
  - "[[skills/dag-orchestration]]"
---

# pipeline-architecture-patterns

Data pipeline architecture reference: ETL vs ELT, Lambda/Kappa/Medallion architectures, DAG orchestration patterns, and data quality frameworks.

## Overview

`pipeline-architecture-patterns` provides the architectural decision framework for data engineering pipelines. The central ETL vs ELT decision depends on the compute location: traditional on-premise warehouses favor ETL (transform before load); cloud warehouses (Snowflake, BigQuery) favor ELT (load raw, transform in warehouse using warehouse compute).

Medallion Architecture (Bronze/Silver/Gold) is the current standard for lakehouse designs, replacing Lambda Architecture's dual-codebase complexity for most use cases.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Consumed by**: [de-pipeline-expert](../agents/de-pipeline-expert.md)

## Architecture Decision Matrix

| Architecture | When to Use |
|-------------|-------------|
| ELT | Cloud warehouses (Snowflake, BigQuery, Redshift) |
| ETL | On-premise, complex pre-aggregation requirements |
| Lambda | Mixed batch + real-time with different SLAs |
| Kappa | Stream-first, simpler codebase preferred |
| Medallion | Lakehouse (Databricks, Delta Lake) |

## Orchestration Patterns

DAG-based (Airflow, Prefect, Dagster): explicit dependencies, retry policies, SLA monitoring, backfill support.

## Relationships

- **Orchestration**: [[skills/dag-orchestration]] for DAG execution patterns
- **Streaming**: [[de-kafka-expert]] for Kappa architecture implementation
- **Batch**: [[de-spark-expert]] for batch processing at scale

## Sources

- `.claude/skills/pipeline-architecture-patterns/SKILL.md`
