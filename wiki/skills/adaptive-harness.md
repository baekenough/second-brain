---
title: adaptive-harness
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/adaptive-harness/SKILL.md
related:
  - "[[r016]]"
  - "[[mgr-creator]]"
  - "[[mgr-sauron]]"
---

# adaptive-harness

Auto-detects project context and optimizes the oh-my-customcode harness — deactivates unused agents/skills, suggests missing experts, and generates a persistent project profile.

## Overview

`adaptive-harness` is a harness-scope skill that scans the current project's tech stack and compares it against installed agents/skills to identify gaps and unused components. It generates a project profile that drives future agent activation decisions and records learned patterns over time. The `--learn` flag specifically records failure patterns from R016 violation data.

## Key Details

- **Scope**: harness | **User-invocable**: true | **Version**: 1.0.0 | **Effort**: high
- **Commands**: `--optimize`, `--scan`, `--learn`, `--export`, `--import`, `--dry-run`

## Relationships

- **Improvement loop**: [[r016]] — `--learn` flag records violation patterns
- **Agent creation**: [[mgr-creator]] uses profile for dynamic agent creation
- **Verification**: [[mgr-sauron]] can leverage profile for faster verification

## Sources

- `.claude/skills/adaptive-harness/SKILL.md`
