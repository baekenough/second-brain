-- Migration 040: chunk_sparse_context — 청크 희소(FTS/bigm) 매칭 대상에 붙일
-- 파생 문맥(제목·참여자 등 결정론적 헤더 + 청크 본문) 테이블(#270 phase B).
--
-- chunks 테이블 자체에는 컬럼을 더하지 않는다. chunks 에는 이미
-- content_tsv(GENERATED STORED) + GIN 과 embedding vector(1536) + HNSW 가
-- 있어서, 컬럼 하나를 더 붙이면 테이블 전체 재작성(ACCESS EXCLUSIVE 락 +
-- HNSW 포함 전 인덱스 재구축)이 일어난다 — 약 8.2만 행 규모에서 감당할 이유가
-- 없는 대가다. 별도 테이블이면 롤백도 이 테이블 제거와 노브 원복만으로 끝난다.
--
-- RunMigrations(internal/store/postgres.go)는 되돌림(down) 파일이 없고 매
-- 기동마다 모든 *.sql 을 다시 실행한다 — 이 파일의 모든 문장은 반드시
-- 멱등(IF NOT EXISTS)이어야 한다. 백필(행 채우기)은 여기서 하지 않는다:
-- cmd/sparsectx 가 배치로, 별도 트랜잭션으로 채운다.

-- sparse_text 는 헤더 단독이 아니라 "헤더 + 청크 본문"을 담는다.
-- plainto_tsquery 는 토큰을 AND 로 묶으므로, "<이름> 예산" 처럼 이름은
-- 헤더에·예산은 본문에 있는 질의가 한 tsvector 에서 만나야 한다 — 두 컬럼을
-- 각각의 GIN 인덱스로 OR 하면 그 인덱스들을 못 쓴다.
--
-- fingerprint 는 이 문맥이 만들어질 당시 문서의 source_type·title·
-- occurred_at·metadata 를 해시한 값이다(internal/store 의
-- chunkSparseFingerprintSQL 이 SQL 로만 계산하고, 백필 워커와 조회 레인
-- 양쪽에서 동일한 SQL 문자열을 그대로 재사용한다 — Go 에서 따로 계산하지
-- 않으므로 두 계산이 어긋날 수가 없다). 문서 title 이 바뀌거나 개인정보
-- 마스킹으로 metadata 가 바뀌면 이 값이 달라지고, 조회 레인은 fingerprint 가
-- 일치하는 문맥 행만 쓴다 — 불일치하면 즉시(백필 워커가 다시 돌기 전에도)
-- raw 청크 본문 매칭으로 빠지므로, 마스킹 이전 이름이 문맥을 통해 새어
-- 나가는 경로가 구조적으로 없다.
CREATE TABLE IF NOT EXISTS chunk_sparse_context (
    chunk_id        BIGINT      NOT NULL REFERENCES chunks(id) ON DELETE CASCADE,
    context_version TEXT        NOT NULL,
    fingerprint     TEXT        NOT NULL,
    sparse_text     TEXT        NOT NULL,
    sparse_tsv      tsvector    GENERATED ALWAYS AS (to_tsvector('simple', sparse_text)) STORED,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (chunk_id, context_version)
);

-- migrations/004_chunks.sql·006_bigm.sql 이 chunks 에 쓰는 것과 같은 인덱스
-- 조합(GIN tsvector + GIN pg_bigm)을 이 테이블에도 둔다.
CREATE INDEX IF NOT EXISTS idx_chunk_sparse_context_tsv
    ON chunk_sparse_context USING gin (sparse_tsv);

CREATE INDEX IF NOT EXISTS idx_chunk_sparse_context_bigm
    ON chunk_sparse_context USING gin (sparse_text gin_bigm_ops);

-- 배치별 재개점. context_version 별로 하나씩만 있다 — 레시피(v1-tp/v1-full)
-- 를 동시에 백필해도 서로의 진행을 덮어쓰지 않는다.
CREATE TABLE IF NOT EXISTS chunk_sparse_backfill_state (
    context_version TEXT        PRIMARY KEY,
    last_chunk_id   BIGINT      NOT NULL DEFAULT 0,
    pass_started_at TIMESTAMPTZ,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
