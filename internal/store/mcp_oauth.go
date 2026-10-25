package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	pgxstd "github.com/jackc/pgx/v5"

	"github.com/baekenough/second-brain/internal/mcpauth"
)

// MCPOAuthStore 는 MCP OAuth 리프레시 토큰의 Postgres 저장소다(migrations/042).
// mcpauth.RefreshStore 를 구현한다. 토큰 원문은 받지도 저장하지도 않고 SHA-256
// 해시만 다룬다.
type MCPOAuthStore struct {
	pg *Postgres

	// beforeRotateWrite 는 테스트가 회전 트랜잭션을 검사 직후에 멈춰 세우는 훅이다
	// (운영에서는 nil).
	beforeRotateWrite func()
}

// mcpOAuthLockNS 는 계보 잠금의 advisory lock 네임스페이스(첫 번째 키)다.
const mcpOAuthLockNS int32 = 0x4d4f4155

// lockFamily 는 계보 단위 트랜잭션 advisory lock 을 건다.
//
// 왜 필요한가: READ COMMITTED 에서 재사용 탐지의 `UPDATE … WHERE family_id=… AND
// revoked_at IS NULL` 은 UPDATE 시작 시점의 스냅숏만 본다. 같은 계보의 최신 토큰을 동시에
// 회전하는 트랜잭션이 그 뒤에 커밋한 자식 행은 폐기에서 빠지고, 공격자가 회전을 반복하면
// 폐기를 이겨 살아남는다. 계보를 바꾸는 모든 트랜잭션(회전·폐기)이 이 잠금을 먼저
// 잡으면 직렬화되어, 뒤에 잡은 쪽의 SELECT/UPDATE 가 앞 트랜잭션의 커밋을 본다.
func lockFamily(ctx context.Context, tx pgxstd.Tx, familyID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2))`, mcpOAuthLockNS, familyID)
	return err
}

var _ mcpauth.RefreshStore = (*MCPOAuthStore)(nil)

// NewMCPOAuthStore 는 저장소를 만든다.
func NewMCPOAuthStore(pg *Postgres) *MCPOAuthStore { return &MCPOAuthStore{pg: pg} }

// mcpOAuthExpiredRetention 은 만료된 행을 지우기까지 남겨 두는 시간이다. 만료된 토큰은
// 어차피 쓸 수 없으므로 오래 둘 이유가 없고, 재사용 탐지는 만료 전 구간에서만
// 의미가 있다.
const mcpOAuthExpiredRetention = 24 * time.Hour

// Create 는 새 계보의 첫 리프레시 토큰을 저장한다. 같은 호출에서 오래 전에
// 만료된 행을 정리해 별도 청소 작업 없이 테이블이 자라지 않게 한다.
func (s *MCPOAuthStore) Create(ctx context.Context, g mcpauth.RefreshGrant, tokenHash []byte, expiresAt, now time.Time) error {
	if _, err := s.pg.pool.Exec(ctx,
		`DELETE FROM mcp_oauth_refresh_tokens WHERE expires_at < $1`,
		now.Add(-mcpOAuthExpiredRetention)); err != nil {
		return fmt.Errorf("prune expired mcp oauth refresh tokens: %w", err)
	}
	_, err := s.pg.pool.Exec(ctx,
		`INSERT INTO mcp_oauth_refresh_tokens
		     (token_hash, family_id, client_id, scope, resource, issued_at, expires_at)
		 VALUES ($1, $2::uuid, $3, $4, $5, $6, $7)`,
		tokenHash, g.FamilyID, g.ClientID, g.Scope, g.Resource, now, expiresAt)
	if err != nil {
		return fmt.Errorf("insert mcp oauth refresh token: %w", err)
	}
	return nil
}

// Rotate 는 제시된 토큰을 원자적으로 소모하고 새 토큰을 같은 계보에 잇는다.
//
// 행 잠금(FOR UPDATE)이 동시성의 핵심이다: 같은 토큰으로 동시에 두 요청이
// 오면 둘째는 첫째가 끝날 때까지 기다리고, 깨어난 뒤에는 used_at 이 차 있어
// 재사용으로 판정된다(계보 폐기). 재사용 판정의 폐기 UPDATE 는 롤백되지 않도록
// 반드시 커밋한다.
func (s *MCPOAuthStore) Rotate(ctx context.Context, req mcpauth.RotateRequest) (mcpauth.RefreshGrant, mcpauth.RotateOutcome, error) {
	tx, err := s.pg.pool.Begin(ctx)
	if err != nil {
		return mcpauth.RefreshGrant{}, mcpauth.RotateInvalid, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1) 계보를 알아낸다(잠금 없이). 모르는 토큰이면 끝.
	var familyID string
	err = tx.QueryRow(ctx,
		`SELECT family_id::text FROM mcp_oauth_refresh_tokens WHERE token_hash = $1`,
		req.PresentedHash).Scan(&familyID)
	if errors.Is(err, pgxstd.ErrNoRows) {
		return mcpauth.RefreshGrant{}, mcpauth.RotateInvalid, nil
	}
	if err != nil {
		return mcpauth.RefreshGrant{}, mcpauth.RotateInvalid, fmt.Errorf("lookup family: %w", err)
	}
	// 2) 계보를 잠근다.
	if err := lockFamily(ctx, tx, familyID); err != nil {
		return mcpauth.RefreshGrant{}, mcpauth.RotateInvalid, fmt.Errorf("lock family: %w", err)
	}

	// 3) 잠근 뒤에 다시 읽는다(그 사이 폐기·회전이 커밋됐을 수 있다).
	var (
		g         mcpauth.RefreshGrant
		expiresAt time.Time
		usedAt    *time.Time
		revokedAt *time.Time
	)
	err = tx.QueryRow(ctx,
		`SELECT family_id::text, client_id, scope, resource, expires_at, used_at, revoked_at
		   FROM mcp_oauth_refresh_tokens
		  WHERE token_hash = $1
		    FOR UPDATE`, req.PresentedHash).
		Scan(&g.FamilyID, &g.ClientID, &g.Scope, &g.Resource, &expiresAt, &usedAt, &revokedAt)
	if errors.Is(err, pgxstd.ErrNoRows) {
		return mcpauth.RefreshGrant{}, mcpauth.RotateInvalid, nil
	}
	if err != nil {
		return mcpauth.RefreshGrant{}, mcpauth.RotateInvalid, fmt.Errorf("select refresh token: %w", err)
	}

	switch {
	case revokedAt != nil:
		return mcpauth.RefreshGrant{}, mcpauth.RotateInvalid, nil
	case usedAt != nil:
		if _, err := tx.Exec(ctx,
			`UPDATE mcp_oauth_refresh_tokens SET revoked_at = $2
			  WHERE family_id = $1::uuid AND revoked_at IS NULL`,
			g.FamilyID, req.Now); err != nil {
			return mcpauth.RefreshGrant{}, mcpauth.RotateInvalid, fmt.Errorf("revoke family: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return mcpauth.RefreshGrant{}, mcpauth.RotateInvalid, fmt.Errorf("commit revoke: %w", err)
		}
		return mcpauth.RefreshGrant{}, mcpauth.RotateReused, nil
	case !req.Now.Before(expiresAt), g.ClientID != req.ClientID:
		return mcpauth.RefreshGrant{}, mcpauth.RotateInvalid, nil
	}

	if s.beforeRotateWrite != nil {
		s.beforeRotateWrite()
	}
	if _, err := tx.Exec(ctx,
		`UPDATE mcp_oauth_refresh_tokens SET used_at = $2 WHERE token_hash = $1`,
		req.PresentedHash, req.Now); err != nil {
		return mcpauth.RefreshGrant{}, mcpauth.RotateInvalid, fmt.Errorf("mark used: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO mcp_oauth_refresh_tokens
		     (token_hash, family_id, client_id, scope, resource, issued_at, expires_at)
		 VALUES ($1, $2::uuid, $3, $4, $5, $6, $7)`,
		req.NewHash, g.FamilyID, g.ClientID, g.Scope, g.Resource, req.Now, req.NewExpiresAt); err != nil {
		return mcpauth.RefreshGrant{}, mcpauth.RotateInvalid, fmt.Errorf("insert rotated token: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return mcpauth.RefreshGrant{}, mcpauth.RotateInvalid, fmt.Errorf("commit rotation: %w", err)
	}
	return g, mcpauth.RotateOK, nil
}

// RevokeFamily 는 계보의 아직 폐기되지 않은 모든 토큰을 폐기한다. 회전과 같은 계보
// 잠금 아래에서 하므로 동시에 커밋되는 자식 행을 놓치지 않는다.
func (s *MCPOAuthStore) RevokeFamily(ctx context.Context, familyID string, now time.Time) error {
	tx, err := s.pg.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockFamily(ctx, tx, familyID); err != nil {
		return fmt.Errorf("lock family: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE mcp_oauth_refresh_tokens SET revoked_at = $2
		  WHERE family_id = $1::uuid AND revoked_at IS NULL`,
		familyID, now); err != nil {
		return fmt.Errorf("revoke mcp oauth family: %w", err)
	}
	return tx.Commit(ctx)
}
