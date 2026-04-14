---
title: "Guide: Snowflake"
type: guide
updated: 2026-04-12
sources:
  - guides/snowflake/README.md
related:
  - "[[de-snowflake-expert]]"
  - "[[de-dbt-expert]]"
  - "[[skills/snowflake-best-practices]]"
---

# Guide: Snowflake

Reference documentation for Snowflake cloud data warehouse — query optimization, warehouse sizing, data loading, and cost governance.

## Overview

The Snowflake guide provides reference documentation for `de-snowflake-expert` and the `snowflake-best-practices` skill. It covers Snowflake's unique architecture characteristics — virtual warehouses, micro-partition pruning, result caching — and how to design queries and schemas that leverage them.

## Key Topics

- **Virtual Warehouses**: Sizing (XS to 6XL), auto-suspend/resume, multi-cluster warehouses
- **Query Optimization**: Clustering keys, micro-partition pruning, search optimization service
- **Result Caching**: Query result cache (24h), local disk cache, remote disk cache
- **Data Loading**: COPY INTO from stages, Snowpipe for continuous loading, external stages
- **Materialized Views**: When to use vs regular views, refresh strategies
- **Zero-Copy Cloning**: Dev/test environment creation, data sharing
- **Iceberg Tables**: Native open table format support
- **Cost Governance**: Credit monitoring, query profiling, warehouse attribution

## Relationships

- **Agent**: [[de-snowflake-expert]] primary consumer
- **dbt**: [[de-dbt-expert]] for Snowflake-targeted dbt models
- **Skill**: [[skills/snowflake-best-practices]] implements patterns

## Sources

- `guides/snowflake/README.md`
