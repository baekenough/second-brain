---
title: reasoning-sandwich
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/reasoning-sandwich/SKILL.md
related:
  - "[[skills/evaluator-optimizer]]"
  - "[[skills/multi-model-verification]]"
  - "[[r009]]"
---

# reasoning-sandwich

Model allocation pattern that wraps implementation with stronger-model pre-reasoning and post-verification phases.

## Overview

`reasoning-sandwich` defines a three-phase execution pattern for complex tasks where the implementation quality depends on correct problem framing and thorough verification. The "sandwich" positions an opus-level reasoning phase before the action and a verification phase after.

The pattern's core insight: implementation quality is bounded by analysis quality. Using sonnet to implement a problem that opus would frame differently wastes both the implementation and the review.

## Key Details

- **Scope**: core | **User-invocable**: false

## Model Allocation

| Phase | Model | Role |
|-------|-------|------|
| Pre-reasoning | opus | Analyze requirements, identify edge cases, define success criteria |
| Action | sonnet | Implement, generate code, execute plan |
| Post-verification | sonnet or haiku | Verify against criteria, check regressions |

## When to Apply

Apply for complex implementation tasks, architecture decisions, and any action where edge-case misses would be costly to fix later. Not needed for simple, well-defined tasks.

## Relationships

- **Evaluator model**: [[skills/evaluator-optimizer]] uses this pattern for evaluator model selection (opus for evaluation)
- **Multi-model**: [[skills/multi-model-verification]] extends this concept to three simultaneous reviewers

## Sources

- `.claude/skills/reasoning-sandwich/SKILL.md`
