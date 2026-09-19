-- Migration 033: unify call-log and call-transcript into a single 'call'
-- source_type — one document per phone call, recorded or not.
--
-- Background: model.SourceCall's doc comment (internal/model/document.go).
-- User decision: "통화(call-log)와 전사(call-transcript)를 합치자. 별개일
-- 이유가 없다." Historically every recorded call could produce TWO
-- documents:
--   - call-log:{dateMs}:{numHash}:{durHash}  — created by smsmap.MapCall /
--     internal/api/ingest_recording.go, short summary content.
--   - transcript:{relPath}                    — created by WhisperCollector,
--     full transcript content, with no link back to the call-log document.
-- Both source_type values are deprecated as of this migration — see
-- model.SourceCallLog's doc comment. New code already reads/writes
-- source_type='call':
--   - internal/collector/smsmap/smsmap.go's MapCall
--   - internal/api/ingest_recording.go's ingestRecordingHandler
--   - internal/collector/whisper.go's WhisperCollector (merges into the
--     existing call document via callLogMergeSourceID when a recording
--     sidecar identifies the call, or keeps a standalone transcript:{path}
--     document otherwise)
--   - internal/store/document.go's DocumentStore.AttachTranscript (the live,
--     ongoing counterpart of what this migration does once, historically)
--
-- This migration is a ONE-TIME BACKFILL for documents that predate that code
-- change, including the 3,381 call-transcript / 2,002 call-log documents
-- migration 027 already normalized from source_type='secretary' (see
-- migrations/027_secretary_source_normalization.sql).
--
-- Idempotency: every statement below is scoped to
-- source_type IN ('call-log', 'call-transcript'), and the final step (6)
-- renames every surviving row of those types to 'call'. A second run
-- therefore finds zero matching rows in every step and is a true no-op.
-- This property is load-bearing, not cosmetic: RunMigrations re-executes
-- every .sql file under migrations/ on EVERY server boot (see
-- internal/store/postgres.go — there is no migration-tracking table), so a
-- migration that was not naturally idempotent would re-run its side effects
-- (or its RAISE EXCEPTION safety guard) on every single restart.
--
-- IMPORTANT — back up before deploying this migration:
--   pg_dump -t documents -t chunks "$DATABASE_URL" > pre-033-backup.sql
-- (see deploy/ubuntu1-stack/README.md's deploy checklist, updated alongside
-- this migration). Every destructive step below is a SOFT delete
-- (status='deleted', never a physical DELETE on `documents` itself — only on
-- the narrow join-table/duplicate rows described in step 4/5), but a
-- table-level backup is the only way to inspect the true pre-migration state
-- if something about the pairing heuristic below turns out to be wrong.
--
-- Pairing heuristic (steps 1-2): a call-log document and a call-transcript
-- document describe the SAME call when the call-log's
-- metadata->>'audio_file' (the on-disk filename internal/api/
-- ingest_recording.go wrote) equals the basename of the call-transcript's
-- metadata->>'relative_path' (WhisperCollector's WalkDir-relative path — may
-- differ in leading directory components from audio_file, but never in
-- basename, since WhisperCollector scans the exact directory
-- ingest_recording.go wrote into).
--
-- Two-pass tie-break (handles both directions of duplication):
--   Pass 1 (best_log_per_tr) — for each call-transcript document, keep only
--     its most-recently-updated matching call-log document. Handles a
--     transcript that could join to more than one call-log row.
--   Pass 2 (best_tr_per_log) — from that already-deduplicated mapping, for
--     each call-log document keep only its most-recently-updated matching
--     transcript. Handles the reverse: one call-log row whose recording was
--     transcribed more than once (e.g. after a file rename that
--     recordingbackfill re-keyed, or a whisper re-run before the
--     transcription ledger existed).
-- A transcript that is not chosen in the final pass is left completely
-- untouched: it becomes its own independent 'call' document after step 6,
-- rather than being force-merged or deleted. This is the safe fallback for
-- "N:1" duplication — you cannot losslessly combine two distinct
-- transcripts' full text into one `content` column, so leaving the loser as
-- its own searchable document is strictly better than deleting real content.
--
-- Known un-handled duplicate (documented, not fixed by this migration): a
-- call-log document created by a failed app-side recording/call-log link
-- (a duplicate call logged with duration_seconds=0 and NO
-- metadata.audio_file at all — see agent memory
-- project_call_recording_ingest_outage) cannot be matched by the audio_file
-- join above, because it never had an audio_file to match with. Merging it
-- would require a heuristic (same dateMs±60s window, same addrHash) that
-- risks silently combining two DISTINCT real calls placed from the same
-- number within a minute of each other — a false merge destroys information
-- a leftover duplicate row does not, so this migration leaves any such rows
-- as separate 'call' documents after step 6. If they need closing, do it as
-- its own reviewed follow-up migration with a human looking at the
-- candidate pairs before they are merged.

DO $$
DECLARE
    v_paired       bigint := 0;
    v_soft_deleted bigint := 0;
    v_retyped      bigint := 0;
    v_ledger_moved bigint := 0;
    -- ~2,002 + 3,381 = 5,383 is the full migration-027 volume for these two
    -- kinds; the number of PAIRS (soft-deletes) this migration can produce is
    -- bounded by min(#call-log, #call-transcript) plus anything ingested
    -- since — 6,000 leaves generous headroom over that ceiling while still
    -- catching a structurally wrong join (e.g. a basename match that is far
    -- too permissive) before it soft-deletes most of the corpus.
    v_threshold    CONSTANT bigint := 6000;
BEGIN

    -- Step 1: build the candidate pairing set (audio_file <-> relative_path
    -- basename match, both sides still 'active'), then collapse it to at
    -- most one (log_id, tr_id) pair per log_id AND per tr_id — see the
    -- two-pass tie-break explained in the header comment above.
    CREATE TEMP TABLE call_unify_pairs ON COMMIT DROP AS
    WITH candidates AS (
        SELECT
            log.id         AS log_id,
            log.updated_at AS log_updated_at,
            tr.id          AS tr_id,
            tr.source_id   AS tr_source_id,
            tr.updated_at  AS tr_updated_at
        FROM documents log
        JOIN documents tr
          ON tr.source_type = 'call-transcript'
         AND tr.status      = 'active'
         AND regexp_replace(tr.metadata ->> 'relative_path', '^.*/', '')
             = (log.metadata ->> 'audio_file')
        WHERE log.source_type = 'call-log'
          AND log.status      = 'active'
          AND log.metadata  ?  'audio_file'
          AND tr.metadata   ?  'relative_path'
    ),
    best_log_per_tr AS (
        -- One call-log winner per transcript: the most recently updated
        -- match, tie-broken by log_id for determinism. tr_updated_at is
        -- carried through (not just tr_id/tr_source_id) because the next
        -- CTE's ORDER BY needs it.
        SELECT DISTINCT ON (tr_id) log_id, tr_id, tr_source_id, tr_updated_at
        FROM candidates
        ORDER BY tr_id, log_updated_at DESC, log_id
    ),
    best_tr_per_log AS (
        -- One transcript winner per (already-deduplicated) call-log: the
        -- most recently updated match, tie-broken by tr_id for determinism.
        SELECT DISTINCT ON (log_id) log_id, tr_id, tr_source_id
        FROM best_log_per_tr
        ORDER BY log_id, tr_updated_at DESC, tr_id
    )
    SELECT log_id, tr_id, tr_source_id
    FROM best_tr_per_log;

    GET DIAGNOSTICS v_paired = ROW_COUNT;

    IF v_paired > v_threshold THEN
        RAISE EXCEPTION 'call unify aborted: % candidate pairs exceeds safety threshold % — review the audio_file/relative_path join before re-running', v_paired, v_threshold;
    END IF;

    -- Step 2: reassign chunks from the transcript document to the surviving
    -- call-log document. The winner's OWN existing chunks are deleted first
    -- — its content is about to be fully replaced by the transcript's
    -- content (step 3), exactly as store.AttachTranscript's live merge path
    -- does, and internal/scheduler's persistChunks will regenerate the
    -- correct chunk set from the merged content on the next collection
    -- cycle. Deleting first also avoids a chunks.UNIQUE(document_id,
    -- chunk_index) collision between the winner's old chunk_index values and
    -- the transcript's incoming ones.
    DELETE FROM chunks
    WHERE document_id IN (SELECT log_id FROM call_unify_pairs);

    UPDATE chunks c
    SET document_id = p.log_id
    FROM call_unify_pairs p
    WHERE c.document_id = p.tr_id;

    -- Step 3: copy the transcript's content/embeddings onto the surviving
    -- call-log document. metadata is MERGED (transcript's `||` operand wins
    -- on key collision — jsonb `||` is right-biased), matching
    -- store.AttachTranscript's live semantics exactly, except that the
    -- call-log-authoritative fields (contact_name/direction/
    -- duration_seconds/number) are explicitly re-asserted from the
    -- PRE-MERGE call-log metadata afterward, in case the transcript's own
    -- sidecar-derived copy of those same keys is stale or absent — the
    -- call-log side is always the source of truth for them, never the
    -- transcript. transcript_source_id + transcription="done" are stamped
    -- so store.TranscribedSourceIDSet's ledger union (see that function's
    -- doc comment) recognises this call as already transcribed.
    UPDATE documents d
    SET content           = tr.content,
        embedding         = COALESCE(tr.embedding, d.embedding),
        title_summary     = COALESCE(tr.title_summary, d.title_summary),
        bullet_summary    = COALESCE(tr.bullet_summary, d.bullet_summary),
        summary_embedding = COALESCE(tr.summary_embedding, d.summary_embedding),
        metadata          = (d.metadata || tr.metadata || jsonb_build_object(
                                 'transcript_source_id', tr.source_id,
                                 'transcription', 'done'
                             )) || jsonb_strip_nulls(jsonb_build_object(
                                 'contact_name',     d.metadata -> 'contact_name',
                                 'direction',         d.metadata -> 'direction',
                                 'duration_seconds',  d.metadata -> 'duration_seconds',
                                 'number',            d.metadata -> 'number'
                             )),
        occurred_at       = COALESCE(d.occurred_at, tr.occurred_at),
        updated_at        = now()
    FROM call_unify_pairs p
    JOIN documents tr ON tr.id = p.tr_id
    WHERE d.id = p.log_id;

    -- Step 4: reassign FK references from the losing transcript document to
    -- the surviving call-log document, for every table that references
    -- documents.id (chunks was already handled in step 2). A reassignment
    -- that would violate a UNIQUE constraint (the winner already has its own
    -- row for the same key) is skipped rather than attempted — that row's
    -- information is superseded by the winner's own equivalent row, not
    -- lost — and the untouched remainder is deleted immediately after so it
    -- does not end up dangling on a document step 5 is about to soft-delete
    -- (these tables' FKs are ON DELETE CASCADE, but a soft-delete does not
    -- trigger a physical DELETE, so CASCADE never fires here).

    -- actions.document_id has no UNIQUE constraint on document_id alone
    -- (identity_key is the table's unique column) — a plain reassignment
    -- cannot conflict.
    UPDATE actions a
    SET document_id = p.log_id
    FROM call_unify_pairs p
    WHERE a.document_id = p.tr_id;

    -- feedback.document_id: no table-wide UNIQUE, but migration 025's
    -- PARTIAL unique index uq_feedback_ask_evidence
    -- (session_id, md5(lower(btrim(query))), document_id)
    -- WHERE source = 'ask_evidence' can conflict for that source only.
    UPDATE feedback f
    SET document_id = p.log_id
    FROM call_unify_pairs p
    WHERE f.document_id = p.tr_id
      AND NOT (
          f.source = 'ask_evidence' AND EXISTS (
              SELECT 1 FROM feedback f2
              WHERE f2.source = 'ask_evidence'
                AND f2.document_id = p.log_id
                AND f2.session_id IS NOT DISTINCT FROM f.session_id
                AND md5(lower(btrim(COALESCE(f2.query, '')))) = md5(lower(btrim(COALESCE(f.query, ''))))
          )
      );
    DELETE FROM feedback f
    USING call_unify_pairs p
    WHERE f.document_id = p.tr_id;

    -- golden_judgments.document_id: UNIQUE(query_id, document_id, judge)
    -- (migration 031).
    UPDATE golden_judgments g
    SET document_id = p.log_id
    FROM call_unify_pairs p
    WHERE g.document_id = p.tr_id
      AND NOT EXISTS (
          SELECT 1 FROM golden_judgments g2
          WHERE g2.document_id = p.log_id
            AND g2.query_id    = g.query_id
            AND g2.judge       = g.judge
      );
    DELETE FROM golden_judgments g
    USING call_unify_pairs p
    WHERE g.document_id = p.tr_id;

    -- document_entities: PK(document_id, entity_id) (migration 017) — the
    -- standard insert-then-delete merge pattern for a composite-key join
    -- table (ON CONFLICT DO NOTHING handles a relation the winner already
    -- carries).
    INSERT INTO document_entities (document_id, entity_id)
    SELECT p.log_id, de.entity_id
    FROM document_entities de
    JOIN call_unify_pairs p ON p.tr_id = de.document_id
    ON CONFLICT (document_id, entity_id) DO NOTHING;
    DELETE FROM document_entities de
    USING call_unify_pairs p
    WHERE de.document_id = p.tr_id;

    -- entity_relations.evidence_document_id: UNIQUE(from_entity_id,
    -- to_entity_id, type, evidence_document_id) (migration 021).
    UPDATE entity_relations er
    SET evidence_document_id = p.log_id
    FROM call_unify_pairs p
    WHERE er.evidence_document_id = p.tr_id
      AND NOT EXISTS (
          SELECT 1 FROM entity_relations er2
          WHERE er2.evidence_document_id = p.log_id
            AND er2.from_entity_id        = er.from_entity_id
            AND er2.to_entity_id          = er.to_entity_id
            AND er2.type                  = er.type
      );
    DELETE FROM entity_relations er
    USING call_unify_pairs p
    WHERE er.evidence_document_id = p.tr_id;

    -- Step 5: soft-delete the losing transcript document. Never a hard
    -- DELETE — content and history stay recoverable (set status='active',
    -- deleted_at=NULL) exactly like every other soft-delete path in this
    -- codebase (store.SoftDeleteBySourceID, migration 019's collision
    -- losers). metadata.merged_into records the winner's id for
    -- traceability/audit.
    UPDATE documents d
    SET status     = 'deleted',
        deleted_at = now(),
        metadata   = d.metadata || jsonb_build_object('merged_into', p.log_id::text),
        updated_at = now()
    FROM call_unify_pairs p
    WHERE d.id = p.tr_id;

    GET DIAGNOSTICS v_soft_deleted = ROW_COUNT;

    -- Step 6: rename every surviving call-log/call-transcript document to
    -- source_type='call' — merged winners, unmerged standalones, AND the
    -- just-soft-deleted losers alike (deliberately no status filter here,
    -- mirroring migration 027's own rename step: a soft-deleted document's
    -- source_type should reflect the same rename as everything else, so a
    -- future `status='active'` restore does not resurrect a stale
    -- source_type). metadata.legacy_source_type preserves the pre-migration
    -- value for reversibility/audit, matching migration 027's convention.
    --
    -- transcription is backfilled where step 3 has not already set it:
    --   - already has a non-empty metadata.transcription value (stamped as
    --     'done' by step 3 for a merge winner, or already 'pending' on one
    --     of the ~608 known PENDING-placeholder call-log documents created
    --     by the pre-fix internal/api/ingest_recording.go — see that file's
    --     content-format change in this same commit) → leave it alone.
    --   - a lone call-transcript document (never paired with a call-log,
    --     e.g. a standalone voice-memo-style transcript) → 'done': it
    --     already IS a transcript, nothing about it is pending.
    --   - anything else (a lone call-log document with no transcription key
    --     at all) → 'none': no recording was ever associated with it.
    --
    -- content is rebuilt for the known PENDING-placeholder rows into the
    -- same 4-line summary format smsmap.MapCall now produces, so the
    -- "[TRANSCRIPTION PENDING]" placeholder text stops polluting FTS/search
    -- for calls that will never actually get transcribed (the audio file no
    -- longer exists, or the pairing above did not find a match for it). A
    -- byte-for-byte match with smsmap.MapCall's Go-side formatting is not
    -- required here — this is a display/search-quality backfill, not an
    -- identity or dedup key — but the shape (contact/direction/time/
    -- duration, one per line) is the same.
    UPDATE documents d
    SET source_type = 'call',
        content     = CASE
                          WHEN d.content LIKE '%[TRANSCRIPTION PENDING]%'
                          THEN format(E'상대방: %s\n통화 방향: %s\n시각: %s\n통화 시간: %ss',
                                      COALESCE(NULLIF(d.metadata ->> 'contact_name', ''),
                                               d.metadata ->> 'number',
                                               '상대'),
                                      COALESCE(d.metadata ->> 'direction', 'incoming'),
                                      to_char(COALESCE(d.occurred_at, d.collected_at) AT TIME ZONE 'UTC',
                                              'YYYY-MM-DD HH24:MI:SS') || ' UTC',
                                      COALESCE(d.metadata ->> 'duration_seconds', '0'))
                          ELSE d.content
                      END,
        metadata    = d.metadata
                      || jsonb_build_object('legacy_source_type', d.source_type)
                      || CASE
                             WHEN COALESCE(d.metadata ->> 'transcription', '') <> '' THEN '{}'::jsonb
                             WHEN d.source_type = 'call-transcript' THEN jsonb_build_object('transcription', 'done')
                             ELSE jsonb_build_object('transcription', 'none')
                         END,
        updated_at  = now()
    WHERE d.source_type IN ('call-log', 'call-transcript');

    GET DIAGNOSTICS v_retyped = ROW_COUNT;

    -- Step 7: migrate the transcription_ledger (migration 020) alongside the
    -- documents it tracks, so store.TranscribedSourceIDSet's first UNION arm
    -- (`WHERE source_type = $1` against the ledger) keeps matching once
    -- callers query it with sourceType='call' instead of 'call-transcript'.
    --
    -- 7a. Rename existing ledger rows in place.
    INSERT INTO transcription_ledger (source_type, source_id, transcribed_at)
    SELECT 'call', source_id, transcribed_at
    FROM transcription_ledger
    WHERE source_type = 'call-transcript'
    ON CONFLICT (source_type, source_id) DO NOTHING;

    DELETE FROM transcription_ledger WHERE source_type = 'call-transcript';

    -- 7b. Defense-in-depth: also ledger the RAW audio identity
    -- (tr_source_id, "transcript:{relPath}") of every transcript this
    -- migration just merged into a call-log document. This is technically
    -- redundant with store.TranscribedSourceIDSet's second UNION arm (which
    -- already finds these documents via their now-live
    -- metadata->>'transcript_source_id'), but it survives even if the
    -- merged document is later soft-deleted for an unrelated reason (mass-
    -- delete guard, manual cleanup) — the underlying audio file is
    -- immutable and must never be re-transcribed regardless of what later
    -- happens to the document that represents it.
    INSERT INTO transcription_ledger (source_type, source_id, transcribed_at)
    SELECT 'call', tr_source_id, now()
    FROM call_unify_pairs
    ON CONFLICT (source_type, source_id) DO NOTHING;

    GET DIAGNOSTICS v_ledger_moved = ROW_COUNT;

    RAISE NOTICE 'call unify: % pairs merged (% chunks/FKs reassigned, % transcript documents soft-deleted), % documents retyped to call, % ledger rows added in step 7b',
        v_paired, v_paired, v_soft_deleted, v_retyped, v_ledger_moved;

END $$;
