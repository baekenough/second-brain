-- Search weight change history and promotion state (Part D — feedback loop, Task 7).
--
-- Every candidate the tuner produces is recorded here, including the ones that
-- are refused: without a row for the refusals, "why did the weights not change
-- last week?" has no answer other than re-running the search.
--
-- No query text and no document IDs are stored — only aggregate scalars. The
-- labels this table summarises are personal; the summary must not be.
CREATE TABLE IF NOT EXISTS search_weights_history (
    id              BIGSERIAL PRIMARY KEY,
    weights         JSONB       NOT NULL,          -- model.SearchWeights serialised
    status          TEXT        NOT NULL,          -- proposed|active|rolled_back|superseded
    train_objective DOUBLE PRECISION,
    holdout_ndcg10  DOUBLE PRECISION,
    holdout_fp10    DOUBLE PRECISION,
    baseline_ndcg10 DOUBLE PRECISION,
    baseline_fp10   DOUBLE PRECISION,
    holdout_queries INTEGER     NOT NULL DEFAULT 0,
    gate_reason     TEXT,                          -- human-readable refusal reason
    metadata        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    promoted_at     TIMESTAMPTZ,
    CHECK (status IN ('proposed','active','rolled_back','superseded'))
);

-- At most one active row, enforced by the database rather than by an
-- application-level check. Two concurrent tuner runs would both pass an
-- "is there already an active row?" test and both write one; a partial unique
-- index makes the second one fail instead.
CREATE UNIQUE INDEX IF NOT EXISTS uq_search_weights_active
  ON search_weights_history ((status)) WHERE status = 'active';

CREATE INDEX IF NOT EXISTS idx_search_weights_created
  ON search_weights_history (created_at DESC);
