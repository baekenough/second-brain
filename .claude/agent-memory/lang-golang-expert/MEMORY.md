# lang-golang-expert Memory Index

- [project_second_brain.md](project_second_brain.md) — second-brain Go server architecture and already-fixed P0 bugs
- [project_calendar_multi_collector.md](project_calendar_multi_collector.md) — 캘린더 복수 캘린더 지원: freeBusyReader 빈콘텐츠 skip 패턴, source_id 네임스페이스 규칙, 단일 워터마크로 복수 하위소스 다루는 트레이드오프
- [project_whisper_transcription_ledger.md](project_whisper_transcription_ledger.md) — whisper infinite re-transcription fix: ledger table, authoritative index-skip, worker pool; do-not-run store DB tests
- [project_note_capture_enrichment.md](project_note_capture_enrichment.md) — note/insight pipeline: the three-budget deadline model + the four insight echo-chamber gates
- [project_search_rrf_relevance.md](project_search_rrf_relevance.md) — mergeRRF equal-weight fix (fixed) + unfixed HNSW ef_search relevance bug (follow-up needed)
- [project_temporal_window_retrieval.md](project_temporal_window_retrieval.md) — sort hints can't filter: occurred_at must be a per-lane WHERE; chunk lanes ignore the include source filter
- [project_query_planner.md](project_query_planner.md) — /ask QueryPlan: plan owns retrieval, Params owns ranking (direction derived, not planned); LLM_THINKING=disabled is a hard dependency
- [project_action_surface.md](project_action_surface.md) — /actions cards: constant summary + counterpart never resolved for awaiting_my_reply; rows self-heal via worker re-tick, no migration
- [project_llm_reasoning_budget.md](project_llm_reasoning_budget.md) — reasoning_content eats max_tokens → empty completions; measured option table, reasoning_effort=low rejected
- [project_govulncheck_pdf_removal.md](project_govulncheck_pdf_removal.md) — govulncheck history: pdf stage removal; otel must bump as a set; x/crypto/openpgp residual is permanent, not a to-do
- [feedback_never_log_raw_llm_output.md](feedback_never_log_raw_llm_output.md) — #194: log response length + shape label, never raw completion text or bodies in errors
- [feedback_test_doubles_must_fail.md](feedback_test_doubles_must_fail.md) — store fakes must inject errors on every write AND record ctx.Err(); two Criticals shipped green without it
- [feedback_tests_must_inject_prod_environment.md](feedback_tests_must_inject_prod_environment.md) — inject UTC (prod container TZ), not the dev machine's KST; use internal/timeutil.KST() for all calendar boundaries
- [feedback_prove_worker_liveness_before_regression_verdict.md](feedback_prove_worker_liveness_before_regression_verdict.md) — date rows via id-sequence + pg_stat_user_tables before blaming a deploy; a false regression cost a 447-row prod DELETE
- [feedback_no_blocking_remote_calls_in_write_path.md](feedback_no_blocking_remote_calls_in_write_path.md) — ingest handlers must never embed inline; same retransmit-loop outage happened twice
