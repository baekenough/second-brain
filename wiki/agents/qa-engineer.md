---
title: qa-engineer
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/qa-engineer.md
related:
  - "[[qa-planner]]"
  - "[[qa-writer]]"
  - "[[db-alembic-expert]]"
  - "[[r020]]"
---

# qa-engineer

QA execution specialist that runs tests, identifies defects, validates fixes, and integrates with CI/CD across multiple testing frameworks.

## Overview

`qa-engineer` is the execution arm of the QA team — it receives plans from [[qa-planner]] and test documentation from [[qa-writer]], then executes across manual and automated testing channels. Its scope includes defect documentation, severity classification, and fix verification.

The agent cannot modify source code in production branches — a deliberate constraint to keep QA execution separate from development.

## Key Details

- **Model**: sonnet | **Effort**: medium | **Memory**: project
- **maxTurns**: 20 | **Limitations**: cannot modify source code in production branches

### Supported Frameworks

Jest, Vitest, pytest, go test, JUnit, Playwright, Cypress

### Capabilities

- Manual and automated test execution, regression testing
- Defect identification, severity classification (Critical/High/Medium/Low), documentation
- Fix verification after developer remediation
- Test script development for new scenarios
- CI/CD integration (GitHub Actions test reporting)
- Cross-browser, API, security testing

## Relationships

- **Receives from**: [[qa-planner]] (strategy and priorities), [[qa-writer]] (test documentation)
- **Reports to**: dev-lead (defect reports), [[qa-writer]] (results for documentation)
- **Migration testing**: [[db-alembic-expert]] collaborates for migration rollback testing
- **Completion**: [[r020]] — verification required before declaring tests "done"

## Sources

- `.claude/agents/qa-engineer.md`
