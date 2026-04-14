---
title: npm-audit
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/npm-audit/SKILL.md
related:
  - "[[tool-npm-expert]]"
  - "[[skills/npm-publish]]"
  - "[[skills/npm-version]]"
  - "[[r001]]"
---

# npm-audit

Audit npm dependencies for security vulnerabilities and outdated packages, with optional automatic fixes.

## Overview

`npm-audit` (slash command: `/omcustom:npm-audit`) runs `npm audit` to identify security vulnerabilities, analyzes severity, checks for outdated dependencies, and generates a health report with remediation suggestions. The `--fix` flag applies automatic fixes where safe to do so.

This is a package-scope skill — it operates on npm projects only, not all project types.

## Key Details

- **Scope**: package | **User-invocable**: true
- **Arguments**: `[--fix] [--production] [--json]`
- Slash command: `/omcustom:npm-audit`

## Workflow

1. `npm audit` — security vulnerability scan
2. Severity analysis (critical/high/medium/low)
3. Outdated dependency check
4. Health report generation
5. Remediation suggestions

## Relationships

- **Agent**: [[tool-npm-expert]] for npm package management
- **Publish flow**: [[skills/npm-publish]] includes audit as a pre-publish check
- **Safety**: [[r001]] for external dependency security rules

## Sources

- `.claude/skills/npm-audit/SKILL.md`
