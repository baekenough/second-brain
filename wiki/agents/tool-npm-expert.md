---
title: tool-npm-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/tool-npm-expert.md
related:
  - "[[mgr-gitnerd]]"
  - "[[lang-typescript-expert]]"
  - "[[tool-optimizer]]"
  - "[[skills/npm-audit]]"
  - "[[skills/npm-publish]]"
  - "[[skills/npm-version]]"
---

# tool-npm-expert

npm package management specialist for publishing workflows, semantic versioning, package.json optimization, and dependency auditing.

## Overview

`tool-npm-expert` manages npm packages from development through publication — versioning strategy, changelog management, registry publishing, and vulnerability auditing. It integrates with [[mgr-gitnerd]] for version commits and tags, keeping git history consistent with package versions.

Three modes: Publish (validate → dry-run → publish → verify), Version (bump type → update files → commit + tag), Audit (vulnerabilities → analysis → fix suggestions).

## Key Details

- **Model**: sonnet | **Effort**: medium | **Memory**: project
- **Domain**: universal | **Skills**: npm-audit, npm-publish, npm-version

### Workflow Summary

**Publish**: Validate package.json fields, check version, run tests/lint, `npm pack --dry-run`, `npm publish`, verify on registry.

**Version**: Determine bump type (major/minor/patch), update package.json + CHANGELOG.md, create commit + git tag.

**Audit**: `npm audit`, analyze by severity, suggest `npm update` or `npm audit fix`, check outdated.

## Relationships

- **Version commits**: [[mgr-gitnerd]] for git commit + tag after version bump
- **TypeScript builds**: [[lang-typescript-expert]] for tsc compilation before publish
- **Bundle analysis**: [[tool-optimizer]] for pre-publish bundle size check
- **Bun alternative**: [[tool-bun-expert]] when using Bun package manager

## Sources

- `.claude/agents/tool-npm-expert.md`
