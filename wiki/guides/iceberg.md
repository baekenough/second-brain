---
title: "Guide: Apache Iceberg"
type: guide
updated: 2026-04-12
sources:
  - guides/iceberg/README.md
related:
  - "[[de-snowflake-expert]]"
  - "[[de-spark-expert]]"
  - "[[de-pipeline-expert]]"
---

# Guide: Apache Iceberg

Reference documentation for Apache Iceberg — open table format for large-scale analytic datasets with ACID transactions and schema evolution.

## Overview

The Iceberg guide provides reference documentation for open table format usage in data pipelines. Apache Iceberg provides table-format capabilities that enable ACID transactions, time travel, schema evolution, and partition evolution on object storage (S3, GCS, ADLS).

## Key Topics

- **Table Format**: Iceberg's three-layer design (catalog, metadata, data files)
- **ACID Transactions**: Snapshot isolation, optimistic concurrency control
- **Schema Evolution**: Add/drop/rename columns without data rewrite
- **Partition Evolution**: Change partitioning on existing tables without migration
- **Time Travel**: Query historical table snapshots with AS OF syntax
- **Engine Integration**: Spark, Flink, Trino, Snowflake native Iceberg support
- **Catalog**: REST catalog, Hive metastore, Glue catalog options

## Relationships

- **Snowflake**: [[de-snowflake-expert]] for native Iceberg table support in Snowflake
- **Spark**: [[de-spark-expert]] for Spark + Iceberg read/write patterns
- **Architecture**: [[de-pipeline-expert]] for Kafka + Spark + Iceberg pipeline design

## Sources

- `guides/iceberg/README.md`
