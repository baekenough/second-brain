---
title: multi-model-verification
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/multi-model-verification/SKILL.md
related:
  - "[[skills/evaluator-optimizer]]"
  - "[[skills/worker-reviewer-pipeline]]"
  - "[[r018]]"
  - "[[r009]]"
  - "[[sec-codeql-expert]]"
---

# multi-model-verification

Parallel code verification using three Claude model tiers simultaneously — each focused on a different quality dimension — with severity classification.

## Overview

`multi-model-verification` runs three reviewers in parallel (requires Agent Teams when enabled per R018): `opus` for architecture/design/security, `sonnet` for logic/error handling/edge cases, and `haiku` for style/conventions/docs. Results are aggregated and classified by severity: CRITICAL (must fix), WARNING (should fix), INFO (optional).

This pattern is inspired by Pi Coding Agent's multi-model verification workflow. The three-reviewer split reflects model cost-capability tradeoffs: opus for deep reasoning, haiku for fast pattern checks.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Requires**: Agent Teams for parallel execution (falls back to sequential)

## Review Roles

| Model | Role | Focus |
|-------|------|-------|
| `opus` | Architecture Reviewer | Design, separation, security architecture |
| `sonnet` | Code Quality Reviewer | Logic, error handling, edge cases |
| `haiku` | Style Reviewer | Naming, formatting, docs |

## Severity Levels

| Level | Meaning | Action |
|-------|---------|--------|
| CRITICAL | Bug/security/data loss | Must fix before merge |
| WARNING | Code smell/missing handling | Should fix |
| INFO | Style suggestion | Optional |

## Relationships

- **Agent Teams**: [[r018]] for parallel reviewer spawning
- **Alternative**: [[skills/evaluator-optimizer]] for single-model iterative refinement
- **Security**: [[sec-codeql-expert]] for deep security audit

## Sources

- `.claude/skills/multi-model-verification/SKILL.md`
