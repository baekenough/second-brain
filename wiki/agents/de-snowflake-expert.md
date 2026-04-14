---
title: de-snowflake-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/de-snowflake-expert.md
related:
  - "[[de-dbt-expert]]"
  - "[[de-pipeline-expert]]"
  - "[[skills/snowflake-best-practices]]"
---

# de-snowflake-expert

Snowflake cloud data warehouse developer for query optimization, clustering, data loading, and Iceberg table integration.

## Overview

`de-snowflake-expert` specializes in Snowflake's unique architecture — virtual warehouses, micro-partition pruning with clustering keys, zero-copy cloning, and result set caching. It bridges the gap between SQL querying and cost management, since Snowflake's credit model makes inefficient queries expensive at scale.

The agent supports native Iceberg table integration, making it the bridge between Snowflake's managed format and open table format ecosystems.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: project
- **Domain**: data-engineering | **Skill**: snowflake-best-practices
- **Guides**: `guides/snowflake/`, `guides/iceberg/`

### Capabilities

- Virtual warehouse sizing, auto-scaling, multi-cluster config
- Query optimization: clustering keys, micro-partition pruning, search optimization
- Data loading: COPY INTO, Snowpipe, external stages
- Result caching and materialized views
- Zero-copy cloning for dev/test environments
- Native Iceberg table support (open table format)
- Credit monitoring and cost governance

## Relationships

- **Transformation layer**: [[de-dbt-expert]] for Snowflake-targeted dbt models
- **Architecture**: [[de-pipeline-expert]] for Airflow + dbt + Snowflake pipeline patterns
- **Open format**: Iceberg guide for `guides/iceberg/` open table format context

## Sources

- `.claude/agents/de-snowflake-expert.md`
