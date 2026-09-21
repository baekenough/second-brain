-- The first enabled worker records its cutoff. Deploying this migration alone
-- must not activate collection or make older communications eligible.
CREATE TABLE IF NOT EXISTS calendar_automation_state (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    activated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE IF NOT EXISTS calendar_automation_jobs (
    document_id UUID PRIMARY KEY REFERENCES documents(id) ON DELETE CASCADE,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'retry', 'created', 'duplicate', 'skipped', 'review', 'failed')),
    decision JSONB,
    candidate_event JSONB,
    event_id TEXT NOT NULL DEFAULT '',
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    retry_reason TEXT NOT NULL DEFAULT '',
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_calendar_automation_due
    ON calendar_automation_jobs(next_attempt_at, document_id)
    WHERE status IN ('pending', 'retry');

CREATE INDEX IF NOT EXISTS idx_documents_calendar_automation
    ON documents(created_at, id)
    WHERE status = 'active' AND source_type IN ('gmail', 'sms', 'call');
