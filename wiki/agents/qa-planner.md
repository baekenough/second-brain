---
title: qa-planner
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/qa-planner.md
related:
  - "[[qa-writer]]"
  - "[[qa-engineer]]"
  - "[[arch-speckit-agent]]"
  - "[[r020]]"
---

# qa-planner

QA planning specialist that creates comprehensive test strategies, risk-based prioritization, and acceptance criteria definitions from requirements.

## Overview

`qa-planner` transforms requirements and specifications into structured QA plans. It cannot execute tests or modify code — it is a pure planning agent, creating the strategy that [[qa-writer]] documents and [[qa-engineer]] executes.

Risk-based prioritization is the core approach: not all features need the same test depth, and the planner's job is deciding where coverage effort is most valuable.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: project
- **maxTurns**: 20 | **disallowedTools**: Bash | **Limitations**: cannot execute tests, cannot modify code

### Output Format

YAML qa_plan with: scope, strategy, scenarios (id, description, priority, type, preconditions, steps, expected_result), acceptance_criteria (criterion + validation), risks (risk + mitigation).

### Capabilities

- Risk-based test prioritization (business impact × likelihood)
- Test coverage analysis per component
- Test approach selection: unit, integration, E2E, performance
- Edge case and boundary condition identification
- Data dependency mapping for test fixtures

## Relationships

- **Receives**: requirements, user stories from orchestrator or [[arch-speckit-agent]]
- **Outputs to**: [[qa-writer]] (plan → documentation), [[qa-engineer]] (plan → execution)
- **Acceptance criteria**: [[arch-speckit-agent]] EARS format compatible
- **Completion verification**: [[r020]]

## Sources

- `.claude/agents/qa-planner.md`
