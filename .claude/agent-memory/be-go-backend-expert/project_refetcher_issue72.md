---
name: project-refetcher-issue72
description: Issue #72 — Refetcher interface for remote-file extraction retry in ExtractionRetryWorker
metadata:
  type: project
---

Implemented `Refetcher` interface in `internal/worker/refetch.go` to enable remote-file retry for extraction failures.

**Key design decisions:**
- `Refetcher` interface: `Refetch(ctx, ExtractionFailure) (*RefetchResult, error)` — returns temp file path + cleanup func.
- `ErrRefetchNotSupported` sentinel: unsupported sources return this, worker skips (no regression).
- `URLRefetcher`: handles `http://`/`https://` URLs. Discord needs no auth; Slack needs `SlackBotToken`.
- `Config.Refetcher` is OPTIONAL in worker — nil means old skip-and-log behavior.
- Discord `processAttachment` was changed to store `att.URL` (not `att.Filename`) in `ExtractionFailure.FilePath` so URLs are available for retry.

**Files changed:**
- `internal/worker/refetch.go` — new: Refetcher interface, ErrRefetchNotSupported, RefetchResult, URLRefetcher
- `internal/worker/extraction_retry.go` — Config.Refetcher field, processRemote(), extractAndResolve(), recordFailure() helpers; TODO(issue#8-followup) resolved
- `internal/collector/discord.go` — FilePath now `att.URL` instead of `att.Filename` in ExtractionFailure.Record
- `internal/worker/extraction_retry_test.go` — 5 new Refetcher test cases
- `internal/worker/refetch_test.go` — new: URLRefetcher unit tests (httptest server)

**Wiring note:**
`cmd/collector/main.go` wires the retry worker WITHOUT a Refetcher (backward compatible). To enable Discord/Slack retry, inject `worker.NewURLRefetcher(cfg.SlackBotToken)` into `worker.Config.Refetcher`. Left as a follow-up because Slack token is available in cfg but wiring site comment already explains this.

**Why:** Discord CDN URLs are time-limited but usable within retry window; `ErrRefetchNotSupported` + nil-safe design ensures zero regression.

**Security hardening (deep-verify):**
- `allowedRefetchHosts` map (discord/slack CDN hostnames) — SSRF guard, credential scoping.
- `isAllowedRefetchHost(rawURL, sourceType)` — case-insensitive; rejects unknown sourceTypes.
- `URLRefetcher.hostChecker` injectable field — allows test bypass without mutating global allowlist.
- `CheckRedirect` closure in `download()`: max 3 hops, reject off-allowlist redirect, strip `Authorization` on host change.
- `ResponseHeaderTimeout: 10s` via custom `http.Transport` — slow-loris mitigation.
- `migration 015_chunk_embeddings.sql` guard: `WHERE embedding IS NOT NULL` prevents false block on text-only rows.
- `extraction_retry.go` title fix: `filepath.Base(urlPath(f.FilePath))` strips CDN query string from document title.
