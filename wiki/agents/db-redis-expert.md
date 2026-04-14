---
title: db-redis-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/db-redis-expert.md
related:
  - "[[db-postgres-expert]]"
  - "[[de-kafka-expert]]"
  - "[[skills/redis-best-practices]]"
  - "[[be-fastapi-expert]]"
---

# db-redis-expert

Redis expert for caching strategies, data structure design, Pub/Sub messaging, Streams, Lua scripting, and cluster HA management.

## Overview

`db-redis-expert` covers all of Redis's capabilities as both a cache and a data structure server. Its design philosophy is that Redis misuse (wrong data structure, missing expiry, no eviction policy) causes more production issues than Redis itself — so the agent emphasizes *selection* of the right structure and pattern before implementation.

Memory is user-scoped because Redis patterns (caching strategies, eviction policies) apply broadly across projects.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: user
- **Domain**: backend | **Skill**: redis-best-practices | **Guide**: `guides/redis/`

### Capability Areas

- **Caching**: cache-aside, write-through, write-behind, invalidation strategies
- **Data Structures**: String, Hash, List, Set, Sorted Set, Stream, HyperLogLog, Bitmap — proper selection
- **Messaging**: Pub/Sub for fire-and-forget, Redis Streams for durable event log
- **Atomicity**: Lua scripting for multi-command atomic operations
- **HA**: Cluster and Sentinel configuration
- **Memory**: Eviction policies (LRU, LFU, TTL), RDB/AOF persistence tuning

## Relationships

- **Complementary DB**: [[db-postgres-expert]] for primary data store
- **Messaging alternative**: [[de-kafka-expert]] for durable event streaming at scale
- **Application layer**: [[be-fastapi-expert]] for async Redis client integration

## Sources

- `.claude/agents/db-redis-expert.md`
