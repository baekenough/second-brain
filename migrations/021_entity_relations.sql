-- Migration 021: entity_relations table (Part A extraction pipeline).
-- Spec: docs/superpowers/specs/2026-08-17-knowledge-graph-actions-feedback-design.md §5.4.
-- Additive only — no DROP, no destructive changes.

CREATE TABLE IF NOT EXISTS entity_relations (
    id                    BIGSERIAL    PRIMARY KEY,
    from_entity_id        BIGINT       NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    to_entity_id          BIGINT       NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    type                  TEXT         NOT NULL,  -- closed vocabulary; see model.RelationType
    evidence_document_id  UUID         NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    confidence            NUMERIC(3,2) NOT NULL DEFAULT 1.00 CHECK (confidence >= 0 AND confidence <= 1),
    observed_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (from_entity_id, to_entity_id, type, evidence_document_id)
);

CREATE INDEX IF NOT EXISTS idx_entity_relations_from     ON entity_relations (from_entity_id);
CREATE INDEX IF NOT EXISTS idx_entity_relations_to       ON entity_relations (to_entity_id);
CREATE INDEX IF NOT EXISTS idx_entity_relations_evidence ON entity_relations (evidence_document_id);
