---
name: project-cutover-floor
description: Config-driven cutover floor for IndexAware collectors (SMS/Whisper) — COLLECTOR_CUTOVER env var, no hardcoded date
metadata:
  type: project
---

Config-driven cutover floor implemented across SMS/Whisper/scheduler/config.

**Why:** Legacy secretary source already holds pre-cutover history. IndexAware collectors would re-collect ALL historical data on every run (whisper would re-transcribe hundreds of old recordings). The floor stops that while preserving post-cutover late-arrival recovery.

**How to apply:** Use `COLLECTOR_CUTOVER=2025-01-01T00:00:00Z` (RFC3339). Default = zero time = disabled.

## Key design decisions
- `config.CollectorCutover` field + `collectorCutover()` helper (RFC3339 parse, invalid → slog.Warn + zero)
- `CutoverAwareCollector` interface in `collector.go` (separate from IndexAwareCollector)
- `SMSCollector.WithCutover(t)` + `WhisperCollector.WithCutover(t)` — stored as `cutover time.Time`
- `shouldEmitSMS`: cutover check FIRST (return false if before cutover), then watermark/indexed-set gate
- Whisper: cutover check added before IndexAware filter in `filepath.WalkDir` callback
- Scheduler: `cutover time.Time` field + `WithCutover(t) *Scheduler` builder
  - `runCollector`: advances `since` to cutover when `since.Before(cutover)` (handles gmail/calendar/secretary/llm-memory)
  - After IndexAware setup: calls `cac.WithCutover(s.cutover)` on CutoverAwareCollectors
- `cmd/collector/main.go`: `.WithCutover(cfg.CollectorCutover)` in scheduler builder chain

## Files changed
- `internal/config/config.go` — CollectorCutover field + collectorCutover() helper
- `internal/collector/collector.go` — CutoverAwareCollector interface
- `internal/collector/sms.go` — cutover field + WithCutover + shouldEmitSMS update
- `internal/collector/whisper.go` — cutover field + WithCutover + Collect walk update
- `internal/scheduler/scheduler.go` — cutover field + WithCutover builder + runCollector propagation
- `cmd/collector/main.go` — WithCutover wiring

## Tests added (all green, -race)
- `internal/collector/sms_cutover_test.go` — 5 SMS/call-log cutover tests
- `internal/collector/whisper_cutover_test.go` — 4 whisper cutover tests
- `internal/config/config_test.go` — TestLoad_CollectorCutover (5 subtests)
- `internal/scheduler/scheduler_cutover_test.go` — 7 scheduler cutover tests
