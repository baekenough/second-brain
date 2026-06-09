---
name: project_whisper_filename_cutover
description: Issue #110: Whisper cutover floor uses filename-parsed recording time instead of mtime to correctly skip staged historical audio files
metadata:
  type: project
---

Issue #110 fixed the WhisperCollector cutover floor bug.

**Why:** Staged audio files have recent mtimes (copy time ≠ recording time). A historical
recording from 2026-01-20 staged on 2026-06-09 would pass a 2026-05-30 cutover check
when using mtime — causing re-transcription of historical files.

**Fix:** Added `recordingTime(filename, mtime) time.Time` helper that parses recording
timestamps from filenames:
- Pattern A (Voice Recorder): `<label>_YYMMDD_HHMMSS.<ext>` — 2-digit year +2000
- Pattern B (TPhoneCallRecords): `<number>_YYYYMMDDHHMMSS[-N].<ext>` — 4-digit year, optional suffix

Times parsed as `time.Local`. Falls back to mtime if no pattern matches.

The cutover check was changed from:
  `info.ModTime().Before(c.cutover)` → `recordingTime(d.Name(), info.ModTime()).Before(c.cutover)`

`OccurredAt` in the Document still uses mtime (conservative — only changed what was needed).

**How to apply:** When other collectors have staged-file mtime problems, the same
`recordingTime` helper pattern applies. Keep fallback-to-mtime so unparseable filenames
degrade gracefully.

Key file: `internal/collector/whisper.go`
New test file: `internal/collector/whisper_recording_time_test.go`
