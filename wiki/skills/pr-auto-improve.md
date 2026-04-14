---
title: pr-auto-improve
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/pr-auto-improve/SKILL.md
related:
  - "[[mgr-gitnerd]]"
  - "[[skills/dev-review]]"
  - "[[r010]]"
  - "[[skills/pipeline-guards]]"
---

# pr-auto-improve

Opt-in post-PR analysis and improvement suggestions — advisory-only, never auto-runs, never force-pushes without approval.

## Overview

`pr-auto-improve` analyzes a pull request's diff after creation and suggests targeted improvements. It is strictly opt-in: only activates when the user explicitly says "improve this PR" or "review PR #N". It never runs automatically on PR creation and never force-pushes or modifies PRs without approval (R010).

The max improvement items cap is enforced by [[skills/pipeline-guards]] (default: 20, hard cap: 50).

## Key Details

- **Scope**: core | **User-invocable**: false
- **Opt-in only**: never auto-activates on PR creation

## Activation Triggers

| Trigger | Behavior |
|---------|---------|
| "improve this PR" | Activate analysis |
| "review PR #N" | Activate analysis |
| PR created automatically | Do NOT activate |
| CI fails on PR | Suggest activation only |

## Analysis Categories

- New code: patterns, naming, structure
- Modified code: consistency, regression risk
- Deleted code: orphaned references

## Relationships

- **Git ops**: [[mgr-gitnerd]] for PR interactions
- **Code review**: [[skills/dev-review]] for pre-PR review
- **Delegation**: [[r010]] — advisory only, never direct PR modification

## Sources

- `.claude/skills/pr-auto-improve/SKILL.md`
