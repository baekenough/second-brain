# be-go-backend-expert Memory Index

- [project_second_brain.md](project_second_brain.md) — second-brain Go backend project structure, collector patterns, store interfaces
- [feedback_discord_collector.md](feedback_discord_collector.md) — Discord collector attachment pipeline design decisions
- [project_embedding_dim_wiring.md](project_embedding_dim_wiring.md) — Issue #50: EMBEDDING_DIM → migration 011 wiring via RunMigrations(embeddingDim) + SET LOCAL in dedicated conn/tx
- [project_refetcher_issue72.md](project_refetcher_issue72.md) — Issue #72: Refetcher interface for remote-file extraction retry (URLRefetcher, discord FilePath=att.URL)
- [project_collector_data_integrity_fixes.md](project_collector_data_integrity_fixes.md) — HIGH/MEDIUM/LOW data-integrity and privacy fixes in SMS/Whisper/Gmail collectors (2026-06-09)
- [project_cutover_floor.md](project_cutover_floor.md) — COLLECTOR_CUTOVER env var: config-driven floor for IndexAware collectors; CutoverAwareCollector interface; scheduler WithCutover builder
- [project_sms_streaming_issue102.md](project_sms_streaming_issue102.md) — Issue #102: SMSCollector CollectStream — buffered-read + bounded-batch-emit (smsStreamBatchSize=500); onBatchErr sentinel; FUSE safety preserved
- [project_whisper_filename_cutover.md](project_whisper_filename_cutover.md) — Issue #110: recordingTime() parses VoiceRecorder/TPhone filename dates for cutover floor; mtime fallback for unparseable names
