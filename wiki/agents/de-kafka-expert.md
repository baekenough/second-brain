---
title: de-kafka-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/de-kafka-expert.md
related:
  - "[[de-spark-expert]]"
  - "[[de-pipeline-expert]]"
  - "[[db-redis-expert]]"
  - "[[skills/kafka-best-practices]]"
---

# de-kafka-expert

Apache Kafka expert for event streaming architectures — topic design, idempotent producers, consumer groups, Kafka Streams, Schema Registry, and CQRS patterns.

## Overview

`de-kafka-expert` covers the full Kafka ecosystem for building reliable, high-throughput event streaming systems. It prioritizes correctness — idempotent producers with exactly-once semantics, careful offset management, cooperative rebalancing — over simple fire-and-forget patterns that cause duplicates or data loss.

The agent has unusually detailed coverage of producer/consumer failure modes, which are the most common source of data integrity issues in Kafka systems.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: project
- **Domain**: data-engineering | **Skill**: kafka-best-practices | **Guide**: `guides/kafka/`

### Priority Areas

- **CRITICAL**: Idempotent producers, consumer group management, exactly-once semantics
- **HIGH**: Topic design (partition sizing), schema evolution with Schema Registry
- **MEDIUM**: Kafka Streams topology, Kafka Connect, SMTs

### Key Patterns

- `enable.idempotence=true` + transactional API for EOS
- Cooperative sticky assignor for minimal consumer rebalancing
- Avro/Protobuf schema with compatibility modes (BACKWARD/FORWARD/FULL)
- Log compaction for changelog topics

## Relationships

- **Stream processing**: [[de-spark-expert]] for large-scale Spark Structured Streaming on Kafka
- **Architecture**: [[de-pipeline-expert]] for Kafka + Spark + Iceberg pipeline design
- **Lightweight messaging**: [[db-redis-expert]] for simpler Pub/Sub use cases

## Sources

- `.claude/agents/de-kafka-expert.md`
