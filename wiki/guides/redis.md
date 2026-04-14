---
title: "Guide: Redis"
type: guide
updated: 2026-04-12
sources:
  - guides/redis/README.md
related:
  - "[[db-redis-expert]]"
  - "[[de-kafka-expert]]"
  - "[[skills/redis-best-practices]]"
---

# Guide: Redis

Reference documentation for Redis — data structure selection, caching strategies, Pub/Sub vs Streams, Lua scripting, and cluster HA.

## Overview

The Redis guide provides reference documentation for `db-redis-expert` and the `redis-best-practices` skill. It covers Redis command patterns, data structure trade-offs, messaging design, and operational concerns for production Redis deployments.

## Key Topics

- **Data Structures**: String (KV, counters), Hash (objects), List (queues/stacks), Set (unique members), Sorted Set (leaderboards), Stream (durable log), HyperLogLog (cardinality), Bitmap
- **Caching Strategies**: Cache-aside, write-through, write-behind, read-through — trade-offs
- **Eviction Policies**: LRU, LFU, volatile-ttl, allkeys-lru — selection criteria
- **Pub/Sub vs Streams**: Fire-and-forget (Pub/Sub) vs durable log with consumer groups (Streams)
- **Lua Scripting**: Atomic multi-command operations with `EVAL`/`EVALSHA`
- **Persistence**: RDB snapshots, AOF logging, hybrid persistence trade-offs
- **HA**: Sentinel (automatic failover), Cluster (horizontal sharding), slot distribution

## Relationships

- **Agent**: [[db-redis-expert]] primary consumer
- **Skill**: [[skills/redis-best-practices]] implements patterns
- **Messaging at scale**: [[de-kafka-expert]] for durable event streaming beyond Redis

## Sources

- `guides/redis/README.md`
