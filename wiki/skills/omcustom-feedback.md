---
title: omcustom-feedback
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/omcustom-feedback/SKILL.md
related:
  - "[[r016]]"
---

# omcustom-feedback

Submit feedback about oh-my-customcode (bugs, features, improvements) directly from the terminal session as GitHub issues.

## Overview

`omcustom-feedback` lowers the friction for filing issues against the `baekenough/oh-my-customcode` repository by allowing submission without leaving the Claude Code session. Supports anonymous submissions (prefixes title with `[Anonymous Feedback]`).

`disable-model-invocation: true` — this skill is script-driven.

## Key Details

- **Scope**: harness | **User-invocable**: true
- **Arguments**: `[description] [--anonymous]`
- Slash command: `/omcustom-feedback`
- Target repo: `baekenough/oh-my-customcode`

## Usage

```
/omcustom-feedback HUD display is missing during parallel agent spawn
/omcustom-feedback --anonymous Something feels off with the routing
/omcustom-feedback   (interactive)
```

## Relationships

- **Improvement loop**: Feedback informs [[r016]] continuous improvement rule updates

## Sources

- `.claude/skills/omcustom-feedback/SKILL.md`
