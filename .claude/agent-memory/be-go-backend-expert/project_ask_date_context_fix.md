---
name: project-ask-date-context-fix
description: /api/v1/ask date-context bug fix — buildAskSystemPrompt(now) + occurred_at in document context lines, KST rendering
metadata:
  type: project
---

Fixed a prod-reproduced bug in `internal/api/ask.go`: the Stage 3 synthesis
system prompt was a compile-time `const` with no current-date awareness, and
`buildAskMessages` never included each document's `occurred_at` — so the
model could not resolve "내일"/"어제" and answered it didn't know what day
it was.

**Fix shape** (2026-08-17, no commit made — user's task explicitly forbade
committing):
- `askSystemPrompt` const → `askSystemPromptTemplate` + `buildAskSystemPrompt(now time.Time) string`, renders current date/weekday in KST and instructs the model to resolve relative date terms against it.
- `buildAskMessages` document lines now include `[발생: <formatOccurredAt>]`; nil `OccurredAt` (all llm-memory-source docs, per `internal/model/document.go`) renders `"시각 정보 없음"` rather than panicking or being silently omitted.
- Timezone: explicit `Asia/Seoul` via `time.LoadLocation`, with `time.FixedZone("KST", 9*3600)` fallback if tzdata is unavailable — chosen because prod `Dockerfile` `runtime-base` stage installs `tzdata` specifically for this, and Korea has no DST so the fixed offset is exact, not approximate.
- `Server` got a `now func() time.Time` field + `nowFunc()` helper, mirroring `internal/intent.LLMClassifier`'s existing `now`/nil-means-`time.Now` injected-clock convention exactly — this was an explicit instruction from the user's task brief ("일관성이 중요합니다") to reuse `internal/intent`'s established pattern rather than invent a new one.

**Why this matters for future work**: this project has (at least) two
places that need "now" injected for deterministic date tests —
`internal/intent.LLMClassifier` and now `internal/api.Server`. Both follow
identical shape (`now func() time.Time` field, nil→`time.Now`, exported
setter/field only in same-package or `export_test.go`). Any future package
needing a testable clock in this repo should follow the same convention —
see [[feedback_go_now_injection_convention]] if that memory exists, or use
this file as the reference example.

Verification performed: TDD (tests written first, confirmed compile-failure,
then implementation), `go build ./...`, `go vet ./...`,
`go test ./internal/api/...` and `go test ./...` (full repo, no
regressions), `gofmt -l` on touched files (clean). Forbidden paths
(`internal/intent/`, `internal/llm/`, `internal/worker/`, `internal/store/`,
`migrations/`, `web/`) were not touched — confirmed via `git status
--short internal/` showing only `internal/api/ask.go`,
`internal/api/ask_test.go`, `internal/api/router.go` modified.
