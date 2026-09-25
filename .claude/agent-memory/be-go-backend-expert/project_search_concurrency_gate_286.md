---
name: project-search-concurrency-gate-286
description: #286 item 3 search concurrency gate — per-search-call slots, REST/GraphQL/golden FIFO 1s then 503, /ask yields (TryAcquire polling, 10s cap); real-DB harness gotchas
metadata:
  type: project
---

Search gate (internal/api/search_gate.go, 2026-09-25, branch feature/v0.25.2-b): x/sync semaphore, K = max(1, MaxConns/2), SEARCH_MAX_CONCURRENCY override clamped to MaxConns-1. Acquired only in searchWithTimeout (before the timeout ctx) and gatedSearcher (/ask). Never nest a request-level acquire around these — that creates hold-and-wait deadlock.

**Why:** search and ingest (phone push) share one pgx pool; ingest uses r.Context() only, so a saturated pool starves it → Cloudflare 502 → phone resend loop.

**How to apply:**
- graphql-go v0.8.1 resolves query fields sequentially in one goroutine (executeSubFields; our resolvers return no thunks), so per-search slots are deadlock-free. Re-check this if a resolver ever returns a thunk/func.
- MCP (cmd/mcp), eval, and tune each run as their own process with their own pool, so they are out of scope. Discord NewDiscordGateway has no caller in cmd/ (dead wiring).
- Real-DB proof: TestSearchGate_IngestProtection_RealDB (pool_max_conns=4, pg_sleep searcher, monitor conn outside the pool). Control: ingest starved until its 3s deadline. Gated: ingest took ~40ms.
- Gotcha: smsmap PII redaction rewrites long digit runs inside SMS bodies to [REDACTED], which breaks UUID test markers, so a `LIKE marker` cleanup silently misses the rows. Build markers without digits.
- Observation, not fixed: ingest/messages returns 201 even when every upsert failed (the failures only appear in the errors[] list).
- Priority split (deep-verify follow-up): if /ask queues in the same FIFO, a backlog of /ask waiters starves REST (REST waits past its 1s window and gets 503 while slots are still cycling). Capping /ask waiters (K×2) or their wait time does NOT fix that; the fix is for /ask to poll TryAcquire, which fails whenever waiters are queued, so Release always wakes FIFO (REST) waiters first. /ask waits at most askSearchWaitMax=10s per search, then fails with "retrieval failed".
- A deadline that expires while waiting for a slot (GraphQL request budget) maps to errSearchTimeout. Otherwise it logs slog.Error.
- Fault-injection harness: give subprocess test runs a timeout. A fault that turns a bounded wait into an unbounded one hangs the test binary for its default 10m.
- Related: [[project_search_timeout_input_validation]]
