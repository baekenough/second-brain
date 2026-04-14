---
title: action-validator
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/action-validator/SKILL.md
related:
  - "[[r002]]"
  - "[[r021]]"
  - "[[r010]]"
---

# action-validator

Pre-action boundary checker that validates agent tool calls against declared capabilities and task contracts — advisory only, never blocks.

## Overview

`action-validator` is an advisory pre-action validation layer inspired by AutoHarness (Google DeepMind). It checks whether agent tool calls fall within their declared capabilities, file access scope (R002), and task contracts before execution. Per R021's advisory-first model, it emits warnings but does NOT block actions.

## Key Details

- **Scope**: core | **User-invocable**: false
- Checks: declared capabilities vs actual tool calls, file access scope per R002, task contract boundaries
- Never blocks (R021 advisory-first)

## Relationships

- **Permission rules**: [[r002]] defines file access scope checked by this skill
- **Enforcement policy**: [[r021]] — advisory tier, not hard block
- **Delegation**: [[r010]] — orchestrator uses this to validate subagent actions

## Sources

- `.claude/skills/action-validator/SKILL.md`
