---
title: post-release-followup
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/post-release-followup/SKILL.md
related:
  - "[[skills/professor-triage]]"
  - "[[skills/deep-verify]]"
  - "[[mgr-gitnerd]]"
---

# post-release-followup

After PR creation in the auto-dev release workflow, collects unaddressed findings and presents follow-up actions — execute now, register as issues, or skip.

## Overview

`post-release-followup` is the cleanup step in the release pipeline. It gathers three categories of unfinished work: remaining open GitHub issues with `verify-done` label (not included in the release), MEDIUM/LOW severity findings from the latest `deep-verify` run, and deferred items from `professor-triage`. The user then decides what to do with each finding.

This skill is not user-invocable — it is called automatically within the release pipeline after PR creation.

## Key Details

- **Scope**: harness | **User-invocable**: false | **Effort**: medium

## Follow-up Sources

| Source | Data |
|--------|------|
| Open GitHub issues | Triaged but not in release (`verify-done` label) |
| deep-verify findings | MEDIUM/LOW severity unfixed |
| professor-triage deferred | Items deferred during triage |

## Decision Options

For each finding: execute now, register as GitHub issue, or skip.

## Relationships

- **Triage**: [[skills/professor-triage]] produces deferred items
- **Verification**: [[skills/deep-verify]] produces unfixed findings

## Sources

- `.claude/skills/post-release-followup/SKILL.md`
