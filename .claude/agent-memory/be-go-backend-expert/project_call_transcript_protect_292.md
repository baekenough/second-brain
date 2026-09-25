---
name: project-call-transcript-protect-292
description: "#292 PR-B: how call transcripts are protected in upsert (CASE condition, metadata patch and why), the recording-endpoint status contract with the phone app, the ReadTimeout poison trap, the soft-delete resurrection survey"
metadata:
  type: project
---

#292 PR-B (2026-09-25, branch feature/v0.25.3-b, on top of PR-A #290).

**Transcript protection (store/document.go callTranscriptKeptSQL).** The condition has three parts. The existing row is source_type='call'. The existing row's transcription is not none or pending; a missing key counts as transcribed. The incoming row's transcription is none or pending, which means it is a call-log or recording summary. Both Upsert and UpsertTracked share the same SET fragment (callTranscriptUpsertSetSQL).
- Metadata while protected = the whole existing row || only the call-log-owned keys from the incoming row (contact_name/direction/duration_seconds/number). It is deliberately not a blacklist of transcript keys: migration 033 merged legacy transcript metadata wholesale, so the key set is open-ended. pii_name_redacted is excluded, because the content was not replaced.
- content_changed is now computed against the post-update `documents.content` in RETURNING, not against $4. Comparing against $4 keeps the row but still re-chunks it with the summary (verified by fault injection D2).
- A standalone WhisperCollector transcript (no transcription key on either side) is not protected. The incoming-side condition is what excludes it.

**Recording endpoint contract (the phone app, Uploader.handleRecordingResponse):** 2xx (accepted or skipped) and any 4xx other than 401/403 mean "sent, never retried". 5xx means break out of the recording loop and retry next wake (20 min). So transient errors must return 503, and deterministic outcomes must return 2xx or 4xx. A 5xx on a deterministic error is a poison that blocks every later recording.
- ErrDuplicateTranscript returns 200 {accepted:true, skipped:true, reason:duplicate_content}. The audio is already on disk and whisper will transcribe it.
- Permanent SQLSTATE returns 422.
- Multipart parse errors: readErrRecorder (from PR-A) catches transport errors → 503. fs errors → 503. Everything else defaults to 400; here the default is deliberately not transient, because stdlib parse errors are all malformed-input errors.
- **Trap:** the server ReadTimeout is 15s and covers the request body. Turning timeouts into 503 without also extending the handler deadline makes slow uploads a poison. Fix: http.ResponseController extends the read deadline to 200s (longer than the app's 180s callTimeout). The chi requestLogger wrapper supports Unwrap; this is verified by a real httptest server test.
- The sidecar is written before the audio, and both are written temp-then-rename (.partial is ignored by whisper). A truncated or sidecar-less audio file would get ledgered, and a later retry could not fix it.
- Never log filenames or paths. The raw number is in the stored filename when hashing is off, and *fs.PathError.Error() contains the path. Log only err_type/op/errno.

**Soft-delete resurrection (R2, report only, no code change):** Upsert, UpsertTracked and AttachTranscript all set status='active'. The writers of status='deleted' are: filesystem MarkDeleted (#135 guard), calendar cancellation SoftDeleteBySourceID, migration 030, notes DELETE plus the insight cascade, migration 019 SMS rekey losers, and migration 033 call-unify losers. There is no retention deleter. Notes can be resurrected through a client-supplied source_id. The orchestrator is splitting this into its own issue.

**deep-verify follow-ups (same day):**
- Protection at the store layer alone is not enough. Every caller that rebuilds chunks from the *incoming* doc must honor contentChanged. The scheduler processBatch used plain Upsert and always ran persistChunks, so the SMS XML collector recollected a transcribed call and wrote summary chunks under the transcript row. Fix: DocumentUpserter gained UpsertTracked. processBatch (both the merge and non-merge paths) skips chunks, chunk embedding and entity extraction when the content is unchanged. The EntityWorker backfill covers entities. ForceCollectSlackChannel still uses Upsert and re-chunks unconditionally, on purpose.
- The overlay of call-log-authoritative keys and title applies only when the incoming transcription is 'none'. On 'pending' (recording re-upload), keep everything: the handler hardcodes direction=incoming and may send an empty contact_name.
- Non-object existing metadata (SQL NULL / jsonb null / arrays): the protection condition requires jsonb_typeof='object'. upsertMetadataMergeSQL reads existingMetadataObjectSQL, because jsonb_each on a non-object errors.
- With an empty API_KEY, requireAPIKey passes everything through. Do not extend the recording deadline in that case, and warn at startup.
- Filename bounds: the voice-memo stem is at most 100B plus a hash suffix when truncated (to avoid a shared-prefix collision). Extensions are whitelisted (whisper set, otherwise .audio, keeping the original case). A raw number longer than 32B is replaced by numHash. writeFileAtomic temp names add about 20-30B, and ENAMETOOLONG → 503 would be a deterministic poison. Stale .partial files are cleaned up at startup (mtime > 1h).
- Fault injections must keep variables used, or they BUILD-FAIL and count as a false "caught". Check for BUILD-FAIL in the output.
- The local image second-brain-postgres:test can vanish between sessions (another agent removed it). Rebuild with `docker build -t … deploy/postgres`. Scheduler RealDB tests exist now (they skip without TEST_DATABASE_URL, and CI's DB job only runs store/api).

**Test-infra gotcha:** running `go test ./...` with TEST_DATABASE_URL and parallel packages flakes TestChunkSparseContext_* in the store package, because it fetches with a global limit while the api package writes chunks concurrently. CI runs store and api as separate steps. Locally, use -p 1 (3446 pass).

**How to apply:** For call-document upsert changes, keep the protection fragment shared. For the ingest status codes, derive from the phone app's per-code behavior first. Related: [[project-ingest-messages-contract-290]].
