---
name: note-capture-enrichment
description: feature/capture-backend note+insight pipeline — the three-budget deadline model, the insight echo-chamber guards, and where each one lives
metadata:
  type: project
---

The Capture feature (`feature/capture-backend`) adds `POST /api/v1/notes` →
`source_type='note'` documents → `NoteEnrichmentWorker` → up to 3
`source_type='insight'` documents per note. Five review findings were fixed on
2026-08-17 (see `.superpowers/sdd/2026-08-17-capture-backend/review-fix-report.md`).

**Why these two design facts are worth remembering:** both are non-obvious
invariants that a future edit can silently break, and neither is discoverable
without reading several files at once.

## 1. NoteEnrichmentWorker runs on THREE deadlines, not one

Never reintroduce a single per-document context. The budgets are:

| Budget | Derived from | Covers |
|--------|--------------|--------|
| work | the **caller's** ctx (`LLMTimeout + 30s`) | the LLM call only |
| persist | `WithoutCancel(ctx)`, 30 s | insight + entity writes |
| bookkeeping | `WithoutCancel(ctx)`, **fresh per write**, 10 s | one status UPDATE |

The LLM client's own HTTP timeout (`LLM_TIMEOUT_SECONDS`, default 120)
**governs**; the work budget is derived from it and clamped up, so the client's
timeout always fires first. A shared deadline put bookkeeping writes on a dead
context, so `enrichment_attempts` never incremented and notes were re-billed
every tick forever — with `ListPendingNotes` being
`ORDER BY collected_at ASC LIMIT 5`, five stuck notes starved everything newer.

A caller cancellation (SIGTERM) is deliberately NOT a failed attempt; a
work-deadline expiry is. `cmd/collector/main.go` sizes its SIGTERM drain from
`NoteEnrichmentWorker.MaxDetachedWriteDuration()` — only the write phases are
detached, so the multi-minute LLM budget must not appear in that calculation.

## 2. The insight echo-chamber guard is layered, and every layer is load-bearing

An LLM inference must never become evidence for another inference, or be
spoken back to the user as fact. Four independent gates:

- `ListPendingNotes` is hard-scoped to `source_type='note'` (insight-of-insight
  is structurally impossible).
- `search.Service.Search` applies the insight exclusion — **in the service, not
  in the api handlers**. It was in the handlers, which left `discord.go` (RAG
  → LLM → user prose), `graphql.go` and `cmd/mcp` unguarded. Chunk lanes need a
  separate post-filter because `ChunkSearcher` takes no source-type argument.
- `listUnsummarizedQuery` / `listWithoutEntitiesQuery` exclude
  `source_type='insight'`, or SummarizerWorker and EntityWorker re-feed
  insights into paid LLM calls and into the entity RRF lane.
- The hybrid-search entity CTE (`buildEntityCTE`) must keep its
  `JOIN documents d` + `d.`-qualified filters — without them the entity lane
  bypassed both soft-delete and every source-type exclusion. Filters go INSIDE
  the CTE because it is capped by `LIMIT $3`.

**How to apply:** when touching search, the enrichment worker, or either
backfill listing, check which of these four gates the change moves through.
Removing any one silently re-opens the loop; only the search one has a test
that fails loudly (`internal/search/insight_exclusion_test.go`).

Related: [[feedback_test_doubles_must_fail]],
[[feedback_no_blocking_remote_calls_in_write_path]], [[project_second_brain]]
