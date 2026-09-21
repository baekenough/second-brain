-- 마이그레이션 038: awaiting_my_reply(응답 대기) 액션 일괄 은퇴.
--
-- 사용자 결정(2026-09-21): 메일·SMS에 "답장하지 않았다"로 자동 생성되는
-- awaiting_my_reply 항목은 할일이 아니라 잡음이다. internal/worker/
-- structural_signals.go 가 더 이상 이 종류를 만들지 않도록 코드를 함께
-- 고쳤고(생성 경로 제거), 이 마이그레이션은 그 전까지 이미 만들어져 열려
-- 있던 행을 화면에서 사라지게 한다.
--
-- actions 테이블 자체는 지우지 않는다: document_id 참조와 근거 이력을
-- 남겨 두어야 나중에 필요하면 되돌리거나 감사할 수 있다. 대신
-- action_status.state 를 'ignored' 로 바꿔 /actions 목록(내부적으로
-- COALESCE(state,'open')='open' 인 행만 노출)에서 빠지게 한다.
-- action_status.note 에 은퇴 사유를 남겨 두면, 나중에 "왜 무시됐는가"를
-- 조회 한 번으로 답할 수 있다 — 되돌리려면 이 note 값을 가진 행만 골라
-- state 를 'open' 으로 되돌리면 된다.
--
-- 멱등성: 아래 두 문장 모두 kind = 'awaiting_my_reply' 로 범위를 한정하고,
-- 이미 'ignored' 인 행은 WHERE 조건에서 걸러지므로(1번) 또는
-- ON CONFLICT DO NOTHING 으로 재실행돼도 안전하다(2번).

-- 1) 이미 action_status 행이 있고 아직 'open' 인 것 → 'ignored' 로 전환.
--    resolved_at 은 상태가 실제로 바뀔 때만 채운다(store.SetActionState 의
--    관례와 동일 — 재실행 시 타임스탬프가 계속 갱신되면 "언제 무시됐는가"가
--    무의미해진다).
UPDATE action_status ast
SET state       = 'ignored',
    resolved_by = 'system',
    resolved_at = now(),
    note        = 'awaiting_my_reply 일괄 은퇴: 응답 대기 신호는 할일이 아니라 잡음으로 판단되어 자동 무시 처리됨 (마이그레이션 038, 2026-09-21)'
FROM actions a
WHERE a.identity_key = ast.identity_key
  AND a.kind = 'awaiting_my_reply'
  AND ast.state = 'open';

-- 2) actions 행은 있는데 action_status 행이 아예 없는 경우(과거 EnsureOpenStatus
--    쓰기 실패 등으로 생긴 결손 — internal/worker/extraction_worker.go 와
--    internal/worker/structural_signals.go 의 UpsertAction/EnsureOpenStatus
--    분리 쓰기 참고) — 이런 행은 action_status 가 없으므로 LEFT JOIN 에서
--    COALESCE(state,'open') 이 'open' 으로 보여 그냥 두면 목록에 노출된다.
--    명시적으로 'ignored' 행을 채워 넣는다.
INSERT INTO action_status (identity_key, state, resolved_by, resolved_at, note)
SELECT a.identity_key,
       'ignored',
       'system',
       now(),
       'awaiting_my_reply 일괄 은퇴: 응답 대기 신호는 할일이 아니라 잡음으로 판단되어 자동 무시 처리됨 (마이그레이션 038, 2026-09-21)'
FROM actions a
LEFT JOIN action_status ast ON ast.identity_key = a.identity_key
WHERE a.kind = 'awaiting_my_reply'
  AND ast.identity_key IS NULL
ON CONFLICT (identity_key) DO NOTHING;
