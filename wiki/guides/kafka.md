---
title: "Guide: Apache Kafka"
type: guide
updated: 2026-04-12
sources:
  - guides/kafka/README.md
related:
  - "[[de-kafka-expert]]"
  - "[[de-spark-expert]]"
  - "[[de-pipeline-expert]]"
  - "[[skills/kafka-best-practices]]"
---

# Guide: Apache Kafka

Reference documentation for Apache Kafka — producer/consumer patterns, topic design, Schema Registry, Kafka Streams, and exactly-once semantics.

## Overview

The Kafka guide provides reference documentation for `de-kafka-expert` and the `kafka-best-practices` skill. It covers Kafka's producer-consumer semantics, schema management, and streaming application patterns with a focus on correctness and reliability.

## Key Topics

- **Producer Patterns**: Idempotent producer (`enable.idempotence=true`), transactional API, batching (`linger.ms`, `batch.size`)
- **Consumer Patterns**: Consumer group coordination, cooperative sticky assignor, manual vs auto commit, exactly-once processing
- **Topic Design**: Partition sizing for throughput, replication factor, retention policies, log compaction
- **Schema Registry**: Avro/Protobuf/JSON Schema, subject naming strategies, compatibility modes (BACKWARD/FORWARD/FULL)
- **Kafka Streams**: Topology design, state stores, interactive queries, windowed aggregations
- **Kafka Connect**: Source/sink connectors, Single Message Transforms (SMTs)

## Relationships

- **Agent**: [[de-kafka-expert]] primary consumer
- **Skill**: [[skills/kafka-best-practices]] implements patterns
- **Stream processing**: [[de-spark-expert]] for Spark Structured Streaming on Kafka
- **Architecture**: [[de-pipeline-expert]] for Kafka + Spark + Iceberg design

## Sources

- `guides/kafka/README.md`
