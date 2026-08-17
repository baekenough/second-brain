-- Migration 022: actions + action_status tables (Part A, spec §5.4, §7).
-- Additive only.

CREATE TABLE IF NOT EXISTS actions (
    id                     BIGSERIAL    PRIMARY KEY,
    identity_key           TEXT         NOT NULL UNIQUE,
    document_id            UUID         NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    thread_key             TEXT         NOT NULL,
    kind                   TEXT         NOT NULL,  -- awaiting_my_reply | my_commitment | their_commitment | scheduled
    summary                TEXT         NOT NULL,
    counterpart_entity_id  BIGINT       REFERENCES entities(id) ON DELETE SET NULL,
    due_at                 TIMESTAMPTZ,
    detected_by            TEXT         NOT NULL,  -- structural | llm | both
    confidence             NUMERIC(3,2) NOT NULL DEFAULT 1.00 CHECK (confidence >= 0 AND confidence <= 1),
    observed_at            TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_actions_thread_key ON actions (thread_key);
CREATE INDEX IF NOT EXISTS idx_actions_kind        ON actions (kind);
CREATE INDEX IF NOT EXISTS idx_actions_counterpart ON actions (counterpart_entity_id);
CREATE INDEX IF NOT EXISTS idx_actions_observed_at ON actions (observed_at DESC);

-- action_status: 1:1 with actions via identity_key (NOT actions.id — identity_key
-- is the stable cross-re-extraction handle; see identity_key design in Global
-- Constraints). PRIMARY KEY here doubles as the FK target validity requirement
-- (FK must reference a UNIQUE/PK column; actions.identity_key already is UNIQUE).
CREATE TABLE IF NOT EXISTS action_status (
    identity_key  TEXT         PRIMARY KEY REFERENCES actions(identity_key) ON DELETE CASCADE,
    state         TEXT         NOT NULL DEFAULT 'open',  -- open | done | ignored
    resolved_by   TEXT,                                   -- user | auto
    resolved_at   TIMESTAMPTZ,
    note          TEXT
);

CREATE INDEX IF NOT EXISTS idx_action_status_state ON action_status (state);
