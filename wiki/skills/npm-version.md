---
title: npm-version
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/npm-version/SKILL.md
related:
  - "[[tool-npm-expert]]"
  - "[[skills/npm-publish]]"
  - "[[mgr-gitnerd]]"
  - "[[skills/omcustom-release-notes]]"
---

# npm-version

Semantic version management for npm packages: bump major/minor/patch with automatic changelog and git tag integration.

## Overview

`npm-version` (slash command: `/omcustom:npm-version`) automates the version bump workflow: updates `package.json` version, generates/updates CHANGELOG.md, creates a git commit, and optionally creates a git tag. `disable-model-invocation: true` — this skill is script-driven.

## Key Details

- **Scope**: package | **User-invocable**: true
- **Arguments**: `<major|minor|patch> [--no-tag] [--no-commit]`
- Slash command: `/omcustom:npm-version`

## Version Bump Types

| Type | Change | Example |
|------|--------|---------|
| `major` | Breaking changes | 1.0.0 → 2.0.0 |
| `minor` | New features | 1.0.0 → 1.1.0 |
| `patch` | Bug fixes | 1.0.0 → 1.0.1 |

## Relationships

- **Publish**: [[skills/npm-publish]] uses the bumped version for registry publication
- **Release notes**: [[skills/omcustom-release-notes]] generates changelog from git history
- **Git operations**: [[mgr-gitnerd]] for the resulting commit/tag

## Sources

- `.claude/skills/npm-version/SKILL.md`
