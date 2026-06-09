---
name: project_sms_streaming_issue102
description: Issue #102: SMSCollector CollectStream implementation — buffered-read + bounded-batch-emit approach
metadata:
  type: project
---

Issue #102: Add `StreamingCollector` to `SMSCollector` to prevent OOM on large cumulative XML exports (~300MB+, ~1M records).

**Why:** The OOM is caused by unbounded `[]model.Document` accumulation (1M structs), not the raw XML bytes (~300MB). The streaming fix bounds document accumulation to 500 at a time.

**Design decision (buffered-read + bounded-emit):** Raw XML bytes are still read into `[]byte` via `readFileWithRetry` (required for OneDrive FUSE deadlock safety — EDEADLK/ETIMEDOUT retry semantics). The XML decoder then parses from `bytes.Reader`. Documents are emitted in batches of `smsStreamBatchSize = 500` via `onBatch`, then the batch is reset. This is documented in the code comments.

**Key implementation details:**
- `CollectStream(ctx, since, onBatch)` → calls `streamSMSFile` + `streamCallsFile`
- `onBatchErr` sentinel wrapper distinguishes onBatch errors from internal parse/IO errors
- `isOnBatchError(err)` → `errors.As(err, &*onBatchErr{})` for error routing
- Partial-result contract: file parse failures → `slog.Warn`, continue to next file; onBatch errors propagate immediately
- All existing guards preserved: empty-file (stat size==0), maxFileBytes, FUSE retry, ctx.Err() check per token
- `streamSMSFile` and `streamCallsFile` are direct parallel implementations of `parseSMSFile`/`parseCallsFile` with the emit loop replaced by batch accumulation

**Files changed:**
- `internal/collector/sms.go` — added `CollectStream`, `streamSMSFile`, `streamCallsFile`, `smsStreamBatchSize`, `onBatchErr`, `isOnBatchError`
- `internal/collector/sms_stream_test.go` — 14 new tests covering multi-batch splitting, filter parity, HIGH#1/HIGH#2, guards
- `internal/collector/sms_medium_low_test.go` — added `var _ StreamingCollector = (*SMSCollector)(nil)` compile-time check

**How to apply:** When adding `StreamingCollector` to another XML-based collector using FUSE-safe readFileWithRetry, use the same pattern: read bytes → bounded emit loop with onBatchErr sentinel.

See [[project_collector_data_integrity_fixes]] for the HIGH/MEDIUM/LOW fixes that must be preserved in any future streaming refactor.
