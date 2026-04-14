---
title: mgr-gitnerd
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/mgr-gitnerd.md
related:
  - "[[mgr-sauron]]"
  - "[[tool-npm-expert]]"
  - "[[r010]]"
  - "[[r017]]"
---

# mgr-gitnerd

Git operations specialist for commits, branches, PRs, history management, and GitHub workflow following conventional commits and safety rules.

## Overview

`mgr-gitnerd` is the sole agent authorized to execute git operations per R010 (orchestrator never runs git commands directly). Its push rule is critical: all pushes require prior `mgr-sauron:watch` verification — it will REFUSE a push if sauron was not run.

The agent cannot modify source code or create agents — it is strictly a git workflow specialist.

## Key Details

- **Model**: sonnet | **Effort**: medium | **Memory**: project
- **maxTurns**: 20 | **Limitations**: cannot modify source code, cannot create agents

### Capabilities

- Conventional commits (`feat:`, `fix:`, `docs:`, `style:`, `refactor:`, `test:`, `chore:`)
- Branch management, rebase/merge, conflict resolution
- PR creation with descriptions, branch naming enforcement
- GPG/SSH signing, credential management
- Cherry-pick, squash, history cleanup

### Commit Format

```
<type>(<scope>): <subject>

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>
```

### Safety Rules

- NEVER force push to main/master
- NEVER reset --hard without confirmation
- NEVER skip pre-commit hooks without reason
- ALWAYS create new commits (avoid --amend unless requested)

## Relationships

- **Required before push**: [[mgr-sauron]] verification gate
- **Version commits**: [[tool-npm-expert]] delegates version tag commits
- **R010 delegation**: all git operations must route through this agent
- **R017**: verification flow ends here with commit and push

## Sources

- `.claude/agents/mgr-gitnerd.md`
