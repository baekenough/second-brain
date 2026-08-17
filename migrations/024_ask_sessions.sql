-- Migration 024: ask_sessions (Ask 화면 멀티턴 대화 보존, Part D 피드백
-- 루프의 토대).
--
-- Background: /ask 화면은 답변을 React state에만 보관했다 — 새로고침하면
-- 사라진다(운영에서 발견된 리스크). 기존 feedback 테이블(migration 005)은
-- 이미 session_id/query/document_id/chunk_id/thumbs 컬럼을 갖고 있어
-- "대화 세션이 저장된다"를 전제하고 있었지만, 그 세션을 실제로 만드는
-- 쪽이 없었다. 이 마이그레이션이 그 빈자리를 채운다.
--
-- Design: 한 행 = 한 대화(conversation_id)의 한 turn(turn_index, 0부터
-- 시작). 여러 turn이 conversation_id로 묶여 멀티턴 대화를 이룬다.
-- conversations라는 별도 테이블은 만들지 않는다 — 대화 제목/메타데이터가
-- 아직 필요 없고, 첫 turn(turn_index = 0)의 question이 사실상 제목
-- 역할을 한다(YAGNI). 필요해지면 그때 conversations 테이블을 추가하고
-- ask_sessions.conversation_id에 FK를 건다.
--
-- UNIQUE(conversation_id, turn_index): 같은 대화 안에서 turn 순서가
-- 중복/충돌하지 않도록 보장한다(동시 저장 레이스가 나도 DB가 막는다).
--
-- sources는 JSONB로 저장하고 documents/chunks에 FK를 걸지 않는다: 근거
-- 문서가 나중에 삭제(mass-delete guard, hard delete 등)돼도 "그때 무엇을
-- 근거로 답했는지"라는 이력은 보존되어야 한다. FK를 걸면 문서 삭제 시
-- 이력이 깨지거나(RESTRICT) 조용히 유실된다(SET NULL/CASCADE 모두
-- 부적절). 각 원소 형태: {"id": "<uuid>", "title": "...", "source_type":
-- "...", "score": 0.0}.
--
-- feedback.session_id는 TEXT 타입이며(migration 005), 이제 ask_sessions
-- .conversation_id(UUID)를 문자열로 담는 것이 자연스럽다 — 한 대화 = 한
-- feedback 세션. ask_sessions.id(개별 turn의 PK)가 아니라
-- conversation_id를 담아야 같은 대화의 여러 turn에서 나온 피드백이 하나의
-- session_id로 묶인다. 여기서 FK를 걸지 않는 이유: 배포된 eval 시드
-- feedback 행(50건)의 session_id 값이 UUID 형식이 아닐 수 있어 FK 제약을
-- 추가하면 마이그레이션 자체가 실패할 위험이 있다(사전 확인 결과는
-- store/ask_sessions.go 검증 로그 및 배포 노트를 참고). 애플리케이션
-- 레벨에서 conversation_id를 session_id로 채우는 것으로 연결을 유지한다.
--
-- Additive only.

CREATE TABLE IF NOT EXISTS ask_sessions (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id UUID        NOT NULL,
    turn_index      INT         NOT NULL,
    question        TEXT        NOT NULL,
    answer          TEXT        NOT NULL,
    finish_reason   TEXT        NOT NULL,             -- stop | error | no_evidence
    sources         JSONB       NOT NULL DEFAULT '[]'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (conversation_id, turn_index)
);

-- Conversation restore (ListConversationTurns: turn_index ASC within one
-- conversation_id) and the UNIQUE constraint above share this same column
-- order, so this index also backs constraint enforcement.
CREATE INDEX IF NOT EXISTS idx_ask_sessions_conversation_turn
    ON ask_sessions (conversation_id, turn_index);

-- Recent-conversations-list read pattern (ListRecentConversations: latest
-- turn per conversation, DESC).
CREATE INDEX IF NOT EXISTS idx_ask_sessions_created_at ON ask_sessions (created_at DESC);
