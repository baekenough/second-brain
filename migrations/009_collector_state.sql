-- Per-instance watermark tracking for collectors that share a source_type.
-- Replaces the shared MAX(collected_at) watermark which caused one collector's
-- scan to suppress older files seen by another collector (e.g., laptop vs ubuntu1
-- vs ubuntu2 all using source_type='filesystem').
CREATE TABLE IF NOT EXISTS collector_state (
    instance_id       TEXT NOT NULL,
    source_type       TEXT NOT NULL,
    last_collected_at TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, source_type)
);
