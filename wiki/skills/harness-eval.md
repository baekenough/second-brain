---
title: harness-eval
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/harness-eval/SKILL.md
related:
  - "[[skills/evaluator-optimizer]]"
  - "[[skills/structured-dev-cycle]]"
  - "[[mgr-sauron]]"
  - "[[qa-engineer]]"
---

# harness-eval

Structured software engineering benchmark: 15 task definitions scored across Test Coverage (30%), Architecture Design (25%), Error Handling (25%), and Extensibility (20%).

## Overview

`harness-eval` provides a quantitative quality measurement system derived from the claude-code-harness research, which demonstrated 60% improvement (49.5 → 79.3 points) through structured pre-configuration. Agents are evaluated against 15 canonical SE task categories with weighted scoring rubrics.

The skill serves two purposes: standalone benchmarking (run the 15 tasks and score results) and as a rubric preset for [[skills/evaluator-optimizer]] (the harness dimensions become the sprint contract criteria).

## Key Details

- **Scope**: harness | **User-invocable**: true | **Effort**: high
- **Arguments**: `[--preset all|quick] [--task task-name]`
- Slash command: `/omcustom:harness-eval`

## Quality Dimensions

| Dimension | Weight |
|-----------|--------|
| Test Coverage | 30% |
| Architecture Design | 25% |
| Error Handling | 25% |
| Extensibility | 20% |

## 15 Benchmark Tasks

API Design, Data Modeling, Authentication Flow, Test Suite Creation, Error Handler, Logging System, Configuration Manager, CLI Tool, Database Migration, Cache Layer, Queue Consumer, Middleware Chain, File Processor, + 2 more.

Quick preset: top 5 high-impact benchmarks only.

## Relationships

- **Rubric integration**: [[skills/evaluator-optimizer]] can use harness dimensions as its sprint contract criteria
- **Verification**: [[mgr-sauron]] uses harness scoring as one quality signal

## Sources

- `.claude/skills/harness-eval/SKILL.md`
