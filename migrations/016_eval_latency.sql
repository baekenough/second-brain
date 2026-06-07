-- Migration 016: add read-path (search) latency profiling columns to eval_metrics.
-- All columns are nullable so existing rows are unaffected.
-- No destructive changes (no DROP, no NOT NULL, no DEFAULT).

ALTER TABLE eval_metrics ADD COLUMN IF NOT EXISTS search_latency_p50_ms  DOUBLE PRECISION;
ALTER TABLE eval_metrics ADD COLUMN IF NOT EXISTS search_latency_p95_ms  DOUBLE PRECISION;
ALTER TABLE eval_metrics ADD COLUMN IF NOT EXISTS search_latency_mean_ms DOUBLE PRECISION;
