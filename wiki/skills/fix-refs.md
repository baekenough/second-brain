---
title: fix-refs
type: skill
updated: 2026-04-12
sources:
  - .claude/skills/fix-refs/SKILL.md
related:
  - "[[mgr-supplier]]"
  - "[[skills/audit-agents]]"
  - "[[skills/update-docs]]"
  - "[[r017]]"
---

# fix-refs

Harness skill that repairs broken agent references — missing skill refs, missing guide refs, broken paths, and invalid references.

## Overview

`fix-refs` (slash command: `/omcustom:fix-refs`) is the repair counterpart to `audit-agents`. After `mgr-supplier:audit` identifies broken dependencies, this skill applies fixes: adds missing skill or guide references to agent `.md` files, updates broken paths, and removes invalid references. It re-runs the audit after fixing to confirm health.

The skill is a harness-scope utility — it modifies `.claude/agents/*.md` files, not source code. Dry-run mode shows proposed changes without applying them.

## Key Details

- **Scope**: harness | **User-invocable**: true
- **Arguments**: `[agent-name] [--all] [--dry-run] [--verbose]`
- Slash command: `/omcustom:fix-refs`

## Fixable Issues

| Issue | Action |
|-------|--------|
| Missing skill reference | Add to agent frontmatter |
| Missing guide reference | Add to agent frontmatter |
| Broken path | Update path in agent file |
| Invalid reference | Remove from agent file |

## Workflow

1. Run `mgr-supplier:audit` to identify issues
2. Apply fixes per issue type
3. Re-run `mgr-supplier:audit` to validate

## Relationships

- **Audit**: [[skills/audit-agents]] identifies issues that fix-refs repairs
- **Verification**: [[mgr-supplier]] orchestrates both audit and fix
- **Sync rule**: [[r017]] requires fix-refs to pass before commit

## Sources

- `.claude/skills/fix-refs/SKILL.md`
