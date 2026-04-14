---
title: worker-reviewer-pipeline
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/worker-reviewer-pipeline/SKILL.md
related:
  - "[[skills/evaluator-optimizer]]"
  - "[[skills/pipeline-guards]]"
  - "[[r010]]"
  - "[[r009]]"
---

# worker-reviewer-pipeline

Iterative Worker → Reviewer pipeline for quality-critical code — one agent implements, another reviews, cycles repeat until quality criteria pass or max iterations are reached.

## Overview

`worker-reviewer-pipeline` is the code-focused iterative refinement pattern. A Worker agent implements; a Reviewer agent evaluates against quality criteria (correctness, security, performance). The pipeline repeats until criteria pass or the max iteration limit (from [[skills/pipeline-guards]]) is reached.

Orchestrator-only (R010): only the main conversation activates this pipeline; Worker and Reviewer are subagents. Uses `context: fork` for isolated execution.

## Key Details

- **Scope**: core | **User-invocable**: false | **Context**: fork

## Activation Conditions

| Condition | Activate? |
|-----------|----------|
| Quality-critical code (auth, security, payments) | Yes |
| Complex refactoring (5+ files) | Yes |
| User explicitly requests review cycle | Yes |
| Simple config or doc changes | No |

## Pipeline Config

```yaml
worker:
  agent: lang-typescript-expert
  model: sonnet
reviewer:
  agent: lang-typescript-expert
  model: opus
criteria:
  - correctness
  - security
  - performance
max_iterations: 3
```

## Relationships

- **Generalization**: [[skills/evaluator-optimizer]] extends this pattern to any domain
- **Limits**: [[skills/pipeline-guards]] enforces max_iterations cap
- **Delegation**: [[r010]] — orchestrator-only activation

## Sources

- `.claude/skills/worker-reviewer-pipeline/SKILL.md`
