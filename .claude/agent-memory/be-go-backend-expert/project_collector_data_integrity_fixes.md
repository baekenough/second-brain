---
name: project_collector_data_integrity_fixes
description: Deep-verify HIGH/MEDIUM/LOW data-integrity and privacy bug fixes in SMS/Whisper/Gmail collectors (2026-06-09)
metadata:
  type: project
---

## SMS/Whisper/Gmail Collector Data-Integrity Fixes (2026-06-09)

All changes are in working tree (not committed).

### HIGH Bugs Fixed

**HIGH#1: Event-time + wall-clock watermark data loss**
- Root cause: `sms.go` and `whisper.go` used `OccurredAt > since` / `mtime > since` filters.
  Records arriving after a run (OneDrive sync lag) had event-time < watermark → permanently lost.
- Fix: Added `IndexAwareCollector` interface in `collector.go`. Both `SMSCollector` and
  `WhisperCollector` now implement `WithIndexedIDs(map[string]struct{})`.
- The scheduler's `runCollector` (`scheduler.go`) was generalized from the old
  `*FilesystemCollector`-specific type assertion to use `IndexAwareCollector` for ALL collectors.
- Emission condition: `OccurredAt > since OR (indexedIDs != nil AND sourceID ∉ indexedIDs)`.

**HIGH#2: XML token loop treats io.EOF and parse errors identically**
- Root cause: `if err != nil { break }` silently swallowed XML truncation errors.
  Partial collect succeeded → watermark advanced → post-truncation records permanently lost.
- Fix: `errors.Is(err, io.EOF)` distinguishes clean end vs real errors.
  Real errors emit `slog.Warn("sms: xml token stream error (file may be truncated; records will be re-collected next run)")`.

### MEDIUM Bugs Fixed

**PII in SourceID** (`sms.go:206, 270`):
- Old: `sms:{ms}:{raw_phone}` logged downstream.
- New: `sms:{ms}:{sha256(addr)[:16]}:{sha256(body)[:8]}` — address hashed, body hash as collision discriminator.
- Call-log: `call-log:{ms}:{sha256(number)[:16]}:{sha256(duration_str)[:8]}`.

**OTP body redaction** (`sms.go`):
- When `is_auth_like == true`, `otpDigitsRe.ReplaceAllString(body, "[REDACTED]")` applied before storing Content.
- Raw OTP never reaches external LLM / embedding API. Surrounding context preserved.

**Unbounded memory guard** (`sms.go`):
- Originally hardcoded `smsMaxFileBytes = 256 << 20` (256 MB) — caused regression when user's
  real SMS export grew to 298 MB (silently skipped).
- Fixed (2026-06-09 follow-up): cap is now configurable via `SMS_MAX_FILE_BYTES` env var.
  Default: `1 << 30` (1 GiB). Cap=0 disables the guard entirely (no limit).
- `config.Config.SMSMaxFileBytes int64` parsed in `internal/config/config.go::smsMaxFileBytes()`.
- `SMSCollector.maxFileBytes int64` field; `NewSMSCollector(sourceDir string, maxFileBytes int64)`.
- Guard condition: `c.maxFileBytes > 0 && info.Size() > c.maxFileBytes`.
- Applied to both `parseSMSFile` and `parseCallsFile`.
- Tests: `TestSMSCollector_OversizedFileSkipped` (explicit cap), `TestSMSCollector_UnderCapFileProcessed`,
  `TestSMSCollector_ZeroCapMeansUnlimited` (cap=0 → no skip).

**Gmail nil Payload deref** (`gmail.go:176`):
- Added `if msg.Payload == nil { slog.Warn; return error }` before header/body extraction.

**SMS partial-result contract** (`sms.go`):
- Changed from early `return nil, err` to `slog.Warn` + accumulate partial docs.
- Matches gmail/calendar/whisper pattern.

### LOW Bugs Fixed

**Whisper cloud-endpoint guard** (`whisper.go`):
- `isLocalWhisperEndpoint(rawURL)` checks: localhost/127.0.0.1/::1/host.docker.internal + RFC-1918 ranges.
- Called at start of `Collect`; emits `slog.Warn` if non-local. Does NOT hard-fail.

**Same-ms SourceID collision** (via body hash in SourceID format).

**msToUTC sub-second precision** (now uses `time.UnixMilli(ms).UTC()` — simpler and correct).

**latestFileByPrefix lexicographic selection** (`sms.go`):
- Primary sort: `e.Name() > latestName` (lexicographic greatest filename).
- Tiebreak: mtime when names are equal.

**FilesystemCollector.WithIndexedIDs** return type changed from `*FilesystemCollector` to `void`
to satisfy the new `IndexAwareCollector` interface. No callers used the return value.

### TDD Evidence

Two failing test files written first, then implementation:
- `sms_high_bugs_test.go` — compile-failed before implementation; all 6 tests pass after.
- `sms_medium_low_test.go` + `sms_high_bugs_test.go` — all pass.

### Files Changed

- `internal/collector/collector.go` — new `IndexAwareCollector` interface
- `internal/collector/sms.go` — all fixes; new methods `WithIndexedIDs`, `shouldEmitSMS`, `smsShortHash`, `smsBodyHash`
- `internal/collector/whisper.go` — `WithIndexedIDs`, `isLocalWhisperEndpoint`, cloud guard in `Collect`
- `internal/collector/filesystem.go` — `WithIndexedIDs` return type `void`
- `internal/collector/gmail.go` — nil Payload guard
- `internal/scheduler/scheduler.go` — generalized IndexAwareCollector usage
- `internal/collector/sms_test.go` — updated SourceID format expectations
- `internal/collector/sms_high_bugs_test.go` — new TDD tests for HIGH bugs
- `internal/collector/sms_medium_low_test.go` — new tests for MEDIUM/LOW bugs

**Why:** All changes in working tree per task spec (no commit).
**How to apply:** Run `go test ./... -count=1` to verify all green before committing.

---

## Issue #103: Empty Source File Observability (2026-06-09)

**Problem:** A 0-byte sms-*.xml / calls-*.xml (OneDrive bridge staging an empty placeholder) silently yielded 0 records at INFO level — operators couldn't distinguish real "no new records" from a broken/empty source.

**Fix:** Added a zero-byte guard immediately after `os.Stat` in both `parseSMSFile` and `parseCallsFile` (before the size-cap check). If `info.Size() == 0`, emit `slog.Warn("sms: source file is empty — possible OneDrive materialization/bridge failure; skipping", "file", path)` and return `nil, nil` to skip parsing.

- Placement: after `os.Stat` error check, before the `maxFileBytes` cap check.
- Partial-result contract preserved: empty SMS file skipped → valid calls docs still returned (and vice versa).
- The `syscall.Stat_t.Blocks` placeholder-detection was skipped (not portable to non-Unix — the size==0 guard is the sufficient deliverable).

**Tests added to `sms_medium_low_test.go`:**
- `TestSMSCollector_EmptySMSFile_SkippedGracefully` — 0-byte sms file → 0 docs, no error
- `TestSMSCollector_EmptyCallsFile_SkippedGracefully` — 0-byte calls file → 0 docs, no error
- `TestSMSCollector_EmptyBothFiles_SkippedGracefully` — both 0-byte → 0 docs, no error
- `TestSMSCollector_EmptySMSFile_ValidCallsStillCollected` — empty sms + valid calls → 1 call-log doc returned

**Files changed:** `internal/collector/sms.go`, `internal/collector/sms_medium_low_test.go`
