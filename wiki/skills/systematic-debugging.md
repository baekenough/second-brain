---
title: systematic-debugging
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/systematic-debugging/SKILL.md
related:
  - "[[skills/stuck-recovery]]"
  - "[[skills/deep-verify]]"
  - "[[r020]]"
---

# systematic-debugging

Enforce reproduce-first, root-cause-first, failing-test-first debugging discipline — prevents speculative fixes and "while I'm here" scope creep.

## Overview

`systematic-debugging` (external skill, MIT license from tmdgusya/engineering-disciplines) enforces strict debugging discipline through hard gates. The three core purposes: fix causes not symptoms, prevent guess-based patching, and lock the failure as a test before fixing.

The skill activates on any bug, test failure, or unexpected behavior. Written in Korean for the target audience.

## Key Details

- **Scope**: core | **User-invocable**: false
- **Version**: 1.0.0

## Hard Gates (No Exceptions)

1. No fixes before reproduced/observable state exists
2. No fixes before a causal hypothesis is stated
3. No fixes before a failing test or equivalent reproduction exists
4. One hypothesis per fix attempt
5. No "while I'm here" refactoring during bug fixes
6. After 3 failed fix attempts: suspect structural problem before more patches

## Why Hard Gates

Each gate prevents a class of debugging failure: gate 1 prevents fixing the wrong thing, gate 2 prevents random changes, gate 3 ensures regression prevention, gate 4 prevents compound errors, gate 5 prevents scope creep, gate 6 prevents thrashing.

## Relationships

- **Recovery**: [[skills/stuck-recovery]] for when debugging enters a loop
- **Verification**: [[r020]] for completion verification after fixes are applied

## Sources

- `.claude/skills/systematic-debugging/SKILL.md`
