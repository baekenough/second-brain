---
title: kafka-best-practices
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/kafka-best-practices/SKILL.md
related:
  - "[[de-kafka-expert]]"
  - "[[guides/kafka]]"
  - "[[de-pipeline-expert]]"
  - "[[skills/pipeline]]"
---

# kafka-best-practices

Apache Kafka patterns for reliable event streaming: idempotent producers, offset management, topic design, and exactly-once semantics.

## Overview

`kafka-best-practices` codifies Kafka's reliability guarantees and their configuration requirements. The most critical rule: enable idempotent producers (`enable.idempotence=true`) — this requires `acks=all`, `retries > 0`, and `max.in.flight.requests.per.connection <= 5` to work correctly. Exactly-once end-to-end processing requires the transactional API (`initTransactions`, `beginTransaction`, `commitTransaction`).

Consumer offset management strategy determines delivery semantics: auto-commit gives at-least-once; manual `commitSync`/`commitAsync` after processing gives controlled at-least-once with lower risk of loss.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Consumed by**: [de-kafka-expert](../agents/de-kafka-expert.md)

## Critical Producer Config

```
enable.idempotence=true   (prevents duplicates)
acks=all                  (required for idempotence)
retries > 0               (required for idempotence)
max.in.flight.requests.per.connection <= 5
```

## Topic Design Rules

- Partition count: set for target throughput, hard to change later
- Replication factor: 3 for production (min.insync.replicas=2)
- Compact topics for changelog/event-sourcing patterns
- Avoid single-partition topics for high-throughput streams

## Consumer Groups

One group per independent processing pipeline. Consumer count ≤ partition count (excess consumers are idle). Rebalance handling: implement `ConsumerRebalanceListener` to commit offsets before partition revocation.

## Relationships

- **Agent**: [[de-kafka-expert]] applies these patterns
- **Pipeline**: [[de-pipeline-expert]] for Kafka within data pipelines
- **Guide**: [[guides/kafka]] for extended reference

## Sources

- `.claude/skills/kafka-best-practices/SKILL.md`
