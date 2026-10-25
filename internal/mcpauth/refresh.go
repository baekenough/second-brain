package mcpauth

import (
	"context"
	"sync"
	"time"
)

// RefreshGrant 는 리프레시 토큰 한 계보(family)가 이어받는 권한이다. 회전할
// 때마다 새 토큰이 같은 값을 물려받는다.
type RefreshGrant struct {
	FamilyID string
	ClientID string
	Scope    string
	Resource string
}

// RotateOutcome 은 리프레시 토큰 회전 결과다.
type RotateOutcome int

const (
	// RotateInvalid: 모르는 토큰, 만료, 폐기된 계보, 클라이언트 불일치.
	RotateInvalid RotateOutcome = iota
	// RotateOK: 제시한 토큰을 사용 처리하고 새 토큰을 저장했다.
	RotateOK
	// RotateReused: 이미 회전된 토큰이 다시 제시됐다. 계보 전체를 폐기했다.
	RotateReused
)

// RotateRequest 는 회전 입력이다. 토큰 원문은 어디에도 넘기지 않고 해시만 쓴다.
type RotateRequest struct {
	PresentedHash []byte
	ClientID      string
	NewHash       []byte
	NewExpiresAt  time.Time
	Now           time.Time
}

// RefreshStore 는 리프레시 토큰의 영속 저장소다. 구현은 Rotate 를 원자적으로
// 처리해야 한다 — 같은 토큰을 동시에 두 번 제시하면 정확히 한 번만 RotateOK 이고
// 나머지는 재사용으로 판정돼 계보가 폐기된다.
type RefreshStore interface {
	// Create 는 새 계보의 첫 토큰을 저장한다.
	Create(ctx context.Context, g RefreshGrant, tokenHash []byte, expiresAt, now time.Time) error
	// Rotate 는 제시된 토큰을 소모하고 NewHash 를 같은 계보에 잇는다.
	Rotate(ctx context.Context, req RotateRequest) (RefreshGrant, RotateOutcome, error)
	// RevokeFamily 는 계보의 모든 토큰을 폐기한다(인가 코드 재사용 탐지 등).
	RevokeFamily(ctx context.Context, familyID string, now time.Time) error
}

// MemoryRefreshStore 는 RefreshStore 의 메모리 구현이다. 단위·통합 테스트용이며
// 재시작하면 사라진다. 운영은 internal/store.MCPOAuthStore(Postgres)를 쓴다.
type MemoryRefreshStore struct {
	mu   sync.Mutex
	rows map[string]*memRefreshRow
}

type memRefreshRow struct {
	grant     RefreshGrant
	expiresAt time.Time
	used      bool
	revoked   bool
}

// NewMemoryRefreshStore 는 빈 메모리 저장소를 만든다.
func NewMemoryRefreshStore() *MemoryRefreshStore {
	return &MemoryRefreshStore{rows: map[string]*memRefreshRow{}}
}

func (m *MemoryRefreshStore) Create(_ context.Context, g RefreshGrant, tokenHash []byte, expiresAt, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[string(tokenHash)] = &memRefreshRow{grant: g, expiresAt: expiresAt}
	return nil
}

func (m *MemoryRefreshStore) Rotate(_ context.Context, req RotateRequest) (RefreshGrant, RotateOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[string(req.PresentedHash)]
	switch {
	case !ok, row.revoked:
		return RefreshGrant{}, RotateInvalid, nil
	case row.used:
		m.revokeLocked(row.grant.FamilyID)
		return RefreshGrant{}, RotateReused, nil
	case !req.Now.Before(row.expiresAt), row.grant.ClientID != req.ClientID:
		return RefreshGrant{}, RotateInvalid, nil
	}
	row.used = true
	m.rows[string(req.NewHash)] = &memRefreshRow{grant: row.grant, expiresAt: req.NewExpiresAt}
	return row.grant, RotateOK, nil
}

func (m *MemoryRefreshStore) RevokeFamily(_ context.Context, familyID string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.revokeLocked(familyID)
	return nil
}

func (m *MemoryRefreshStore) revokeLocked(familyID string) {
	for _, r := range m.rows {
		if r.grant.FamilyID == familyID {
			r.revoked = true
		}
	}
}

// Len 은 저장된 토큰 행 수다(테스트용).
func (m *MemoryRefreshStore) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows)
}
