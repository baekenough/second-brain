-- Migration 031: 골든셋(golden set) — 검색/hermes 품질 판정용 사람 정답셋.
--
-- Background: cmd/eval의 NDCG 정답 쌍은 지금까지 사용자 피드백(thumbs; see
-- internal/store/eval.go)에서만 파생됐다. 피드백은 시스템이 이미 화면에
-- 보여준 문서에만 존재할 수 있으므로 리콜(누락) 문제를 측정하지 못하고,
-- 결과의 정확성만 부분적으로 확인한다(자기확증 편향). 이 마이그레이션은
-- 사용자가 질의마다 후보 문서 집합 전체를 relevant/irrelevant/noise로 직접
-- 판정하는 독립적인 골든셋을 도입한다.
--
-- Design:
--   golden_queries   — 판정 대상 질의. source는 실사용 질의 이력
--                       (ask_history) / 수기 시드(seed) / 수동 추가(manual) /
--                       hermes 대화 중 자동 생성(hermes) 중 하나. status는
--                       진행 상태(open→done, 또는 skipped).
--   golden_judgments — 질의×문서 판정. judge는 이 판정을 내린 주체가
--                       사람(user)인지 hermes가 대화 중 스스로 매긴
--                       LLM 자동판정(llm)인지를 구분한다. 이 구분이 없으면
--                       검증되지 않은 LLM 판정이 사람이 만든 골든셋(평가
--                       홀드아웃)에 섞여 들어가 오염시킨다 — cmd/eval
--                       --golden과 GET .../export 기본값은 judge='user'만
--                       내보낸다. UNIQUE(query_id, document_id, judge)는
--                       같은 문서에 대해 사람 판정과 LLM 판정이 공존할 수
--                       있게 하면서도(서로 다른 judge 값), 같은 judge
--                       안에서의 중복 판정은 막는다. 재판정은 upsert로
--                       덮어쓴다. rank_at_judgment는 판정 시점에 검색
--                       결과에서 그 문서가 차지했던 순위(재현/분석용,
--                       nullable — 검색 외부에서 추가된 문서일 수도 있음).
--
-- documents FK는 ON DELETE CASCADE: 문서가 (mass-delete guard 등으로) 삭제되면
-- 그 문서에 대한 판정은 더 이상 참조할 대상이 없어 의미가 없다. ask_sessions
-- .sources(마이그레이션 024)와 달리 여기서는 "그 시점에 무엇을 보여줬는지"
-- 이력을 보존할 요구가 없다 — 판정은 재현 가능한 라벨이지 감사 로그가
-- 아니다.
--
-- golden_queries.text에 UNIQUE 제약을 둔 이유: 같은 질의를 두 번 생성하면
-- 진행 상태(status)가 복제되어 "판정 완료"와 "미판정"이 같은 질문에 대해
-- 동시에 존재하는 모순이 생긴다. 정확히 같은 텍스트만 막고, 근사 중복 제거
-- (공백/대소문자 정규화)는 애플리케이션 레이어(internal/store/golden.go가
-- internal/dataset.Normalize를 재사용)에서 처리한다 — SQL에 정규화 표현식을
-- 새로 만들면 dataset.Normalize/SplitOf가 이미 고정한 규칙과 두 번째 정의가
-- 생기고 어긋날 위험이 있다.
--
-- Additive only. All statements are idempotent (IF NOT EXISTS) so a re-run
-- against an already-migrated database is a no-op.

CREATE TABLE IF NOT EXISTS golden_queries (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    text       TEXT NOT NULL,
    source     TEXT NOT NULL CHECK (source IN ('ask_history', 'seed', 'manual', 'hermes')),
    status     TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'done', 'skipped')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (text)
);

CREATE TABLE IF NOT EXISTS golden_judgments (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    query_id         UUID NOT NULL REFERENCES golden_queries (id) ON DELETE CASCADE,
    document_id      UUID NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
    judgment         TEXT NOT NULL CHECK (judgment IN ('relevant', 'irrelevant', 'noise')),
    judge            TEXT NOT NULL DEFAULT 'user' CHECK (judge IN ('user', 'llm')),
    rank_at_judgment INT,
    judged_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (query_id, document_id, judge)
);

-- GET /api/v1/golden/next: pick the open query with the fewest judgments so
-- far. The index below backs the lookup of judgments per query; the LEFT
-- JOIN + COUNT + ORDER BY in the query itself does the "fewest" ranking.
CREATE INDEX IF NOT EXISTS idx_golden_judgments_query_id ON golden_judgments (query_id);

-- POST /api/v1/golden/judgments feedback loop: reverse lookup from a
-- document to its judgments is not on the hot path today, but is cheap
-- insurance for future analysis (e.g. "which documents were judged noise
-- most often") and mirrors the query_id index above.
CREATE INDEX IF NOT EXISTS idx_golden_judgments_document_id ON golden_judgments (document_id);

-- GET /api/v1/golden/next and .../export both filter/group by judge (default
-- 'user') in addition to query_id — this composite index backs both without
-- forcing a fallback to the query_id-only index above plus a filter step.
CREATE INDEX IF NOT EXISTS idx_golden_judgments_query_judge ON golden_judgments (query_id, judge);

-- GET /api/v1/golden/next filters on status='open' before ranking by
-- judgment count; status has low cardinality (3 values) but the table will
-- grow past the point where a sequential scan is free once ask_history
-- generation runs repeatedly.
CREATE INDEX IF NOT EXISTS idx_golden_queries_status ON golden_queries (status);
