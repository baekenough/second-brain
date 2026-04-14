---
title: de-spark-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/de-spark-expert.md
related:
  - "[[de-kafka-expert]]"
  - "[[de-pipeline-expert]]"
  - "[[de-snowflake-expert]]"
  - "[[skills/spark-best-practices]]"
---

# de-spark-expert

Apache Spark developer for PySpark and Scala distributed data processing, optimization, and Structured Streaming applications.

## Overview

`de-spark-expert` handles large-scale distributed data processing with Spark. It focuses on performance optimization — broadcast joins, partition strategies, bucketing, resource tuning — because correct Spark code that ignores execution plans is often 10x-100x slower than necessary.

The agent covers both batch processing (DataFrame/Dataset APIs) and real-time processing (Structured Streaming), plus modern storage format integration (Parquet, Delta, Iceberg).

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: project
- **Domain**: data-engineering | **Skill**: spark-best-practices | **Guide**: `guides/spark/`

### Capabilities

- DataFrame and Dataset APIs, Spark SQL
- Broadcast joins and optimizer hint strategies
- Partitioning and bucketing for shuffle optimization
- Structured Streaming: sources, sinks, watermarks, triggers
- Resource management: executor/driver memory, dynamic allocation
- Storage formats: Parquet, ORC, Delta Lake, Apache Iceberg
- Spark UI profiling and bottleneck identification

## Relationships

- **Streaming source**: [[de-kafka-expert]] for Kafka → Spark Structured Streaming pipelines
- **Architecture**: [[de-pipeline-expert]] for Kafka + Spark + Iceberg system design
- **Warehouse sink**: [[de-snowflake-expert]] for Spark → Snowflake loading patterns

## Sources

- `.claude/agents/de-spark-expert.md`
