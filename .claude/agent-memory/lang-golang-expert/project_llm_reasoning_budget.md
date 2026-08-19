---
name: llm-reasoning-budget
description: deepseek-v4-flash reasoning_content eats max_tokens and returns empty completions; LLM_THINKING=disabled is the fix, reasoning_effort=low was measured and rejected
metadata:
  type: project
---

`deepseek-v4-flash` (the remote curation/extraction model) is a **reasoning** model, and its
`reasoning_content` is billed against the same `max_tokens` budget as the visible answer. On
inputs whose reasoning runs long the budget is spent before any content is emitted: the response
comes back with `content=""` and `finish_reason="length"`, which downstream looked like
`"failed to parse LLM JSON"` (~16% of extraction-worker documents).

Measured on one 2,768-char document (do not re-measure — costs money):

| option | result | finish_reason | content | reasoning | completion tokens |
|---|---|---|---|---|---|
| `max_tokens=8000` (prod value) | empty | length | 0 | 25,388 | 8,000 |
| `max_tokens=32000` | ok | stop | 9,253 | 30,106 | 13,042 |
| `reasoning_effort=low` | **still truncated** | length | 3,952 | 22,842 | 8,000 |
| `thinking={"type":"disabled"}` | ok | stop | 1,922 | 0 | 750 |

**Why:** `reasoning_effort=low` was tried and rejected — it truncates too. Raising `max_tokens`
works but costs ~17x the tokens. Disabling reasoning is the only option that is both correct and
cheap; the same reasoning tokens are the suspected cause of the summary backfill costing 3.2x its
estimate.

**How to apply:** `LLM_THINKING` (default `disabled`) gates the `thinking` request field in
`internal/llm`. The field is a **pointer with `omitempty`** — `enabled` omits the key entirely
rather than sending `{"type":"enabled"}`, because OpenAI-compatible endpoints that do not
implement this vendor parameter answer HTTP 400. Never make it a value-type struct field.
Truncation (`finish_reason=length`) is surfaced as `llm.TruncatedError` / `llm.ErrTruncated` and
is deliberately **not retried** — the same prompt against the same budget fails identically and
would bill three full generations per failure.

Related: [[feedback-never-log-raw-llm-output]].
