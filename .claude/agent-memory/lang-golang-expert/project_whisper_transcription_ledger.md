---
name: whisper-transcription-ledger
description: Why whisper has a transcription_ledger table + authoritative index-skip + worker pool (infinite re-transcription loop fix)
metadata:
  type: project
---

The whisper collector had an infinite re-transcription loop: any audio source_id absent from the active document index was re-transcribed every ~60s cycle forever (pegged whisper at 380% CPU for 11 days). A source_id is absent when its doc was rejected by content-dedup (ErrDuplicateTranscript) or transcription failed.

**Fix (migration 020):** a `transcription_ledger` table (source_type, source_id, transcribed_at; PK both) decouples "already transcribed" from "active in search index".

- `store.TranscribedSourceIDSet` / `store.RecordTranscribed` (in `internal/store/transcription_ledger.go`). RecordTranscribed uses `unnest($2::text[]) ON CONFLICT DO NOTHING` — single round-trip, no per-id loop.
- scheduler `DocumentUpserter` interface gained both methods. `runCollector` loads `union(ActiveSourceIDSet, TranscribedSourceIDSet)` and passes via `WithIndexedIDs`. The old `&& !since.IsZero()` guard was REMOVED (it disabled dedup on first run).
- scheduler `processBatch` calls `RecordTranscribed` for the whole batch BEFORE the upsert loop (only for `model.SourceCallTranscript`), so duplicate-rejected files still get ledgered. Non-fatal on error.

**Whisper collector authoritative index-skip:** when `c.indexedIDs != nil` it is AUTHORITATIVE — known source_id → skip; absent → transcribe regardless of mtime. nil → mtime fallback. Audio is immutable. This replaced the broken `mtimeNew || notIndexed` logic.

**Worker pool + streaming incremental ledger:** WalkDir stays SEQUENTIAL for all filtering (keeps `failedQuarantine` map single-goroutine, no mutex). Files passing filters go into a `pendingTranscription` slice; Phase 2 transcribes via a pool of `c.concurrency()` (= `max(1, cfg.WhisperConcurrency)`, env `WHISPER_CONCURRENCY`, default 1). concurrency==1 == original sequential order.

WhisperCollector is now a `StreamingCollector`: `CollectStream(ctx, since, onBatch)` is the primary path; `Collect` is a thin accumulator wrapper around it (appends every batch). `streamPending` runs feeder + worker pool → unbuffered `results` chan → ONE drain goroutine that buffers `whisperStreamBatchSize`(=5) docs and calls `onBatch` once per batch + final flush. **emit MUST be single-goroutine** (scheduler `processBatch`/embedding not concurrency-safe). Leak-safe via a derived `streamCtx` with `defer cancel()`: workers select on `streamCtx.Done()` for both job dequeue and `results <- doc`, so early return (onBatch error / parent cancel) unblocks them; a closer goroutine `wg.Wait(); close(results)` (only close site). Scheduler auto-takes the streaming branch (`sc.CollectStream(ctx, since, processBatch)`) so each batch is ledgered live — progress survives a mid-drain restart. The `onBatchErr` sentinel + `isOnBatchError` live in sms.go (shared, same package); whisper just returns the onBatch error directly. Tests: `whisper_stream_test.go` (multi-batch, exactly-once via sync.Map/atomic, ctx-cancel, onBatch-error), `scheduler_stream_ledger_test.go` (per-batch RecordTranscribed). All pass under `-race`.

**Tailscale:** `whisperPrivateCIDRs` includes `100.64.0.0/10` (RFC 6598 CGNAT) so Tailscale 100.x endpoints count as local (issue #100).

**Testing gotcha:** do NOT run `go test ./internal/store/...` — live prod postgres (db `second_brain`) is reachable and store tests open real connections. Run only DB-free store tests by name (TestEmbeddingDimMigrationFilename, TestNeedsEmbeddingDimGUC). Collector/scheduler/config tests are DB-free and safe.
