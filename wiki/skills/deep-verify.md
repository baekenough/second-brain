---
title: deep-verify
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/deep-verify/SKILL.md
related:
  - "[[skills/deep-plan]]"
  - "[[skills/multi-model-verification]]"
  - "[[r020]]"
---

# deep-verify

Multi-angle release quality verification using parallel expert review teams.

## Overview

`deep-verify` runs parallel expert reviews from multiple angles (security, performance, correctness, UX, API design) and aggregates findings into a structured quality report. Version 1.1.0. Used for pre-release quality gates or verifying complex implementation plans.

## Key Details

- **Scope**: core | **User-invocable**: true | **Effort**: high | **Version**: 1.1.0

## Relationships

- **Planning**: [deep-plan](deep-plan.md) uses this for plan verification
- **Multi-model**: [multi-model-verification](multi-model-verification.md) for model-diverse verification
- **Completion**: [[r020]] — verification results feed completion determination

## Sources

- `.claude/skills/deep-verify/SKILL.md`
