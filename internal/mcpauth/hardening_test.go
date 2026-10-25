package mcpauth

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ChatGPT 가 https://chatgpt.com/oauth/client.json 으로 실제 서빙하는 문서(바이트 그대로).
const chatGPTRealDocument = `{"client_id":"https://chatgpt.com/oauth/client.json","client_uri":"https://chatgpt.com/","redirect_uris":["https://chatgpt.com/connector_platform_oauth_redirect"],"token_endpoint_auth_method":"private_key_jwt","token_endpoint_auth_methods_supported":["none","private_key_jwt"],"grant_types":["authorization_code","refresh_token"],"response_types":["code"],"client_name":"ChatGPT","logo_uri":"https://persistent.oaistatic.com/sonic/misc/openai-logo.png","token_endpoint_auth_signing_alg":"RS256","jwks_uri":"https://chatgpt.com/oauth/jwks.json"}`

const chatGPTClientID = "https://chatgpt.com/oauth/client.json"

// ---------------------------------------------------------------------------
// B. 실제 ChatGPT 문서
// ---------------------------------------------------------------------------

func TestCIMD_ChatGPTRealDocumentIsAccepted(t *testing.T) {
	t.Parallel()
	hosts := map[string]bool{"chatgpt.com": true}
	c, err := parseCIMDDocument(chatGPTClientID, []byte(chatGPTRealDocument), hosts)
	if err != nil {
		t.Fatalf("real ChatGPT document rejected: %v", err)
	}
	if c.ID != chatGPTClientID || c.Name != "ChatGPT" || len(c.RedirectURIs) != 1 ||
		c.RedirectURIs[0] != "https://chatgpt.com/connector_platform_oauth_redirect" {
		t.Fatalf("client = %+v", c)
	}
}

func TestCIMD_AuthMethodNegotiationRules(t *testing.T) {
	t.Parallel()
	hosts := map[string]bool{"chatgpt.com": true}
	mk := func(single any, supported any) []byte {
		return cimdDoc(cimdID, func(m map[string]any) {
			delete(m, "token_endpoint_auth_method")
			if single != nil {
				m["token_endpoint_auth_method"] = single
			}
			if supported != nil {
				m["token_endpoint_auth_methods_supported"] = supported
			}
		})
	}
	ok := map[string][]byte{
		"singular none":                      mk("none", nil),
		"singular absent":                    mk(nil, nil),
		"singular pkjwt but none in list":    mk("private_key_jwt", []string{"none", "private_key_jwt"}),
		"singular none, list without none":   mk("none", []string{"private_key_jwt"}),
		"singular absent, list without none": mk(nil, []string{"private_key_jwt"}),
		"jwks + signing alg ignored": cimdDoc(cimdID, func(m map[string]any) {
			m["jwks"] = map[string]any{"keys": []any{}}
			m["jwks_uri"] = "https://chatgpt.com/jwks"
			m["token_endpoint_auth_signing_alg"] = "RS256"
		}),
	}
	for name, doc := range ok {
		if _, err := parseCIMDDocument(cimdID, doc, hosts); err != nil {
			t.Errorf("%s: rejected: %v", name, err)
		}
	}
	bad := map[string][]byte{
		"pkjwt only (no none anywhere)": mk("private_key_jwt", nil),
		"pkjwt with list lacking none":  mk("private_key_jwt", []string{"private_key_jwt"}),
		"client_secret_basic singular":  mk("client_secret_basic", []string{"client_secret_basic"}),
		"real doc plus client_secret":   []byte(strings.Replace(chatGPTRealDocument, `"client_name"`, `"client_secret":"x","client_name"`, 1)),
	}
	for name, doc := range bad {
		if _, err := parseCIMDDocument(cimdID, doc, hosts); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCIMD_RealDocumentFullFlowAndNoAssertionAtToken(t *testing.T) {
	t.Parallel()
	ff := &fakeCIMD{docs: map[string][]byte{chatGPTClientID: []byte(chatGPTRealDocument)}}
	f := newCIMDFixture(t, ff)
	cb := "https://chatgpt.com/connector_platform_oauth_redirect"

	q := authzQuery(set("client_id", chatGPTClientID), set("redirect_uri", cb))
	rec := f.get(PathAuthorize + "?" + q.Encode())
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ChatGPT") {
		t.Fatalf("consent: %d %s", rec.Code, rec.Body.String())
	}
	code := f.approve(q, testOwnerKey)

	// 클라이언트 어서션을 보내면 거절한다(광고하지 않은 방식).
	tf := tokenForm(code, set("client_id", chatGPTClientID), set("redirect_uri", cb),
		set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"), set("client_assertion", "a.b.c"))
	status, out, _ := f.token(tf)
	if status != http.StatusBadRequest || out.Error != "invalid_client" {
		t.Fatalf("assertion at /token: %d %q", status, out.Error)
	}
	// 어서션 거절은 코드를 소모하지 않는다(인증 단계에서 끝남): 어서션 없이 교환된다.
	status, tok, _ := f.token(tokenForm(code, set("client_id", chatGPTClientID), set("redirect_uri", cb)))
	if status != http.StatusOK || tok.RefreshToken == "" {
		t.Fatalf("exchange without assertion: %d %+v", status, tok)
	}
	if status, _, _ := f.token(refreshForm(tok.RefreshToken, set("client_id", chatGPTClientID))); status != http.StatusOK {
		t.Fatalf("refresh: %d", status)
	}
	status, out, _ = f.token(refreshForm(tok.RefreshToken, set("client_id", chatGPTClientID), set("client_assertion", "a.b.c")))
	if status != http.StatusBadRequest || out.Error != "invalid_client" {
		t.Fatalf("assertion on refresh: %d %q", status, out.Error)
	}
}

// ---------------------------------------------------------------------------
// C6. CIMD 기아·캐시 오염, 토큰 엔드포인트의 라이브 조회 금지
// ---------------------------------------------------------------------------

func TestCIMD_TokenEndpointNeverFetches(t *testing.T) {
	t.Parallel()
	ff := &fakeCIMD{docs: map[string][]byte{cimdID: cimdDoc(cimdID)}}
	f := newCIMDFixture(t, ff)
	code := f.approve(cimdQuery(cimdID), testOwnerKey)
	status, tok, _ := f.token(tokenForm(code, set("client_id", cimdID), set("redirect_uri", cimdCallback)))
	if status != http.StatusOK {
		t.Fatalf("exchange: %d", status)
	}
	before := ff.calls.Load()

	// 문서 서버가 죽고 캐시도 24시간 넘게 낡아도 갱신은 된다.
	ff.err = errors.New("down")
	f.clock.Add(cimdCacheTTL + cimdStaleTTL + time.Hour)
	if status, _, _ := f.token(refreshForm(tok.RefreshToken, set("client_id", cimdID))); status != http.StatusOK {
		t.Fatalf("refresh with document server down: %d", status)
	}
	if ff.calls.Load() != before {
		t.Fatalf("token endpoint fetched the client document (%d -> %d calls)", before, ff.calls.Load())
	}
	// 형태가 맞지 않는 client_id 는 여전히 invalid_client.
	if status, out, _ := f.token(refreshForm(tok.RefreshToken, set("client_id", "https://evil.example.com/oauth/client.json"))); status != http.StatusBadRequest || out.Error != "invalid_client" {
		t.Fatalf("foreign client_id: %d %q", status, out.Error)
	}
}

func TestCIMD_RandomClientIDsCannotEvictGoodEntries(t *testing.T) {
	t.Parallel()
	ff := &fakeCIMD{docs: map[string][]byte{cimdID: cimdDoc(cimdID)}}
	reg, err := newCIMDRegistry(defaultCIMDPatterns, []string{"chatgpt.com"}, ff.fetch, time.Now, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	reg.limiter.perKeyMax, reg.limiter.globalMax = 1<<30, 1<<30 // 이 테스트는 캐시만 본다
	ctx := context.Background()
	if _, ok := reg.Lookup(ctx, cimdID); !ok {
		t.Fatal("seed failed")
	}
	for i := 0; i < cimdCacheMax*3; i++ {
		reg.Lookup(ctx, fmt.Sprintf("https://chatgpt.com/oauth/rnd%d/client.json", i)) // 모두 실패(부정 캐시)
	}
	calls := ff.calls.Load()
	if _, ok := reg.Lookup(ctx, cimdID); !ok || ff.calls.Load() != calls {
		t.Fatalf("good entry was evicted by negative entries (calls %d -> %d)", calls, ff.calls.Load())
	}
	reg.mu.Lock()
	n := len(reg.cache)
	reg.mu.Unlock()
	if n > cimdCacheMax {
		t.Fatalf("cache grew to %d (> %d)", n, cimdCacheMax)
	}
}

func TestCIMD_FetchRateLimited(t *testing.T) {
	t.Parallel()
	ff := &fakeCIMD{docs: map[string][]byte{cimdID: cimdDoc(cimdID)}}
	f := newCIMDFixture(t, ff)
	// 같은 IP 가 서로 다른 client_id 로 계속 요청해도 가져오기는 분당 cimdFetchesPerIP 회뿐이다.
	for i := 0; i < 40; i++ {
		f.get(PathAuthorize + "?" + cimdQuery(fmt.Sprintf("https://chatgpt.com/oauth/z%d/client.json", i)).Encode())
	}
	if got := ff.calls.Load(); got != cimdFetchesPerIP {
		t.Fatalf("fetches = %d, want %d (per-IP limit)", got, cimdFetchesPerIP)
	}
	// 제한에 걸린 요청은 부정 결과를 캐시하지 않는다: 제한이 풀리면 곧바로 쓸 수 있다.
	f.clock.Add(cimdFetchWindow + time.Second)
	if rec := f.get(PathAuthorize + "?" + cimdQuery(cimdID).Encode()); rec.Code != http.StatusOK {
		t.Fatalf("after window: %d", rec.Code)
	}
}

func TestCIMD_FetchGlobalLimitAcrossIPs(t *testing.T) {
	t.Parallel()
	ff := &fakeCIMD{}
	f := newCIMDFixture(t, ff)
	for i := 0; i < 200; i++ {
		req := httptest.NewRequest(http.MethodGet, PathAuthorize+"?"+cimdQuery(fmt.Sprintf("https://chatgpt.com/oauth/g%d/client.json", i)).Encode(), nil)
		req.RemoteAddr = fmt.Sprintf("198.51.100.%d:1", i%250+1)
		f.do(req)
	}
	if got := ff.calls.Load(); got > cimdFetchesGlobal {
		t.Fatalf("global fetches = %d, want <= %d", got, cimdFetchesGlobal)
	}
}

func TestCIMD_StalePositiveServedWhenRateLimitedOrDown(t *testing.T) {
	t.Parallel()
	ff := &fakeCIMD{docs: map[string][]byte{cimdID: cimdDoc(cimdID)}}
	f := newCIMDFixture(t, ff)
	if rec := f.get(PathAuthorize + "?" + cimdQuery(cimdID).Encode()); rec.Code != http.StatusOK {
		t.Fatal("seed")
	}
	f.clock.Add(cimdCacheTTL + time.Second)
	// 같은 IP 가 제한을 소진한다.
	for i := 0; i < 10; i++ {
		f.get(PathAuthorize + "?" + cimdQuery(fmt.Sprintf("https://chatgpt.com/oauth/s%d/client.json", i)).Encode())
	}
	if rec := f.get(PathAuthorize + "?" + cimdQuery(cimdID).Encode()); rec.Code != http.StatusOK {
		t.Fatalf("stale positive entry not served under rate limit: %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// C7. 발급 도중 코드 재사용
// ---------------------------------------------------------------------------

// replayOnCreateStore 는 Create 가 불리는 순간 같은 코드를 다시 교환하는 저장소다.
type replayOnCreateStore struct {
	*MemoryRefreshStore
	onCreate func()
}

func (r *replayOnCreateStore) Create(ctx context.Context, g RefreshGrant, h []byte, exp, now time.Time) error {
	if r.onCreate != nil {
		r.onCreate()
	}
	return r.MemoryRefreshStore.Create(ctx, g, h, exp, now)
}

func TestToken_CodeReplayDuringIssuanceRevokesAndWithholdsTokens(t *testing.T) {
	t.Parallel()
	mem := NewMemoryRefreshStore()
	rs := &replayOnCreateStore{MemoryRefreshStore: mem}
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	cfgp, _ := ConfigFromEnv(envOf(validEnv()))
	cfg := *cfgp
	cfg.OwnerKeySHA256 = mustHash(testOwnerKey)
	srv, err := NewServer(cfg, rs, WithClock(clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, srv: srv, h: srv.Handler(testStaticKey, protectedStub), clock: clock, store: mem, cfg: cfg}

	code := f.approve(authzQuery(), testOwnerKey)
	var replayStatus int
	rs.onCreate = func() { // 첫 교환이 저장소에 쓰는 동안 탈취자가 같은 코드를 낸다
		rs.onCreate = nil
		replayStatus, _, _ = f.token(tokenForm(code))
	}
	status, out, _ := f.token(tokenForm(code))
	if replayStatus != http.StatusBadRequest {
		t.Fatalf("replay status = %d", replayStatus)
	}
	if status == http.StatusOK || out.AccessToken != "" || out.RefreshToken != "" {
		t.Fatalf("first exchange issued tokens although the code was replayed during issuance: %d %+v", status, out)
	}
	// 만들어 둔 리프레시 토큰은 폐기됐다.
	for _, row := range mem.rows {
		if !row.revoked {
			t.Fatal("a refresh token of the replayed code is still live")
		}
	}
}

// ---------------------------------------------------------------------------
// C3. 정적 키가 아닌 주체의 본문 상한
// ---------------------------------------------------------------------------

func TestGate_BodyCapForNonStaticPrincipals(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	login := f.login()

	var gotErr error
	var gotLen int
	reader := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		gotErr, gotLen = err, len(b)
	})
	h := f.srv.Gate(testStaticKey, reader)
	big := strings.NewReader(strings.Repeat("a", NonStaticBodyMaxBytes+1024))
	small := strings.NewReader(strings.Repeat("a", 1024))

	do := func(body io.Reader, auth string) {
		req := httptest.NewRequest(http.MethodPost, "/mcp", body)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		gotErr, gotLen = nil, 0
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	var mbe *http.MaxBytesError
	for name, auth := range map[string]string{"anonymous": "", "oauth": "Bearer " + login.AccessToken} {
		do(big, auth)
		big = strings.NewReader(strings.Repeat("a", NonStaticBodyMaxBytes+1024))
		if !errors.As(gotErr, &mbe) || gotLen > NonStaticBodyMaxBytes {
			t.Errorf("%s: err=%v len=%d, want MaxBytesError at the cap", name, gotErr, gotLen)
		}
		do(small, auth)
		small = strings.NewReader(strings.Repeat("a", 1024))
		if gotErr != nil || gotLen != 1024 {
			t.Errorf("%s: small body: err=%v len=%d", name, gotErr, gotLen)
		}
	}
	// 정적 키는 이 상한을 받지 않는다(add_note 용으로 더 큰 상한은 호출 쪽이 건다).
	do(big, "Bearer "+testStaticKey)
	if gotErr != nil || gotLen != NonStaticBodyMaxBytes+1024 {
		t.Errorf("static key: err=%v len=%d, want the full body", gotErr, gotLen)
	}
}

// ---------------------------------------------------------------------------
// C4. 소수 IP 로 소유자를 잠글 수 없다
// ---------------------------------------------------------------------------

func TestRateLimit_FewIPsCannotLockOutOwner(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for ip := 1; ip <= 4; ip++ { // 4개 IP x 5회 = 20회 실패(예전 전체 상한)
		for j := 0; j < attemptsPerIP; j++ {
			f.attempt("wrong", fmt.Sprintf("203.0.113.%d:1", ip), nil)
		}
	}
	if rec := f.attempt(testOwnerKey, "198.51.100.200:1", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("owner locked out by 4 attacker IPs: %d", rec.Code)
	}
	if attemptsGlobally < 500 {
		t.Fatalf("global cap = %d, want a high cap that only stops mass attacks", attemptsGlobally)
	}
}

// ---------------------------------------------------------------------------
// C5. nonce 저장소
// ---------------------------------------------------------------------------

func TestConsent_DenyDoesNotTouchNonceStoreOrBudget(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for i := 0; i < 50; i++ {
		form := f.openConsent(authzQuery())
		form.Set("action", "deny")
		if rec := f.postForm(PathAuthorize, form, "198.51.100.9:1"); rec.Code != http.StatusSeeOther {
			t.Fatalf("deny %d: %d", i, rec.Code)
		}
	}
	f.srv.nonces.mu.Lock()
	n := len(f.srv.nonces.used)
	f.srv.nonces.mu.Unlock()
	if n != 0 {
		t.Fatalf("deny recorded %d nonces", n)
	}
	// 같은 IP 의 시도 예산도 쓰지 않았다.
	if rec := f.attempt(testOwnerKey, "198.51.100.9:1", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("approve after denies: %d", rec.Code)
	}
}

func TestConsent_LimiterRunsBeforeNonceConsumption(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const ip = "198.51.100.77:1"
	for i := 0; i < attemptsPerIP; i++ {
		f.attempt("wrong", ip, nil)
	}
	f.srv.nonces.mu.Lock()
	before := len(f.srv.nonces.used)
	f.srv.nonces.mu.Unlock()
	form := f.openConsent(authzQuery())
	form.Set("owner_key", "wrong")
	if rec := f.postForm(PathAuthorize, form, ip); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked-out status = %d", rec.Code)
	}
	f.srv.nonces.mu.Lock()
	after := len(f.srv.nonces.used)
	f.srv.nonces.mu.Unlock()
	if after != before {
		t.Fatalf("a rate-limited request consumed a nonce (%d -> %d)", before, after)
	}
}

func TestNonceStore_EvictsOldestInsteadOfFailing(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	n := newNonceStore(clock.Now)
	exp := clock.Now().Add(formTokenTTL)
	for i := 0; i < maxNonces+500; i++ {
		if !n.consume(fmt.Sprintf("n%d", i), exp) {
			t.Fatalf("consume %d failed (store must evict, not fail closed)", i)
		}
	}
	if len(n.used) > maxNonces {
		t.Fatalf("store holds %d nonces, cap %d", len(n.used), maxNonces)
	}
	if n.consume(fmt.Sprintf("n%d", maxNonces+499), exp) {
		t.Error("recent nonce accepted twice")
	}
	if !n.consume("n0", exp) { // 가장 오래된 것은 축출되었다
		t.Error("oldest nonce should have been evicted")
	}
	// 만료된 항목은 앞에서부터 정리된다.
	clock.Add(formTokenTTL + time.Second)
	n.consume("fresh", clock.Now().Add(formTokenTTL))
	if len(n.used) != 1 {
		t.Errorf("expired nonces not pruned: %d left", len(n.used))
	}
}

func TestNonceStore_ConcurrentConsumeOnce(t *testing.T) {
	t.Parallel()
	n := newNonceStore(time.Now)
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if n.consume("same", time.Now().Add(time.Minute)) {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 {
		t.Fatalf("%d consumers won, want 1", ok.Load())
	}
}

// ---------------------------------------------------------------------------
// C9. 서명 키 엔트로피
// ---------------------------------------------------------------------------

func TestConfigFromEnv_SigningKeyEntropy(t *testing.T) {
	t.Parallel()
	b64 := func(n int) string { return base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", n))) }
	withKey := func(k string) (*Config, error) {
		env := validEnv()
		env[EnvSigningKey] = k
		return ConfigFromEnv(envOf(env))
	}
	// base64 처럼 보이면 디코딩한 길이를 본다: 문자열은 길어도 실제 엔트로피가 모자라면 거절.
	for name, k := range map[string]string{
		"base64 of 24 bytes (32 chars)": b64(24),
		"base64 of 30 bytes (40 chars)": b64(30),
		"hex of 20 bytes (40 chars)":    hex.EncodeToString([]byte(strings.Repeat("x", 20))),
		"hex of 31 bytes (62 chars)":    hex.EncodeToString([]byte(strings.Repeat("x", 31))),
	} {
		if _, err := withKey(k); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), k) {
			t.Errorf("%s: error leaks the key", name)
		}
	}
	// 충분한 경우: 경고 없음.
	for name, k := range map[string]string{
		"base64 of 32 bytes": b64(32),
		"base64 of 48 bytes": b64(48),
		"hex of 32 bytes":    hex.EncodeToString([]byte(strings.Repeat("x", 32))),
	} {
		cfg, err := withKey(k)
		if err != nil || len(cfg.Warnings) != 0 {
			t.Errorf("%s: err=%v warnings=%v", name, err, cfg)
		}
	}
	// base64/hex 가 아니면 확인할 수 없으므로 경고만 한다.
	cfg, err := withKey("this is a long passphrase-like signing key 123!")
	if err != nil || len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "openssl rand") {
		t.Fatalf("passphrase: err=%v warnings=%v", err, cfg.Warnings)
	}
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "passphrase") {
			t.Error("warning leaks the key")
		}
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func TestCIMD_NegativeEntriesNeverEvictFullPositiveCache(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	fetch := func(_ context.Context, u string) ([]byte, error) {
		calls.Add(1)
		if strings.Contains(u, "/oauth/ok") {
			return cimdDoc(u), nil
		}
		return nil, errors.New("nope")
	}
	reg, err := newCIMDRegistry(defaultCIMDPatterns, []string{"chatgpt.com"}, fetch, time.Now, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	reg.limiter.perKeyMax, reg.limiter.globalMax = 1<<30, 1<<30
	reg.sem = make(chan struct{}, 1<<10)
	ctx := context.Background()
	good := func(i int) string { return fmt.Sprintf("https://chatgpt.com/oauth/ok%d/client.json", i) }
	for i := 0; i < cimdCacheMax; i++ {
		if _, ok := reg.Lookup(ctx, good(i)); !ok {
			t.Fatalf("seed %d failed", i)
		}
	}
	for i := 0; i < 200; i++ {
		reg.Lookup(ctx, fmt.Sprintf("https://chatgpt.com/oauth/bad%d/client.json", i))
	}
	calls.Store(0)
	for i := 0; i < cimdCacheMax; i++ {
		if _, ok := reg.Lookup(ctx, good(i)); !ok {
			t.Fatalf("good entry %d lost", i)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("%d good entries were evicted by failing lookups", calls.Load())
	}
}
