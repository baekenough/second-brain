---
title: spark-best-practices
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/spark-best-practices/SKILL.md
related:
  - "[[de-spark-expert]]"
  - "[[guides/spark]]"
  - "[[de-pipeline-expert]]"
  - "[[skills/pipeline-architecture-patterns]]"
---

# spark-best-practices

Apache Spark patterns for PySpark/Scala: broadcast joins, shuffle minimization, caching strategy, executor tuning, and Delta Lake integration.

## Overview

`spark-best-practices` addresses the two most expensive Spark operations: shuffles and the wrong join strategy. Broadcast joins (`broadcast(small_df)`) eliminate shuffle for small-large table joins — critical for performance when the small table fits in executor memory (<100MB). Minimize shuffles with `coalesce()` (no shuffle) vs `repartition()` (forces shuffle, use sparingly).

## Key Details

- **Scope**: core | **User-invocable**: false
- **Consumed by**: [de-spark-expert](../agents/de-spark-expert.md)

## Critical Rules

- **Broadcast joins**: use when small table < 100MB (default auto-threshold: 10MB)
- **Shuffle minimization**: filter before joins; use `coalesce()` not `repartition()` to reduce partitions
- **Caching**: `df.cache()` only for DataFrames used multiple times; always `df.unpersist()` when done
- **Predicate pushdown**: filter early, before joins
- **Avoid UDFs**: prefer Spark SQL/built-in functions (JVM optimization); if needed, use Pandas UDFs for vectorization
- **Partition size**: target 100-200MB per partition; avoid <10MB (overhead) or >1GB (OOM)

## Executor Config

`spark.executor.memory`, `spark.executor.cores`, `spark.default.parallelism` must be tuned per cluster size. Rule: 2-5 cores per executor, memory = cores * data_per_core + 40% overhead.

## Relationships

- **Agent**: [[de-spark-expert]] applies these patterns
- **Pipeline**: [[de-pipeline-expert]] for Spark within data pipelines

## Sources

- `.claude/skills/spark-best-practices/SKILL.md`
