---
title: de-lead-routing
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/de-lead-routing/SKILL.md
related:
  - "[[de-airflow-expert]]"
  - "[[de-dbt-expert]]"
  - "[[de-kafka-expert]]"
  - "[[de-pipeline-expert]]"
  - "[[de-snowflake-expert]]"
  - "[[de-spark-expert]]"
  - "[[r015]]"
  - "[[r019]]"
---

# de-lead-routing

Routes data engineering tasks to the correct DE expert agent — Airflow, dbt, Kafka, Spark, Snowflake, or pipeline architecture.

## Overview

`de-lead-routing` is one of the four core routing skills. It analyzes user requests for data engineering intent (keywords, file patterns like `*.py dags/`, `models/*.sql`) and routes to the most appropriate DE expert. Uses `context: fork` for isolated routing execution.

## Key Details

- **Scope**: core | **User-invocable**: false | **context**: fork

## Routing Targets

[[de-airflow-expert]], [[de-dbt-expert]], [[de-kafka-expert]], [[de-spark-expert]], [[de-snowflake-expert]], [[de-pipeline-expert]]

## Relationships

- **Transparency**: [[r015]] — routing decisions displayed per intent transparency
- **Enrichment**: [[r019]] — ontology-RAG enriches skill suggestions for routed agent
- **Orchestration**: Invoked by main conversation as routing layer

## Sources

- `.claude/skills/de-lead-routing/SKILL.md`
