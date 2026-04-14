---
title: redis-best-practices
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/redis-best-practices/SKILL.md
related:
  - "[[db-redis-expert]]"
  - "[[guides/redis]]"
  - "[[db-postgres-expert]]"
---

# redis-best-practices

Redis patterns for caching strategies, data structure selection, key naming, TTL policy, and high availability configuration.

## Overview

`redis-best-practices` codifies the caching strategy decision (Cache-Aside vs Write-Through vs Write-Behind) and the data structure selection rules. Cache-Aside is the default for read-heavy workloads: check cache → miss → read DB → populate cache → serve. Write-Through ensures consistency at the cost of write latency. Write-Behind maximizes write speed with risk of data loss.

Data structure choice is as important as caching strategy: Strings for simple values, Lists for queues/stacks, Sets for unique memberships, Sorted Sets for leaderboards/time-series, Hashes for objects, HyperLogLog for cardinality estimates.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Consumed by**: [db-redis-expert](../agents/db-redis-expert.md)

## Critical Rules

- **TTL on everything**: every key must have an expiry (prevent memory bloat)
- **Key naming**: `{app}:{entity}:{id}` format (e.g., `app:user:12345`)
- **Avoid KEYS \***: use SCAN for production key iteration
- **MULTI/EXEC**: for atomic operations (not a substitute for distributed locks)
- **Lua scripts**: for complex atomic operations
- **Sentinel/Cluster**: for production HA — never single-node

## Caching Strategy Selection

| Strategy | When |
|----------|------|
| Cache-Aside | Read-heavy, occasional stale data acceptable |
| Write-Through | Strong consistency required |
| Write-Behind | Write-heavy, eventual consistency acceptable |

## Relationships

- **Agent**: [[db-redis-expert]] applies these patterns
- **Guide**: [[guides/redis]] for extended reference

## Sources

- `.claude/skills/redis-best-practices/SKILL.md`
