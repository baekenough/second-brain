---
title: "Guide: Git Worktree Workflow"
type: guide
updated: 2026-04-12
sources:
  - guides/git-worktree-workflow/README.md
related:
  - "[[mgr-gitnerd]]"
  - "[[r001]]"
  - "[[r006]]"
---

# Guide: Git Worktree Workflow

Reference documentation for Git worktrees — parallel development workflows, agent isolation, and branch management patterns.

## Overview

The Git Worktree Workflow guide covers `git worktree` patterns for parallel development and agent isolation. Worktrees allow multiple branches to be checked out simultaneously in separate directories — enabling agents to work on different features without branch switching conflicts.

This guide is particularly relevant for the `isolation: worktree` agent frontmatter option (R006), which uses git worktrees to give agents isolated file system views.

## Key Topics

- **Worktree Basics**: `git worktree add`, `list`, `remove`, `prune` commands
- **Parallel Development**: Feature branch isolation without stashing
- **Agent Isolation**: How `isolation: worktree` creates separate checkout directories
- **Cleanup**: Preventing stale worktrees, pruning detached references
- **Conventions**: Naming worktree directories, organizing parallel work

## Relationships

- **Git operations**: [[mgr-gitnerd]] for all git worktree commands
- **Isolation frontmatter**: [[r006]] documents the `isolation: worktree` agent option
- **Safety**: [[r001]] for branch protection rules with worktrees

## Sources

- `guides/git-worktree-workflow/README.md`
