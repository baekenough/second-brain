-- Preserve legacy rows, but never use an unversioned run as a matched baseline.
ALTER TABLE eval_metrics ADD COLUMN IF NOT EXISTS config_hash text NOT NULL DEFAULT '';
ALTER TABLE eval_metrics ADD COLUMN IF NOT EXISTS label_hash text NOT NULL DEFAULT '';
ALTER TABLE eval_metrics ADD COLUMN IF NOT EXISTS code_revision text NOT NULL DEFAULT '';
ALTER TABLE eval_metrics ADD COLUMN IF NOT EXISTS run_config jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE eval_metrics ADD COLUMN IF NOT EXISTS attempted integer NOT NULL DEFAULT 0;
ALTER TABLE eval_metrics ADD COLUMN IF NOT EXISTS failed integer NOT NULL DEFAULT 0;
ALTER TABLE eval_metrics ADD COLUMN IF NOT EXISTS fp_penalty10 double precision NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_eval_metrics_matched ON eval_metrics(config_hash,label_hash,run_at DESC) WHERE failed=0 AND config_hash<>'' AND label_hash<>'';
