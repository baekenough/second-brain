---
name: feedback-bidirectional-index-verification
description: Table-to-file orphan checks alone miss files-that-exist-but-are-unindexed; verification must run both directions
metadata:
  type: feedback
---

CLAUDE.md's rule summary table was found missing R021/R022/R023 entirely — the rule files existed on disk and were actively in effect, but the index/table of contents simply never listed them. A standard "orphaned reference" audit (table → file, checking that every referenced file exists) cannot catch this class of gap by construction: it only validates references that are present, never notices references that should exist but don't.

**Why:** Orphan-reference checking is directional. It answers "does everything the table points to actually exist?" but not "does everything that exists actually get pointed to?" The second question requires a separate pass: enumerate the real files (`.claude/rules/*.md`, `.claude/agents/*.md`, etc.) and diff against what the index claims to cover.

**How to apply:** Any R017-style sync verification (rule tables, agent counts, skill counts, wiki index counts) must run in both directions:
1. Forward: for each entry in the index/table, confirm the target file exists (orphan check — the existing practice)
2. Reverse: for each file matching the expected glob pattern, confirm it has a corresponding index entry (coverage check — the gap that missed R021-R023)

A related contamination hazard when running either direction of this check: `grep -r` style searches across the repo can pick up stale copies inside `.claude-backup-*/` directories or old worktree checkouts, producing false positives that look like the "real" config is present when only a backup snapshot has it. Exclude backup/worktree directories explicitly (or rely on `.gitignore`'d paths being excluded) before trusting a coverage-check result.

Related: [[project-postgres-migration-macmini-ubuntu1]] pattern of "passes a naive check but is actually wrong" — same shape of failure (checks answer the wrong question), different domain.
