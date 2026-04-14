---
title: optimize-report
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/optimize-report/SKILL.md
related:
  - "[[tool-optimizer]]"
  - "[[skills/optimize-analyze]]"
  - "[[skills/optimize-bundle]]"
---

# optimize-report

Generate a comprehensive optimization report with analysis, metrics, baseline comparison, and prioritized recommendations.

## Overview

`optimize-report` (slash command: `/optimize-report`) is the reporting step in the three-skill optimization workflow. It runs full analysis, collects all metrics, optionally compares against a previous baseline, calculates performance scores, and formats the output as text, JSON, or markdown.

The baseline comparison (`--baseline <file>`) is the key feature for tracking improvement over time — it shows delta in bundle size, coverage, and performance scores between runs.

## Key Details

- **Scope**: core | **User-invocable**: true
- **Arguments**: `[--baseline <file>] [--format <format>]`
- Slash command: `/optimize-report`
- Formats: text (default), json, markdown

## Relationships

- **Analysis**: [[skills/optimize-analyze]] provides the raw data
- **Optimization**: [[skills/optimize-bundle]] changes that this report measures
- **Agent**: [[tool-optimizer]] orchestrates all three optimization skills

## Sources

- `.claude/skills/optimize-report/SKILL.md`
