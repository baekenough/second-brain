---
name: project-ingest-messages-contract-290
description: "#290 PR-A: the ingest/messages status code is the phone's resend signal (201 advances the cursor, 503 resends); error classification, upsert and chunk replacement in one transaction, real-DB fault injection methods, ci-checks [7/7] Hangul rule"
metadata:
  type: project
---

#290 PR-A (2026-09-25, feature/v0.25.3). The phone app (Uploader.kt) ignores errors[] on any 2xx and advances its cursor. It retries on 5xx and on 4xx other than 401/403. So the status code of POST /api/v1/ingest/messages is what decides whether a record is lost or resent. A 4xx for the whole batch is a poison batch.

Decisions that are hard to see in the code alone:
- Error classes live in internal/api/ingest_errors.go. Transient (503 + Retry-After 30) is the default. Only SQLSTATE 22, 23 and 54 and handler validation errors are Permanent (201 + errors[]). store.ErrDuplicateTranscript counts as skipped, because treating it as transient would make it a poison record. When the budget ctx is done, the class is always Transient, whatever the error chain says.
- store.DocumentStore.UpsertTrackedWithChunks runs the upsert and the chunk replacement in one transaction. If they are separate, a chunk failure followed by a resend takes the contentChanged=false path, and the document stays chunkless forever. The defect was reproduced on the pre-fix code.
- Follow-up to the deep-verify review: not all of SQLSTATE class 23 is Permanent. Only 23502 and 23514 are. 23505 (chunks_document_id_chunk_index_key) comes only from a race where another writer replaces the same document's chunks at the same time. Reason: ON CONFLICT DO UPDATE takes only FOR NO KEY UPDATE, which does not conflict with the FK's KEY SHARE, so the two writers do not block each other. Fix: ChunkStore.ReplaceDocument now locks the document row with `SELECT 1 FROM documents WHERE id=$1 FOR UPDATE` before deleting chunks. Lock order is always document row, then chunk rows. The chunk-embedding backfill never takes the document lock, so there is no deadlock.
- Skip reasons are logged as a map of reason code to count (error_reasons/skip_reasons). When accepted==0 and errors>0, it logs ERROR, because that pattern signals contract drift. The Kotlin ApiModels.kt field names are pinned by a test that parses the source file (it FAILs if the path changes). sanitized counts only records that were stored successfully.
- Inline embedding was removed from the request path (decision D2). The collector backfill picks up the new rows through ListChunksNeedingEmbedding and ListDocumentsNeedingEmbedding.
- Separating a truncated upload (503) from truncated JSON (400): json.Decoder returns io.ErrUnexpectedEOF in both cases. The fix wraps the body in a readErrRecorder that stores the underlying Read error.
- Array elements are decoded as json.RawMessage, one record at a time. A type error in one element then skips only that record.

**Real-DB fault injection methods (reusable):**
- Add a BEFORE row trigger that calls pg_sleep only for marker rows. From a separate connection, find the backend with `wait_event='PgSleep' AND query ILIKE '%table%'` and call pg_terminate_backend on it. pgx CopyFrom (COPY) also fires row triggers, so this works during the chunk step as well.
- To test budget expiry, take `LOCK TABLE chunks IN ACCESS EXCLUSIVE MODE` from another connection. Send the request from a goroutine and bound the wait. Otherwise, when the budget is missing, the test hangs instead of failing.
- For a permanent error, add `CHECK (...) NOT VALID` to documents and drop it in Cleanup.
- Local DB: `docker build -t second-brain-postgres:test deploy/postgres`, then run it on 127.0.0.1:15490. This is the same image CI uses; it includes pg_bigm, so the full migrations apply.

**Verification pitfalls:**
- scripts/ci-checks.sh [7/7] warns on any added comment line without Hangul. That includes code-like lines inside comments (e.g. "//	503 + Retry-After"). The requirement is 0 warnings.
- The package has older gofmt-unformatted files (document.go, ingest_file.go, ...). Run gofmt -l only on changed files.
- slog capture tests (slog.SetDefault) must not be parallel. Parallel tests run after the serial tests finish, so the capture is safe.

**Why:** PR-B (call transcript protection, recording path) reuses the same classifier and store interface.
**How to apply:** For PR-B, reuse classifyIngestErr and UpsertTrackedWithChunks. Related: [[project-search-concurrency-gate-286]], [[store-sql-test-harness]].
