---
title: research
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/research/SKILL.md
related:
  - "[[skills/result-aggregation]]"
  - "[[skills/deep-plan]]"
  - "[[r018]]"
  - "[[r009]]"
  - "[[r013]]"
---

# research

10-team parallel deep analysis with cross-verification for any topic, repository, or technology — produces ADOPT/ADAPT/AVOID structured reports.

## Overview

`research` (slash command: `/research`) orchestrates 10 parallel research teams for comprehensive analysis. Teams-compatible: works from the main conversation (R010) and inside Agent Teams members. Pre-flight guards prevent running on simple factual questions or topics with fewer than 3 comparison dimensions — at ~$8-15 per full run, this is a high-cost operation.

The output follows an ADOPT/ADAPT/AVOID taxonomy for technology evaluation, making actionable recommendations rather than just summarizing findings.

## Key Details

- **Scope**: core | **User-invocable**: true | **Teams-compatible**: true
- Slash command: `/research`
- **Cost**: ~$8-15 per full run (10 parallel teams)

## When NOT to Use

- Simple factual question (use single WebSearch)
- Single-file code review (use `/dev-review`)
- Topic with < 3 comparison dimensions (single Explore agent)
- Known solution (use `/structured-dev-cycle`)

## Output Taxonomy

`ADOPT` (use it), `ADAPT` (use with modifications), `AVOID` (don't use it) — with supporting evidence from cross-team verification.

## Relationships

- **Result processing**: [[skills/result-aggregation]] for aggregating team findings
- **Planning**: [[skills/deep-plan]] for research-validated planning
- **Parallelism**: [[r009]] for 10-team execution pattern
- **Ecomode**: [[r013]] for compressed team output

## Sources

- `.claude/skills/research/SKILL.md`
