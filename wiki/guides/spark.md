---
title: "Guide: Apache Spark"
type: guide
updated: 2026-04-12
sources:
  - guides/spark/README.md
related:
  - "[[de-spark-expert]]"
  - "[[de-kafka-expert]]"
  - "[[de-pipeline-expert]]"
  - "[[skills/spark-best-practices]]"
---

# Guide: Apache Spark

Reference documentation for Apache Spark — DataFrame API, query optimization, Structured Streaming, and resource management.

## Overview

The Spark guide provides reference documentation for `de-spark-expert` and the `spark-best-practices` skill. It covers PySpark and Scala Spark for distributed data processing, with emphasis on query plan optimization and production deployment patterns.

## Key Topics

- **DataFrame API**: Column operations, `select`/`filter`/`groupBy`/`join`, avoiding UDF anti-patterns
- **Query Optimization**: Broadcast joins (`broadcast()` hint), AQE (Adaptive Query Execution), predicate pushdown
- **Partitioning**: Repartition vs coalesce, bucketing for join optimization, partition pruning
- **Structured Streaming**: Sources (Kafka, files), sinks, watermarks, trigger types, checkpointing
- **Storage Formats**: Parquet (columnar), ORC, Delta Lake (ACID), Apache Iceberg (open format)
- **Resource Management**: Executor memory/cores, dynamic allocation, off-heap memory, Spark UI profiling
- **Spark SQL**: Temp views, catalog API, SQL hint syntax

## Relationships

- **Agent**: [[de-spark-expert]] primary consumer
- **Skill**: [[skills/spark-best-practices]] implements patterns
- **Streaming source**: [[de-kafka-expert]] for Kafka → Spark pipelines
- **Architecture**: [[de-pipeline-expert]] for Spark + Iceberg + Kafka design

## Sources

- `guides/spark/README.md`
