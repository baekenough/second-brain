-- 삭제 주체가 불명확한 과거 행(수동 SQL 포함)은 재수집으로 복원하지 않는다.
-- 파일시스템 오삭제 복구는 운영자가 확인 후 deleted_by='filesystem'로 지정한다.
ALTER TABLE documents ADD COLUMN IF NOT EXISTS deleted_by TEXT
    CHECK (deleted_by IN ('user', 'policy', 'system', 'filesystem'));

-- 취소 상태가 남은 캘린더는 마이그레이션 030의 명확한 시스템 삭제다.
UPDATE documents SET deleted_by = 'system'
WHERE status = 'deleted' AND source_type = 'calendar'
  AND metadata->>'status' = 'cancelled' AND deleted_by IS NULL;

-- 워커가 삭제 전에 읽은 노트로 뒤늦게 추론을 저장하는 경쟁 조건도 막는다.
-- 부모 잠금은 노트 삭제와 직렬화한다. 먼저 저장되면 뒤따른 연쇄 삭제가 지우고,
-- 먼저 삭제되면 새 추론 저장을 거절한다. 기존 근거 없는 레거시 행은 건드리지 않는다.
CREATE OR REPLACE FUNCTION guard_insight_source_note() RETURNS trigger AS $$
DECLARE
    parent_status TEXT;
    parent_id TEXT;
BEGIN
    IF NEW.source_type <> 'insight' OR NEW.status <> 'active' THEN
        RETURN NEW;
    END IF;
    parent_id := NEW.metadata->'provenance'->>'source_note_id';
    IF parent_id IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT status INTO parent_status FROM documents
    WHERE id = parent_id::uuid AND source_type = 'note' FOR SHARE;
    IF parent_status IS DISTINCT FROM 'active' THEN
        RAISE EXCEPTION 'insight source note is not active' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS insight_source_note_active ON documents;
CREATE TRIGGER insight_source_note_active
BEFORE INSERT OR UPDATE ON documents
FOR EACH ROW EXECUTE FUNCTION guard_insight_source_note();
