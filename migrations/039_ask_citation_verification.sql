-- 마이그레이션 039: ask_sessions에 citation_verification 컬럼 추가 (issue #268).
--
-- Background: /ask 답변이 프롬프트에 실제로 보여준 문서 ID만 인용했는지를
-- 결정론적으로 검증하는 기능(internal/api/ask_citation.go의
-- validateAskCitations)이 추가되면서, 그 검증 결과("valid" | "invalid" |
-- "missing" | "abstained" | "unverified" 등, internal/store/ask_sessions.go
-- AskCitationVerification 참고)를 turn마다 함께 저장해야 한다.
--
-- 저장 이유는 두 가지: (1) 새로고침 시 같은 검증 결과를 다시 계산하지 않고
-- 그대로 복원, (2) recentAskHistory(internal/api/ask_history.go)가 이 값을
-- 보고 "invalid"/"unverified" turn을 이후 프롬프트 재생(history replay)에서
-- 제외한다 — 한 번 검증에 실패한 답변을 다시 모델에게 보여주지 않기 위함.
--
-- NULLable로 둔다: migration 024의 sources 컬럼(NOT NULL DEFAULT '[]')과
-- 다르게, "이 turn의 인용을 검증했다"와 "검증 자체를 하지 않았다"는 서로 다른
-- 사실이며 후자를 빈 값으로 흉내 내면 안 된다 — no_evidence turn(애초에
-- synthesize가 호출되지 않음)과 이 마이그레이션 이전에 쓰인 모든 기존 행이
-- 여기 해당한다. 두 경우 모두 NULL이 정직한 표현이다(AskSource.OccurredAt이
-- #218 이전 행에 대해 같은 이유로 NULL을 쓰는 것과 동일한 논리).
--
-- Additive only — IF NOT EXISTS로 재실행 안전.

ALTER TABLE ask_sessions
    ADD COLUMN IF NOT EXISTS citation_verification JSONB NULL;
