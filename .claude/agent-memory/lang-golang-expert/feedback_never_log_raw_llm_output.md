---
name: feedback-never-log-raw-llm-output
description: LLM completions and error messages must never carry raw response text into logs — log a length plus a closed-vocabulary shape label instead
metadata:
  type: feedback
---

Never log raw LLM output (not even a truncated 200-char prefix), and never put a response body
into an error message. Log a **length** plus a **content-free shape label** drawn from a closed
vocabulary (`empty` / `fenced-block` / `json-object` / `json-object-unterminated` / `json-array` /
`non-json-text`).

**Why:** the workers run over SMS, call transcripts and mail, so a "harmless prefix" routinely
contains real names, phone numbers and e-mail addresses. Registered as issue #194 (high priority)
after the extraction worker's parse-failure path was found logging exactly that. The prefix also
had zero diagnostic value: the actual failure was an *empty* completion, which the truncated-prefix
log rendered as `response=""` and nobody noticed.

**How to apply:** applies to every `slog` call in a parse-failure path and to error strings in
`internal/llm`. `internal/worker/extraction_worker.go` has the reference implementation
(`llmResponseShape`). As of 2026-08-18 the same 200-char-prefix leak still exists in
`internal/worker/{entity_extractor.go:113, note_enrichment_worker.go:727, summarizer.go:385}` —
they were left alone only because another agent held those files in a worktree.

Related: [[llm-reasoning-budget]].
