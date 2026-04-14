---
title: npm-publish
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/npm-publish/SKILL.md
related:
  - "[[tool-npm-expert]]"
  - "[[skills/npm-version]]"
  - "[[skills/npm-audit]]"
  - "[[mgr-gitnerd]]"
---

# npm-publish

Publish npm packages to the registry with comprehensive pre-publish checks: validation, version conflict detection, tests, lint, build, and dry-run pack.

## Overview

`npm-publish` (slash command: `/omcustom:npm-publish`) enforces a structured pre-publish checklist before executing `npm publish`. It validates `package.json` configuration, checks the version against the registry (prevents version conflicts), runs tests and lint, builds the package, executes `npm pack --dry-run` for sanity, then publishes with the specified dist-tag.

`disable-model-invocation: true` — this skill is script-driven.

## Key Details

- **Scope**: package | **User-invocable**: true
- **Arguments**: `[--tag <tag>] [--dry-run] [--skip-tests]`
- Slash command: `/omcustom:npm-publish`

## Workflow

1. Validate `package.json` (name, version, main, exports)
2. Check version against registry (conflict detection)
3. Run tests and lint
4. Execute `npm pack` dry-run
5. Publish with dist-tag
6. Verify publication success

## Relationships

- **Version management**: [[skills/npm-version]] for bumping versions before publish
- **Dependency audit**: [[skills/npm-audit]] for security check before publish
- **Agent**: [[tool-npm-expert]] for npm package operations

## Sources

- `.claude/skills/npm-publish/SKILL.md`
