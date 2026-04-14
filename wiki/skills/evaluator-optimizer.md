---
title: evaluator-optimizer
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/evaluator-optimizer/SKILL.md
related:
  - "[[skills/worker-reviewer-pipeline]]"
  - "[[skills/reasoning-sandwich]]"
  - "[[skills/harness-eval]]"
  - "[[qa-planner]]"
  - "[[qa-writer]]"
  - "[[qa-engineer]]"
  - "[[arch-documenter]]"
---

# evaluator-optimizer

Parameterized generator-evaluator loop for iterative quality refinement with configurable rubrics, quality gates, and anti-leniency measures.

## Overview

`evaluator-optimizer` generalizes the worker-reviewer pattern to any quality-critical domain: code, documentation, architecture decisions, test plans, UI generation. A generator agent produces output; an evaluator scores it against a rubric; the loop continues until the quality gate passes or max iterations are reached (hard cap: 5).

The skill addresses evaluator leniency — LLMs default to generous scoring. Counter-measures include skepticism prompting, anti-self-praise bias instructions (when generator and evaluator share the same model family), and per-criterion `fail_example` anchors that reduce score inflation by ~20%.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Generator model**: sonnet (default) | **Evaluator model**: opus (default)
- **Max iterations**: 3 default, hard cap 5

## Quality Gate Types

| Type | Behavior |
|------|----------|
| `all_pass` | Every criterion must pass |
| `majority_pass` | >50% of weighted criteria pass |
| `score_threshold` | Weighted average >= threshold |

## Advanced Options

- **Pre-negotiation**: Generator and evaluator align on rubric interpretation before iteration 1, reducing wasted loops
- **Conditional evaluator**: Skip evaluation for low-complexity tasks (saves ~40% tokens)
- **Ecomode**: Compressed output — `[EO] iter 2/3 → 0.85 → PASS`

## Domain Applications

Covers code review, documentation, architecture review, test plans, test coverage optimization, and UI generation (anti-AI-slop rubric with originality > craft > functionality weighting).

## Relationships

- **Related pattern**: [[skills/worker-reviewer-pipeline]] for simpler code review cycles
- **Model guidance**: [[skills/reasoning-sandwich]] for generator/evaluator model selection
- **Harness integration**: [[skills/harness-eval]] preset rubric for SE benchmarks

## Sources

- `.claude/skills/evaluator-optimizer/SKILL.md`
