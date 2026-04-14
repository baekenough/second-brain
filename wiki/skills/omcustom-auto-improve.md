---
title: omcustom-auto-improve
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/omcustom-auto-improve/SKILL.md
related:
  - "[[skills/omcustom-improve-report]]"
  - "[[mgr-sauron]]"
  - "[[mgr-gitnerd]]"
  - "[[r017]]"
---

# omcustom-auto-improve

Automated improvement workflow: reads eval-core suggestions, applies selected ones in an isolated worktree with sauron verification, and creates a PR.

## Overview

`omcustom-auto-improve` (slash command: `/omcustom:auto-improve`) is the action counterpart to `omcustom-improve-report`. Where the report skill is read-only, this skill applies improvements. The workflow: read pending suggestions from eval-core → user selects which to apply → apply in git worktree → run sauron verification → create PR if passing.

Prerequisites: eval-core analysis data must exist (run `/omcustom:improve-report` first).

## Key Details

- **Scope**: harness | **User-invocable**: true | **Effort**: high
- Slash command: `/omcustom:auto-improve`

## Workflow

1. Read pending suggestions (status: `proposed`) from eval-core
2. User selects which suggestions to apply
3. Apply changes in isolated git worktree
4. Run `mgr-sauron:watch` verification
5. Create PR for review

## Relationships

- **Report**: [[skills/omcustom-improve-report]] provides the suggestion data this skill acts on
- **Verification**: [[mgr-sauron]] runs Phase 1-3 checks before PR creation
- **Git**: [[mgr-gitnerd]] for worktree and PR operations

## Sources

- `.claude/skills/omcustom-auto-improve/SKILL.md`
