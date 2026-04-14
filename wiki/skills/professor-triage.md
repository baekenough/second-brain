---
title: professor-triage
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/professor-triage/SKILL.md
related:
  - "[[skills/post-release-followup]]"
  - "[[skills/deep-verify]]"
  - "[[mgr-sauron]]"
  - "[[mgr-gitnerd]]"
---

# professor-triage

Codebase-driven GitHub issue triage — searches relevant code for each issue, assesses blast radius, detects already-resolved issues, and produces prioritized triage reports with automated low-risk actions.

## Overview

`professor-triage` (slash command: `/professor-triage`) takes a set of GitHub issues and analyzes them against the current codebase. For each issue: it searches relevant code, determines if the issue is already resolved, estimates impact and blast radius, assigns priority (P0-P3) and size (XS-XL), and executes low-risk triage actions automatically (labeling, closing resolved issues).

The skill uses `context: fork` for isolated execution and runs at `effort: high`.

## Key Details

- **Scope**: harness | **User-invocable**: true | **Effort**: high | **Context**: fork
- **Arguments**: `[issue-numbers...] [--label <label>] [--state <state>] [--since <date>]`
- Slash command: `/professor-triage`

## Workflow Phases

1. Gather: fetch issues via `gh issue list` with filters
2. Analyze: search codebase for each issue (parallel)
3. Triage: priority, size, blast radius, resolution status
4. Act: apply labels, close resolved, register follow-ups
5. Report: cross-analysis markdown output

## Relationships

- **Follow-up**: [[skills/post-release-followup]] processes deferred triage items
- **Verification**: [[skills/deep-verify]] for technical finding analysis

## Sources

- `.claude/skills/professor-triage/SKILL.md`
