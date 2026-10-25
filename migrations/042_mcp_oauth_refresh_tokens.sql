-- MCP OAuth 리프레시 토큰(cmd/mcp, internal/mcpauth).
--
-- 토큰 원문은 저장하지 않는다. token_hash 는 SHA-256(32바이트)이다. 컨테이너
-- 재시작·배포 뒤에도 연결이 유지되도록 Postgres 에 둔다.
--
-- 회전: 토큰을 쓰면 used_at 을 채우고 같은 family_id 로 새 행을 잇는다. 이미
-- used_at 이 찬 토큰이 다시 오면(재사용) 그 family_id 전체에 revoked_at 을 채운다.
--
-- 모든 구문은 멱등이다: 서버가 기동 때마다 마이그레이션을 다시 실행한다.
-- 전체 폐기(운영 절차): UPDATE mcp_oauth_refresh_tokens SET revoked_at = now() WHERE revoked_at IS NULL;
CREATE TABLE IF NOT EXISTS mcp_oauth_refresh_tokens (
    token_hash  BYTEA       PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    family_id   UUID        NOT NULL,
    client_id   TEXT        NOT NULL,
    scope       TEXT        NOT NULL,
    resource    TEXT        NOT NULL,
    issued_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    used_at     TIMESTAMPTZ,
    revoked_at  TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_mcp_oauth_refresh_tokens_family
    ON mcp_oauth_refresh_tokens (family_id);
CREATE INDEX IF NOT EXISTS idx_mcp_oauth_refresh_tokens_expires
    ON mcp_oauth_refresh_tokens (expires_at);
