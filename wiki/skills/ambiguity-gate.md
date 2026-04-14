---
title: ambiguity-gate
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/ambiguity-gate/SKILL.md
related:
  - "[[skills/intent-detection]]"
  - "[[r015]]"
  - "[[r003]]"
---

# ambiguity-gate

Pre-routing ambiguity analysis — scores request clarity and asks clarifying questions when confidence is too low to proceed safely.

## Overview

`ambiguity-gate` operates before routing, scoring incoming requests for ambiguity. When clarity is insufficient (below a configurable threshold), it generates targeted clarifying questions rather than proceeding with a potentially wrong interpretation. Inspired by the ouroboros pattern from external skill collections.

## Key Details

- **Scope**: core | **User-invocable**: true
- **Argument**: `[request to analyze for ambiguity]`
- Integrates with routing skills as a pre-flight check

## Relationships

- **Routing complement**: [intent-detection](intent-detection.md) works with this as clarification layer
- **Transparency**: [[r015]] — ambiguity gate results displayed per intent transparency rules
- **Interaction**: [[r003]] — `[Confirm]` format for ambiguous requests aligns with this skill

## Sources

- `.claude/skills/ambiguity-gate/SKILL.md`
