---
name: feedback-dirty-worktree-staging
description: Never git add -A on a worktree with pre-existing unrelated changes — stage explicit paths and diff staged vs intended
metadata:
  type: feedback
---

Rule: when committing in a working tree that already has unrelated uncommitted/untracked changes (e.g. a harness reinit left 4 days of pending edits, or a parallel agent's in-progress artifact writes), never run `git add -A` or `git add .`. Stage files by explicit path only, then run `git status --short` (or `git diff --cached --stat`) against the intended file list before committing.

**Why:** `git add -A` in this project has repeatedly swept in files the current task never touched — stray backup files (`*.bak.*`, `.ci-test-tmp`), other agents' concurrent in-progress writes (see [[feedback_concurrent_file_writers]] in sys-memory-keeper), and unrelated pending edits from earlier sessions that hadn't been committed yet. A commit that silently bundles unrelated changes makes `git log`/`git blame` misleading for future sessions and risks committing something sensitive (secret-derived filenames slip past `.gitignore` — see [[feedback_secret_derived_filenames]]).

**How to apply:** Before every commit —
1. `git status --short` first, always, even for "just this one file" commits.
2. Stage only the specific paths relevant to the current task (`git add path1 path2`), never a wildcard/blanket add.
3. Re-run `git status --short` after staging and diff it mentally against the intended file list — anything unexpected must be investigated (is it a stray backup? another agent's in-flight write? a leftover from a prior uncommitted session?) before proceeding.
4. If the working tree already has unrelated dirty state at task start, flag it to the orchestrator/user rather than silently absorbing it into the next commit.
