---
title: snowflake-best-practices
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/snowflake-best-practices/SKILL.md
related:
  - "[[de-snowflake-expert]]"
  - "[[guides/snowflake]]"
  - "[[skills/dbt-best-practices]]"
  - "[[de-dbt-expert]]"
---

# snowflake-best-practices

Snowflake cloud data warehouse patterns: warehouse sizing, query optimization via clustering keys, cost management, data sharing, and Snowpark.

## Overview

`snowflake-best-practices` encodes Snowflake-specific patterns that don't apply to general SQL or other warehouse systems. Warehouse sizing starts at XS/S with auto-suspend (1 minute idle) — scaling up costs money immediately and scaling down takes minutes. Clustering keys enable micro-partition pruning (the primary Snowflake query acceleration mechanism). Separate warehouses per workload prevent query queue contention.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Consumed by**: [de-snowflake-expert](../agents/de-snowflake-expert.md)

## Critical Rules

- **Auto-suspend**: always enable (1 minute idle prevents cost waste)
- **Clustering keys**: for frequently filtered columns in large tables
- **Separate warehouses**: ELT, BI, ad-hoc — never share one warehouse
- **`COPY INTO`**: for bulk loading (not INSERT for large batches)
- **Time Travel**: use for data recovery, not as a backup strategy
- **Zero-Copy Cloning**: for dev/test environments (shares storage)

## Cost Control

`SHOW WAREHOUSES` to monitor usage. `ACCOUNT_USAGE.WAREHOUSE_METERING_HISTORY` for cost analysis. Resource monitors for credit alerts.

## Relationships

- **dbt integration**: [[skills/dbt-best-practices]] for transformation layer
- **Guide**: [[guides/snowflake]] for extended reference

## Sources

- `.claude/skills/snowflake-best-practices/SKILL.md`
