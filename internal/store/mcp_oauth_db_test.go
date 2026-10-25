package store

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/baekenough/second-brain/internal/mcpauth"
)

// 실제 Postgres 에서 migrations/042 와 MCPOAuthStore 의 회전·재사용 탐지를 검증한다.
// TEST_DATABASE_URL 이 없으면 건너뛴다(다른 *_db_test.go 와 같다). TestMain 이
// 모든 마이그레이션을 먼저 적용하므로 042 도 이미 실행된 상태다.

func mcpOAuthTestStore(t *testing.T) (*MCPOAuthStore, *Postgres) {
	t.Helper()
	pg := upsertChunksTestDB(t)
	t.Cleanup(func() {
		_, _ = pg.pool.Exec(context.Background(),
			`DELETE FROM mcp_oauth_refresh_tokens WHERE client_id LIKE 'zz-test-%'`)
	})
	return NewMCPOAuthStore(pg), pg
}

func hash32(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func newGrant() mcpauth.RefreshGrant {
	return mcpauth.RefreshGrant{
		FamilyID: uuid.NewString(), ClientID: "zz-test-" + uuid.NewString()[:8],
		Scope: "brain:read", Resource: "https://brain-mcp.example.com/mcp",
	}
}

func TestMCPOAuthStore_Migration042Schema(t *testing.T) {
	_, pg := mcpOAuthTestStore(t)
	ctx := context.Background()

	cols := map[string]string{}
	rows, err := pg.pool.Query(ctx,
		`SELECT column_name, data_type FROM information_schema.columns WHERE table_name = 'mcp_oauth_refresh_tokens'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n, ty string
		if err := rows.Scan(&n, &ty); err != nil {
			t.Fatal(err)
		}
		cols[n] = ty
	}
	for name, ty := range map[string]string{
		"token_hash": "bytea", "family_id": "uuid", "client_id": "text", "scope": "text", "resource": "text",
		"issued_at": "timestamp with time zone", "expires_at": "timestamp with time zone",
		"used_at": "timestamp with time zone", "revoked_at": "timestamp with time zone",
	} {
		if cols[name] != ty {
			t.Errorf("column %s type = %q, want %q", name, cols[name], ty)
		}
	}
	// 해시가 아닌 값(길이 != 32)은 CHECK 가 막는다: 원문 저장 실수를 구조적으로 차단.
	g := newGrant()
	if _, err := pg.pool.Exec(ctx,
		`INSERT INTO mcp_oauth_refresh_tokens (token_hash, family_id, client_id, scope, resource, expires_at)
		 VALUES ($1, $2::uuid, $3, 's', 'r', now())`, []byte("raw-token-not-a-hash"), g.FamilyID, g.ClientID); err == nil {
		t.Error("a non-32-byte token_hash was accepted")
	}
}

func TestMCPOAuthStore_RotateLifecycle(t *testing.T) {
	s, pg := mcpOAuthTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	g := newGrant()
	h1, h2, h3 := hash32("one-"+g.FamilyID), hash32("two-"+g.FamilyID), hash32("three-"+g.FamilyID)

	if err := s.Create(ctx, g, h1, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}

	// 정상 회전
	got, out, err := s.Rotate(ctx, mcpauth.RotateRequest{PresentedHash: h1, ClientID: g.ClientID, NewHash: h2, NewExpiresAt: now.Add(2 * time.Hour), Now: now})
	if err != nil || out != mcpauth.RotateOK {
		t.Fatalf("rotate: %v %v", out, err)
	}
	if got.FamilyID != g.FamilyID || got.Scope != g.Scope || got.Resource != g.Resource || got.ClientID != g.ClientID {
		t.Fatalf("grant not inherited: %+v", got)
	}

	// 클라이언트 불일치는 소모 없이 거절
	if _, out, _ := s.Rotate(ctx, mcpauth.RotateRequest{PresentedHash: h2, ClientID: "zz-test-other", NewHash: h3, NewExpiresAt: now.Add(time.Hour), Now: now}); out != mcpauth.RotateInvalid {
		t.Fatalf("client mismatch outcome = %v", out)
	}
	// 모르는 토큰
	if _, out, _ := s.Rotate(ctx, mcpauth.RotateRequest{PresentedHash: hash32("unknown"), ClientID: g.ClientID, NewHash: h3, NewExpiresAt: now.Add(time.Hour), Now: now}); out != mcpauth.RotateInvalid {
		t.Fatalf("unknown token outcome = %v", out)
	}

	// 재사용: 이미 회전된 h1 → 계보 전체 폐기
	if _, out, err := s.Rotate(ctx, mcpauth.RotateRequest{PresentedHash: h1, ClientID: g.ClientID, NewHash: h3, NewExpiresAt: now.Add(time.Hour), Now: now}); err != nil || out != mcpauth.RotateReused {
		t.Fatalf("reuse outcome = %v err=%v", out, err)
	}
	// 폐기 UPDATE 가 롤백되지 않고 커밋됐는지: 최신 h2 도 이제 못 쓴다.
	if _, out, _ := s.Rotate(ctx, mcpauth.RotateRequest{PresentedHash: h2, ClientID: g.ClientID, NewHash: h3, NewExpiresAt: now.Add(time.Hour), Now: now}); out != mcpauth.RotateInvalid {
		t.Fatalf("latest token after reuse = %v, want invalid (family revoked)", out)
	}
	var revoked int
	if err := pg.pool.QueryRow(ctx,
		`SELECT count(*) FROM mcp_oauth_refresh_tokens WHERE family_id = $1::uuid AND revoked_at IS NOT NULL`, g.FamilyID).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if revoked != 2 {
		t.Fatalf("revoked rows = %d, want 2", revoked)
	}
	// h3 은 만들어지지 않았다.
	var n int
	_ = pg.pool.QueryRow(ctx, `SELECT count(*) FROM mcp_oauth_refresh_tokens WHERE token_hash = $1`, h3).Scan(&n)
	if n != 0 {
		t.Fatal("a rejected rotation inserted a token")
	}
}

func TestMCPOAuthStore_Expiry(t *testing.T) {
	s, _ := mcpOAuthTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	g := newGrant()
	h1 := hash32("exp-" + g.FamilyID)
	if err := s.Create(ctx, g, h1, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if _, out, _ := s.Rotate(ctx, mcpauth.RotateRequest{PresentedHash: h1, ClientID: g.ClientID, NewHash: hash32("n-" + g.FamilyID), NewExpiresAt: now.Add(time.Hour), Now: now.Add(time.Minute)}); out != mcpauth.RotateInvalid {
		t.Fatalf("expired token outcome = %v", out)
	}
}

func TestMCPOAuthStore_ConcurrentRotateExactlyOnce(t *testing.T) {
	s, pg := mcpOAuthTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	g := newGrant()
	h1 := hash32("conc-" + g.FamilyID)
	if err := s.Create(ctx, g, h1, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	results := map[mcpauth.RotateOutcome]int{}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, out, err := s.Rotate(ctx, mcpauth.RotateRequest{
				PresentedHash: h1, ClientID: g.ClientID,
				NewHash: hash32("conc-new-" + g.FamilyID + string(rune('a'+i))), NewExpiresAt: now.Add(time.Hour), Now: now,
			})
			if err != nil {
				t.Errorf("rotate: %v", err)
				return
			}
			mu.Lock()
			results[out]++
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	// 첫 대기자는 사용 처리된 토큰을 보고 재사용(Reused)으로 계보를 폐기하고, 그 뒤에
	// 깨어난 나머지는 이미 폐기된 계보라 Invalid 를 본다. 어느 쪽이든 성공은 한 번뿐이다.
	if results[mcpauth.RotateOK] != 1 || results[mcpauth.RotateReused] < 1 ||
		results[mcpauth.RotateOK]+results[mcpauth.RotateReused]+results[mcpauth.RotateInvalid] != n {
		t.Fatalf("outcomes = %v, want exactly one OK and at least one reused", results)
	}
	// 재사용 판정으로 계보가 폐기됐으므로 유일한 성공 토큰도 죽어 있다.
	var live int
	_ = pg.pool.QueryRow(ctx,
		`SELECT count(*) FROM mcp_oauth_refresh_tokens WHERE family_id = $1::uuid AND revoked_at IS NULL`, g.FamilyID).Scan(&live)
	if live != 0 {
		t.Fatalf("live rows after concurrent reuse = %d", live)
	}
}

func TestMCPOAuthStore_RevokeFamilyAndPrune(t *testing.T) {
	s, pg := mcpOAuthTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	a, b := newGrant(), newGrant()
	ha, hb := hash32("a-"+a.FamilyID), hash32("b-"+b.FamilyID)
	for _, c := range []struct {
		g mcpauth.RefreshGrant
		h []byte
	}{{a, ha}, {b, hb}} {
		if err := s.Create(ctx, c.g, c.h, now.Add(time.Hour), now); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RevokeFamily(ctx, a.FamilyID, now); err != nil {
		t.Fatal(err)
	}
	if _, out, _ := s.Rotate(ctx, mcpauth.RotateRequest{PresentedHash: ha, ClientID: a.ClientID, NewHash: hash32("a2"), NewExpiresAt: now.Add(time.Hour), Now: now}); out != mcpauth.RotateInvalid {
		t.Fatalf("revoked family still rotates: %v", out)
	}
	if _, out, _ := s.Rotate(ctx, mcpauth.RotateRequest{PresentedHash: hb, ClientID: b.ClientID, NewHash: hash32("b2"), NewExpiresAt: now.Add(time.Hour), Now: now}); out != mcpauth.RotateOK {
		t.Fatalf("other family affected by revoke: %v", out)
	}

	// 오래전에 만료된 행은 다음 Create 때 정리된다.
	old := newGrant()
	hOld := hash32("old-" + old.FamilyID)
	if err := s.Create(ctx, old, hOld, now.Add(-48*time.Hour), now.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	c := newGrant()
	if err := s.Create(ctx, c, hash32("c-"+c.FamilyID), now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = pg.pool.QueryRow(ctx, `SELECT count(*) FROM mcp_oauth_refresh_tokens WHERE token_hash = $1`, hOld).Scan(&n)
	if n != 0 {
		t.Fatal("expired row was not pruned")
	}
}

// C1: 재사용 탐지의 계보 폐기가 동시에 커밋되는 회전의 자식 행을 놓치지 않는다.
//
// 결정적 재현: 회전 트랜잭션 B(최신 토큰 T2 → T3)를 검사 직후 멈춰 세우고, 그 사이에 이미
// 회전된 T1 을 제시하는 A(재사용 탐지, 계보 폐기)를 돌린다. 계보 잠금이 없으면 A 의 UPDATE 는
// B 가 커밋하기 전 스냅숏만 보고 T3 를 폐기하지 못해 T3 가 살아남는다.
func TestMCPOAuthStore_ReuseRevocationCatchesConcurrentRotation(t *testing.T) {
	s, pg := mcpOAuthTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	g := newGrant()
	h1, h2, h3 := hash32("r1-"+g.FamilyID), hash32("r2-"+g.FamilyID), hash32("r3-"+g.FamilyID)
	if err := s.Create(ctx, g, h1, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	rot := func(from, to []byte) mcpauth.RotateOutcome {
		_, out, err := s.Rotate(ctx, mcpauth.RotateRequest{PresentedHash: from, ClientID: g.ClientID, NewHash: to, NewExpiresAt: now.Add(time.Hour), Now: now})
		if err != nil {
			t.Errorf("rotate: %v", err)
		}
		return out
	}
	if rot(h1, h2) != mcpauth.RotateOK { // T1 은 사용됨, 최신은 T2
		t.Fatal("setup rotation failed")
	}

	paused, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.beforeRotateWrite = func() {
		once.Do(func() { close(paused) })
		<-release
	}
	var wg sync.WaitGroup
	var outB, outA mcpauth.RotateOutcome
	wg.Add(1)
	go func() { defer wg.Done(); outB = rot(h2, h3) }() // B
	select {
	case <-paused:
	case <-time.After(10 * time.Second):
		t.Fatal("rotation never reached the hook")
	}
	wg.Add(1)
	go func() { defer wg.Done(); outA = rot(h1, hash32("attacker-"+g.FamilyID)) }() // A: 재사용
	time.Sleep(300 * time.Millisecond)                                              // A 가 먼저 끝나는지/막히는지 갈린다
	close(release)
	wg.Wait()

	if outA != mcpauth.RotateReused {
		t.Fatalf("reuse outcome = %v", outA)
	}
	_ = outB // B 가 OK 든 Invalid 든, 폐기 이후 살아 있는 토큰이 없어야 한다
	var live int
	if err := pg.pool.QueryRow(ctx,
		`SELECT count(*) FROM mcp_oauth_refresh_tokens WHERE family_id = $1::uuid AND revoked_at IS NULL`, g.FamilyID).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Fatalf("%d refresh tokens of the revoked family are still live (revocation raced with rotation)", live)
	}
	if rot(h3, hash32("after-"+g.FamilyID)) == mcpauth.RotateOK {
		t.Fatal("the child created during revocation can still be rotated")
	}
}

// 같은 경합을 비결정적으로 여러 번: 계보를 계속 회전하는 공격자가 폐기를 이기지 못한다.
func TestMCPOAuthStore_RotationLoopCannotOutliveRevocation(t *testing.T) {
	s, pg := mcpOAuthTestStore(t)
	ctx := context.Background()
	for iter := 0; iter < 15; iter++ {
		now := time.Now().UTC()
		g := newGrant()
		cur := hash32(fmt.Sprintf("loop0-%s", g.FamilyID))
		if err := s.Create(ctx, g, cur, now.Add(time.Hour), now); err != nil {
			t.Fatal(err)
		}
		first := cur
		n := hash32("loop1-" + g.FamilyID)
		if _, out, _ := s.Rotate(ctx, mcpauth.RotateRequest{PresentedHash: cur, ClientID: g.ClientID, NewHash: n, NewExpiresAt: now.Add(time.Hour), Now: now}); out != mcpauth.RotateOK {
			t.Fatal("setup")
		}
		cur = n
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { // 공격자: 최신 토큰을 계속 회전
			defer wg.Done()
			c := cur
			for i := 0; i < 25; i++ {
				nx := hash32(fmt.Sprintf("loop-next-%s-%d", g.FamilyID, i))
				_, out, _ := s.Rotate(ctx, mcpauth.RotateRequest{PresentedHash: c, ClientID: g.ClientID, NewHash: nx, NewExpiresAt: now.Add(time.Hour), Now: now})
				if out != mcpauth.RotateOK {
					return
				}
				c = nx
			}
		}()
		go func() { // 소유자 쪽: 낡은 토큰 재사용으로 폐기 유발
			defer wg.Done()
			_, _, _ = s.Rotate(ctx, mcpauth.RotateRequest{PresentedHash: first, ClientID: g.ClientID, NewHash: hash32("x-" + g.FamilyID), NewExpiresAt: now.Add(time.Hour), Now: now})
		}()
		wg.Wait()
		var live int
		_ = pg.pool.QueryRow(ctx,
			`SELECT count(*) FROM mcp_oauth_refresh_tokens WHERE family_id = $1::uuid AND revoked_at IS NULL AND used_at IS NULL`, g.FamilyID).Scan(&live)
		if live != 0 {
			t.Fatalf("iteration %d: %d live unused tokens remain after revocation", iter, live)
		}
	}
}
