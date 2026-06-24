-- Migration 020: Transcription ledger — durable record of every audio source_id
-- the whisper pipeline has already transcribed.
--
-- Background (infinite re-transcription loop):
--   The whisper collector re-transcribed any audio file whose source_id was
--   absent from the active document index on every collection cycle (~60s),
--   forever. A source_id is absent whenever its document was not stored:
--     (a) ErrDuplicateTranscript — content-based dedup rejects distinct calls
--         whose transcripts collide (empty / short / carrier-message audio), or
--     (b) transcription failure.
--   Because the expensive transcription runs BEFORE the cheap dedup, and the
--   fact "this file was already transcribed" was never persisted, the same files
--   were transcribed every cycle (pegging the whisper container at ~380% CPU).
--
-- Fix:
--   This table decouples "already transcribed" (this ledger) from "active in the
--   search index" (documents). The scheduler records EVERY successfully
--   transcribed source_id here, regardless of whether the resulting document was
--   stored or rejected as a duplicate. The whisper collector then treats the
--   UNION of (active index ∪ ledger) as the authoritative "do NOT re-transcribe"
--   set. Audio files are immutable, so an already-transcribed source_id must
--   never be re-transcribed.
--
-- Idempotent: CREATE TABLE IF NOT EXISTS — safe to re-run.

CREATE TABLE IF NOT EXISTS transcription_ledger (
    source_type    TEXT        NOT NULL,
    source_id      TEXT        NOT NULL,
    transcribed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (source_type, source_id)
);
