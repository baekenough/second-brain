---
title: omcustom-analysis
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/analysis/SKILL.md
related:
  - "[[mgr-creator]]"
  - "[[skills/create-agent]]"
  - "[[mgr-supplier]]"
  - "[[r010]]"
---

# omcustom-analysis

Scan a project's tech stack, compare against installed agents and skills, and auto-configure missing items — the system bootstrap skill.

## Overview

`omcustom-analysis` (slash command: `/omcustom:analysis`) is the entry point for setting up oh-my-customcode on a new project. It detects the tech stack via file scanning (package.json, go.mod, requirements.txt, Dockerfiles, etc.), compares detected languages and frameworks against installed agents and skills, and creates missing configuration.

An optional `--interview` mode conducts an interactive architecture questionnaire to capture human context that file scanning cannot determine (project type, team conventions, deployment target).

## Key Details

- **Scope**: harness | **User-invocable**: true
- **Arguments**: `[target-dir] [--interview] [--dry-run] [--verbose]`
- Slash command: `/omcustom:analysis`

## Workflow

1. Optional: run architecture interview (--interview)
2. Scan project files (package.json, go.mod, *.py, Dockerfile, etc.)
3. Detect languages and frameworks
4. Compare against installed agents/skills
5. Create missing agents, skills, guide references
6. Report configuration summary

## Relationships

- **Creation**: [[mgr-creator]] creates any missing agents discovered
- **Audit**: [[mgr-supplier]] validates the resulting configuration

## Sources

- `.claude/skills/analysis/SKILL.md`
