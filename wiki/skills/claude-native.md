---
title: claude-native
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/claude-native/SKILL.md
related:
  - "[[mgr-claude-code-bible]]"
  - "[[mgr-gitnerd]]"
---

# claude-native

Monitor Claude Code releases and auto-generate GitHub issues for each new version.

## Overview

`claude-native` monitors the Claude Code changelog and creates structured GitHub issues for each new version, enabling the team to track which features have been evaluated and potentially adopted. The `--backfill` flag processes historical versions.

## Key Details

- **Scope**: core | **User-invocable**: true | **Version**: 1.0.0
- **Arguments**: `[--backfill] [--dry-run]`

## Relationships

- **Version reference**: [[mgr-claude-code-bible]] for official spec compliance verification
- **Issues**: [[mgr-gitnerd]] for GitHub issue creation via gh CLI

## Sources

- `.claude/skills/claude-native/SKILL.md`
