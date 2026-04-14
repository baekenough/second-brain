---
title: qa-writer
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/qa-writer.md
related:
  - "[[qa-planner]]"
  - "[[qa-engineer]]"
  - "[[arch-documenter]]"
  - "[[r020]]"
---

# qa-writer

QA documentation specialist that transforms test plans into detailed test cases, execution reports, and quality documentation.

## Overview

`qa-writer` bridges the gap between [[qa-planner]]'s strategy documents and [[qa-engineer]]'s executable test runs. It produces step-by-step test cases with concrete data specifications and expected results — documentation precise enough that any engineer can execute the tests without ambiguity.

Cannot execute tests or modify source code — its role is strictly documentation production.

## Key Details

- **Model**: sonnet | **Effort**: medium | **Memory**: project
- **maxTurns**: 20 | **disallowedTools**: Bash | **Limitations**: cannot execute tests, cannot modify source code

### Document Types

- **Test Cases**: step-by-step with data specs, preconditions, expected results
- **Execution Reports**: summary reports after qa-engineer runs
- **Defect Reports**: structured defect documentation with reproduction steps
- **Coverage Reports**: test coverage analysis against requirements
- **Regression Docs**: regression test suites for CI integration
- **Release Readiness**: go/no-go criteria documentation

## Relationships

- **Receives from**: [[qa-planner]] (strategy and plans)
- **Outputs to**: [[qa-engineer]] (executable test documentation), [[arch-documenter]] (QA archive)
- **Completion**: [[r020]] — documentation must be verifiable before declaring complete

## Sources

- `.claude/agents/qa-writer.md`
