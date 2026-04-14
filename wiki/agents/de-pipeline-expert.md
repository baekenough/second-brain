---
title: de-pipeline-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/de-pipeline-expert.md
related:
  - "[[de-airflow-expert]]"
  - "[[de-dbt-expert]]"
  - "[[de-kafka-expert]]"
  - "[[de-spark-expert]]"
  - "[[de-snowflake-expert]]"
  - "[[skills/pipeline-architecture-patterns]]"
---

# de-pipeline-expert

Data pipeline architect for ETL/ELT design, orchestration patterns, data quality frameworks, and cross-tool integration across the modern data stack.

## Overview

`de-pipeline-expert` operates at the architecture level — deciding *how* pipelines should be structured rather than implementing specific tool features. It makes the critical early decisions: ETL vs ELT, batch vs streaming, lambda vs kappa architecture, data quality contracts.

This agent is the coordinator for multi-tool pipeline design, understanding how Airflow, dbt, Snowflake, Kafka, Spark, and Iceberg work together as a system rather than in isolation.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: project
- **Domain**: data-engineering | **Skill**: pipeline-architecture-patterns
- **Guides**: `guides/airflow/`, `guides/dbt/`, `guides/spark/`, `guides/kafka/`, `guides/snowflake/`, `guides/iceberg/`

### Architecture Decisions

- ETL vs ELT selection based on compute and storage trade-offs
- Batch, streaming, and hybrid (lambda/kappa) system design
- Data quality frameworks and contract enforcement
- Data lineage and metadata management
- Cross-tool cost optimization

## Relationships

- **Orchestration tool**: [[de-airflow-expert]] for DAG implementation
- **Transformation**: [[de-dbt-expert]] for SQL modeling layer
- **Streaming**: [[de-kafka-expert]] for event-driven pipeline design
- **Compute**: [[de-spark-expert]] for distributed processing
- **Warehouse**: [[de-snowflake-expert]] for cloud data warehouse integration

## Sources

- `.claude/agents/de-pipeline-expert.md`
