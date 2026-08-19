---
name: project-ask-occurred-at-contract
description: /ask AskSourceItem.OccurredAt wire contract (PR #219) — deliberate no-omitempty, JSONB no-migration, sort applies only when planner resolves a date window
metadata:
  type: project
---

PR #219 (commit `290cc3f`) added `OccurredAt *time.Time \`json:"occurred_at"\`` to `AskSourceItem` and the matching `store.AskSource` field.

## Deliberately no `omitempty`

`omitempty` was intentionally NOT applied to `occurred_at`. The bug this PR fixed was rooted in confusing "key absent" with "key present but null" — omitting the tag would have collapsed that distinction back into the same ambiguity for any future consumer. When there's genuinely no timestamp, the field must still serialize as `"occurred_at": null`, not disappear from the JSON body.

## No DB migration needed

`store.AskSource` is persisted through an existing JSONB column, so adding this field required no migration — new rows get the key, existing historical rows simply lack it (nil on decode, matching the "absent" case pre-fix). This is a reusable pattern for the second-brain codebase: JSONB-backed structs can gain new optional fields without a migration as long as decode-side nil-handling is correct.

## Sort ordering is conditional on planner window resolution, not global

Post-deploy the sort behavior for `/ask` sources was verified in production: past time windows sort descending, and — first real observation of this in the project — future time windows (e.g. a "8/20~9/5" calendar query) sort ascending. But **this ordering only kicks in when the query planner has actually resolved a concrete date window**. Vague temporal queries like "지난주에 뭐 했지" (no resolved window) fall back to relevance-only ordering. This is correct behavior, not a bug, but from the outside it can look like "sorting doesn't apply sometimes" — check whether the planner resolved a window before treating inconsistent sort order as a regression.

## How to apply

- When adding new optional fields to JSON API contracts, default to explicit `null` (no `omitempty`) whenever "absent" and "null" need to mean different things to the consumer.
- JSONB-backed store structs can add fields without migrations — verify nil-handling on the decode path covers historical rows.
- Before treating `/ask` (or planner-driven) sort inconsistency as a bug, check whether the query planner resolved an explicit date window for that query — unresolved-window queries intentionally fall back to relevance ordering.

Related: [[project_query_planner]] (lang-golang-expert — plan owns retrieval, Params owns ranking), [[project_sort_recent_vs_rrf]] (this dir — RecencyAscending shared definition).
