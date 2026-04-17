CREATE TABLE IF NOT EXISTS eval_metrics (
    id         SERIAL PRIMARY KEY,
    ndcg5      DOUBLE PRECISION NOT NULL,
    ndcg10     DOUBLE PRECISION NOT NULL,
    mrr10      DOUBLE PRECISION NOT NULL,
    pairs      INTEGER NOT NULL,
    run_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_eval_metrics_run_at ON eval_metrics (run_at DESC);
