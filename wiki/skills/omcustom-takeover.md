---
title: omcustom-takeover
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/omcustom-takeover/SKILL.md
related:
  - "[[mgr-creator]]"
  - "[[skills/create-agent]]"
  - "[[r006]]"
---

# omcustom-takeover

Reverse-engineer a canonical spec from an existing agent or skill file — extracting intent, invariants, workflow contract, and I/O contract.

## Overview

`omcustom-takeover` (slash command: `/omcustom-takeover`) implements reverse compilation for agent/skill files: when an agent has evolved organically without a formal specification, this skill reads the implementation and derives a structured spec. The output captures intent (why it exists), invariants (what must always be true), workflow contract (steps in order), and I/O contract (inputs, outputs, side effects).

This is useful before running `mgr-updater:docs` or when onboarding a new contributor — the spec becomes the authoritative description rather than the implementation.

## Key Details

- **Scope**: harness | **User-invocable**: true
- **Arguments**: `<agent-name>` or `<skill-name>`
- Slash command: `/omcustom-takeover`

## Output Sections

- **Intent**: Why this agent/skill exists (purpose statement)
- **Invariants**: What must always be true (constraints)
- **Workflow contract**: Ordered steps
- **I/O contract**: Inputs, outputs, side effects

## Relationships

- **Creation**: [[skills/create-agent]] for building new agents from scratch
- **Agent design**: [[r006]] for what a valid spec looks like

## Sources

- `.claude/skills/omcustom-takeover/SKILL.md`
