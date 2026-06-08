-- Migration 017: add knowledge-graph entity tables (MVP, issue #77).
-- Additive only — no DROP, no destructive changes.
-- entities: canonical named-entity registry (deduped by normalized_name + type).
-- document_entities: join table linking documents to extracted entities.

CREATE TABLE IF NOT EXISTS entities (
    id              BIGSERIAL    PRIMARY KEY,
    name            TEXT         NOT NULL,
    type            TEXT         NOT NULL,   -- PERSON | ORG | CONCEPT | OTHER
    normalized_name TEXT         NOT NULL,   -- lower(trim(name)) for dedup
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- Unique constraint: one canonical entity per (normalized_name, type) pair.
CREATE UNIQUE INDEX IF NOT EXISTS idx_entities_normalized_type
    ON entities (normalized_name, type);

-- document_entities: many-to-many join between documents and entities.
CREATE TABLE IF NOT EXISTS document_entities (
    document_id  UUID    NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    entity_id    BIGINT  NOT NULL REFERENCES entities(id)  ON DELETE CASCADE,
    PRIMARY KEY  (document_id, entity_id)
);

CREATE INDEX IF NOT EXISTS idx_document_entities_document_id ON document_entities (document_id);
CREATE INDEX IF NOT EXISTS idx_document_entities_entity_id   ON document_entities (entity_id);
