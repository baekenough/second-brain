---
title: omcustom-release-notes
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/omcustom-release-notes/SKILL.md
related:
  - "[[mgr-gitnerd]]"
  - "[[skills/npm-version]]"
  - "[[skills/npm-publish]]"
---

# omcustom-release-notes

Generate structured release notes from git history and closed GitHub issues within the Claude Code session — no external API needed.

## Overview

`omcustom-release-notes` (slash command: `/omcustom-release-notes`) replaces CI-based release note generation. It analyzes git commits and GitHub issues between the previous tag and current version, then generates structured release notes suitable for `gh release create --notes`.

The skill determines the previous tag automatically (`git tag --sort=-version:refname`) but accepts an explicit `--previous-tag` override.

## Key Details

- **Scope**: harness | **User-invocable**: true
- **Arguments**: `<version> [--previous-tag <tag>]`
- Slash command: `/omcustom-release-notes`

## Workflow

1. Determine previous tag (auto or explicit)
2. Collect git commits between tags
3. Fetch related closed GitHub issues
4. Generate structured release notes
5. Pass notes to `gh release create`

## Relationships

- **Version management**: [[skills/npm-version]] bumps the version before this skill runs
- **Git operations**: [[mgr-gitnerd]] for tag and release creation

## Sources

- `.claude/skills/omcustom-release-notes/SKILL.md`
