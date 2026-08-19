---
name: no-blocking-remote-calls-in-write-path
description: Ingest/capture endpoints in second-brain must never do a synchronous embedding/LLM call before responding — this repo has had the same retransmit-loop outage twice
metadata:
  type: feedback
---

No HTTP ingest/capture handler may block on a remote call (embedding, LLM,
reranker) before writing its response. Persist locally, respond, defer the
remote work to a backfill worker.

**Why:** this exact failure geometry has hit the project twice.
1. Message ingest ran a synchronous per-record re-embed: 224 records took 70 s,
   Cloudflare returned a gateway 502, and the phone retransmitted forever even
   though the server had succeeded (fixed in fe74316, 2026-06-21).
2. `POST /api/v1/notes` ran `EmbedBatch` over up to 2000 chunks inside the
   request, with an empty `sourceID` so every retry minted a new UUID — each
   duplicate note then independently enriched and generating its own insights
   (fixed 2026-08-17, `feature/capture-backend`).

An idempotency key does NOT satisfy this rule on its own: the request still
blocks, still burns the gateway timeout, and still returns nothing while the
backend is down. It treats the duplicates rather than the cause.

**How to apply:** the safe pattern already exists — write chunks with a NULL
embedding and let `Scheduler.backfillChunkEmbeddings` (`internal/scheduler`,
selects `chunks WHERE embedding IS NULL`, runs every collection cycle) fill
them in. `note.Save(..., doEmbed=false, ...)` is the switch. When reviewing any
new ingest handler, trace every call between request decode and
`writeJSON` and confirm none of them crosses the network.

Caveat worth checking before relying on the backfill: it lives in the scheduler
(cmd/collector), not cmd/server. A server-only deployment would never fill
those vectors.

Related: [[project_note_capture_enrichment]], [[feedback_test_doubles_must_fail]]
