-- 마이그레이션 037: 임베딩 버전 추적 컬럼.
--
-- 배경: 지금까지 벡터에는 "어떤 설정으로 만들어졌는가"를 적어 두는 곳이
-- 없었다. 그래서 임베딩 모델을 바꾸거나 임베딩에 넣는 텍스트 구성을 바꾸면,
-- 새 설정으로 만든 벡터와 옛 설정으로 만든 벡터가 같은 인덱스에 말없이 섞였다.
-- 서로 다른 임베딩 공간의 벡터는 거리 비교 자체가 의미를 잃기 때문에, 검색
-- 품질이 조용히 무너져도 원인을 짚을 수단이 없었다.
--
-- embedding_version 값 형식: "{model}:{dimensions}:{recipe}"
--   예) text-embedding-3-small:1536:doc-v1
--       text-embedding-3-small:1536:chunk-ctx-v1
-- 생성 지점은 internal/search/embedding_version.go 의 EmbeddingVersion().
--
-- NULL = 레거시. 이 마이그레이션 이전에 만들어진 모든 행이 여기 해당한다.
-- 특히 chunks 의 기존 벡터는 "청크 본문만" 임베딩한 것이라 문서 제목·발신자·
-- 날짜 맥락이 없다 — 재임베딩 대상으로 잡히도록 일부러 채우지 않는다.
-- (기존 값을 임의로 채워 넣으면 재임베딩이 필요한 행을 영구히 숨기게 된다.)
--
-- 재임베딩은 자동으로 돌지 않는다: EMBEDDING_REEMBED_ENABLED=true 일 때만
-- 스케줄러 백필이 "현재 버전과 다른 행"까지 집어 간다(비용이 드는 작업이므로
-- 기본은 꺼져 있다).
--
-- 멱등: ADD COLUMN IF NOT EXISTS / CREATE INDEX IF NOT EXISTS 만 사용한다.

ALTER TABLE documents ADD COLUMN IF NOT EXISTS embedding_version text;
ALTER TABLE chunks ADD COLUMN IF NOT EXISTS embedding_version text;

-- 재임베딩 대상 스캔용 부분 인덱스.
-- documents 는 active 행만 재임베딩하므로 status 로 좁힌다. chunks 는 활성
-- 문서와 조인해서 걸러내므로 컬럼 단독 인덱스로 둔다.
CREATE INDEX IF NOT EXISTS idx_documents_embedding_version
    ON documents(embedding_version) WHERE status = 'active';
CREATE INDEX IF NOT EXISTS idx_chunks_embedding_version
    ON chunks(embedding_version);
