package mcpauth

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 테스트 픽스처
// ---------------------------------------------------------------------------

const (
	testStaticKey = "static-api-key-for-tests"
	testVerifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk" // RFC 7636 부록 B 예시
	testClientID  = "dots"
	testState     = "state-xyz"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fixture struct {
	t       *testing.T
	srv     *Server
	h       http.Handler
	clock   *fakeClock
	store   *MemoryRefreshStore
	logs    *bytes.Buffer
	cfg     Config
	nextHit int
}

// protectedStub 은 게이트 뒤의 MCP 핸들러 자리다. 게이트가 실은 주체를 응답
// 헤더로 돌려준다.
var protectedStub = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())
	w.Header().Set("X-Principal", fmt.Sprint(int(p.Kind), ":", strings.Join(p.Scopes, ",")))
	_, _ = w.Write([]byte("mcp-ok"))
})

func newFixture(t *testing.T, mods ...func(*Config)) *fixture {
	t.Helper()
	return newFixtureWith(t, nil, mods...)
}

func newFixtureWith(t *testing.T, extra []Option, mods ...func(*Config)) *fixture {
	t.Helper()
	env := validEnv()
	cfgp, err := ConfigFromEnv(envOf(env))
	if err != nil {
		t.Fatal(err)
	}
	cfg := *cfgp
	cfg.OwnerKeySHA256 = mustHash(testOwnerKey)
	for _, m := range mods {
		m(&cfg)
	}
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	logs := &bytes.Buffer{}
	store := NewMemoryRefreshStore()
	opts := append([]Option{WithClock(clock.Now),
		WithLogger(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))}, extra...)
	srv, err := NewServer(cfg, store, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, srv: srv, h: srv.Handler(testStaticKey, protectedStub), clock: clock,
		store: store, logs: logs, cfg: cfg}
}

func mustHash(key string) [32]byte {
	var out [32]byte
	copy(out[:], mustDecodeHex(ownerHash(key)))
	return out
}

func mustDecodeHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func (f *fixture) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) get(target string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	return f.do(req)
}

func (f *fixture) postForm(target string, form url.Values, remote string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if remote != "" {
		req.RemoteAddr = remote
	}
	return f.do(req)
}

// authzQuery 는 정상 인가 요청 파라미터다. mods 로 필드를 바꾸거나(`key=value`)
// 지운다(`key=` + del).
func authzQuery(mods ...func(url.Values)) url.Values {
	v := url.Values{
		"response_type":         {"code"},
		"client_id":             {testClientID},
		"redirect_uri":          {testRedirect},
		"scope":                 {ScopeRead},
		"state":                 {testState},
		"code_challenge":        {pkceS256(testVerifier)},
		"code_challenge_method": {"S256"},
		"resource":              {testBaseURL + "/mcp"},
	}
	for _, m := range mods {
		m(v)
	}
	return v
}

func set(k, val string) func(url.Values) { return func(v url.Values) { v.Set(k, val) } }
func del(k string) func(url.Values)      { return func(v url.Values) { v.Del(k) } }

var hiddenRe = regexp.MustCompile(`<input type="hidden" name="([^"]*)" value="([^"]*)">`)

// hiddenFields 는 동의 화면의 hidden 필드를 폼 값으로 뽑는다.
func hiddenFields(t *testing.T, body string) url.Values {
	t.Helper()
	out := url.Values{}
	for _, m := range hiddenRe.FindAllStringSubmatch(body, -1) {
		out.Set(m[1], html.UnescapeString(m[2]))
	}
	if out.Get("form_token") == "" {
		t.Fatalf("no form_token in page: %s", body)
	}
	return out
}

func (f *fixture) openConsent(q url.Values) url.Values {
	f.t.Helper()
	rec := f.get(PathAuthorize + "?" + q.Encode())
	if rec.Code != http.StatusOK {
		f.t.Fatalf("consent page status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return hiddenFields(f.t, rec.Body.String())
}

func location(t *testing.T, rec *httptest.ResponseRecorder) *url.URL {
	t.Helper()
	loc := rec.Header().Get("Location")
	if loc == "" {
		t.Fatalf("no Location header (status %d): %s", rec.Code, rec.Body.String())
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// approve 는 동의를 거쳐 인가 코드를 받는다.
func (f *fixture) approve(q url.Values, key string) (code string) {
	f.t.Helper()
	form := f.openConsent(q)
	form.Set("owner_key", key)
	form.Set("action", "approve")
	rec := f.postForm(PathAuthorize, form, "")
	if rec.Code != http.StatusSeeOther {
		f.t.Fatalf("approve status = %d: %s", rec.Code, rec.Body.String())
	}
	u := location(f.t, rec)
	code = u.Query().Get("code")
	if code == "" {
		f.t.Fatalf("no code in %s", u)
	}
	return code
}

func tokenForm(code string, mods ...func(url.Values)) url.Values {
	v := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {testRedirect},
		"client_id":     {testClientID},
		"code_verifier": {testVerifier},
	}
	for _, m := range mods {
		m(v)
	}
	return v
}

func refreshForm(rt string, mods ...func(url.Values)) url.Values {
	v := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {testClientID}}
	for _, m := range mods {
		m(v)
	}
	return v
}

type tokenOut struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
}

func (f *fixture) token(form url.Values) (int, tokenOut, *httptest.ResponseRecorder) {
	f.t.Helper()
	rec := f.postForm(PathToken, form, "")
	var out tokenOut
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		f.t.Fatalf("token response not JSON: %v: %s", err, rec.Body.String())
	}
	return rec.Code, out, rec
}

// login 은 전체 흐름(동의 → 코드 → 토큰)을 거쳐 토큰 한 쌍을 받는다.
func (f *fixture) login() tokenOut {
	f.t.Helper()
	code := f.approve(authzQuery(), testOwnerKey)
	status, out, _ := f.token(tokenForm(code))
	if status != http.StatusOK {
		f.t.Fatalf("token status = %d (%s)", status, out.Error)
	}
	return out
}

// ---------------------------------------------------------------------------
// 메타데이터
// ---------------------------------------------------------------------------

func getJSON(t *testing.T, f *fixture, path string) map[string]any {
	t.Helper()
	rec := f.get(path)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status %d", path, rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("%s: content-type %q", path, ct)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMetadata_ProtectedResource(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, path := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp",
	} {
		m := getJSON(t, f, path)
		if m["resource"] != testBaseURL+"/mcp" {
			t.Errorf("%s: resource = %v", path, m["resource"])
		}
		as, _ := m["authorization_servers"].([]any)
		if len(as) != 1 || as[0] != testBaseURL {
			t.Errorf("%s: authorization_servers = %v", path, m["authorization_servers"])
		}
		if sc, _ := m["scopes_supported"].([]any); len(sc) != 1 || sc[0] != ScopeRead {
			t.Errorf("%s: scopes_supported = %v", path, m["scopes_supported"])
		}
		if bm, _ := m["bearer_methods_supported"].([]any); len(bm) != 1 || bm[0] != "header" {
			t.Errorf("%s: bearer_methods_supported = %v", path, m["bearer_methods_supported"])
		}
	}
}

func TestMetadata_AuthorizationServer(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	prm := getJSON(t, f, "/.well-known/oauth-protected-resource")
	for _, path := range []string{"/.well-known/oauth-authorization-server", "/.well-known/openid-configuration"} {
		m := getJSON(t, f, path)
		// issuer 는 PRM 의 authorization_servers 항목과 글자 그대로 같아야 한다.
		if m["issuer"] != prm["authorization_servers"].([]any)[0] {
			t.Errorf("%s: issuer %v != PRM authorization_servers", path, m["issuer"])
		}
		want := map[string]any{
			"authorization_endpoint":                         testBaseURL + "/authorize",
			"token_endpoint":                                 testBaseURL + "/token",
			"authorization_response_iss_parameter_supported": true,
		}
		for k, v := range want {
			if m[k] != v {
				t.Errorf("%s: %s = %v, want %v", path, k, m[k], v)
			}
		}
		lists := map[string]string{
			"response_types_supported":              "code",
			"code_challenge_methods_supported":      "S256",
			"token_endpoint_auth_methods_supported": "none",
			"scopes_supported":                      ScopeRead,
		}
		for k, v := range lists {
			l, _ := m[k].([]any)
			if len(l) != 1 || l[0] != v {
				t.Errorf("%s: %s = %v, want [%s]", path, k, m[k], v)
			}
		}
		if m["client_id_metadata_document_supported"] != true {
			t.Errorf("%s: client_id_metadata_document_supported = %v, want true", path, m["client_id_metadata_document_supported"])
		}
		gt, _ := m["grant_types_supported"].([]any)
		if len(gt) != 2 || gt[0] != "authorization_code" || gt[1] != "refresh_token" {
			t.Errorf("%s: grant_types_supported = %v", path, gt)
		}
		// 구현하지 않은 기능은 광고하지 않는다.
		for _, k := range []string{"registration_endpoint", "jwks_uri", "userinfo_endpoint",
			"id_token_signing_alg_values_supported", "subject_types_supported"} {
			if _, present := m[k]; present {
				t.Errorf("%s: must not advertise %s", path, k)
			}
		}
	}
}

func TestMetadata_MethodHandling(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, path := range []string{"/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource/mcp"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		if rec := f.do(req); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 405", path, rec.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// /authorize 검증: open redirect, PKCE 다운그레이드, scope, resource, iss
// ---------------------------------------------------------------------------

func TestAuthorize_UnvalidatedRedirectURI_NeverRedirects(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	bad := []string{
		"",
		"https://evil.example.com/cb",
		testRedirect + "/",
		testRedirect + "x",
		testRedirect + ".evil.example.com",
		testRedirect + "?x=1",
		testRedirect + "#frag",
		strings.ToUpper(testRedirect),
		"http://chatgpt.com/connector_platform_oauth_redirect",
		"https://chatgpt.com.evil.example.com/connector_platform_oauth_redirect",
		"https://evil.example.com@chatgpt.com/connector_platform_oauth_redirect",
		"//evil.example.com/cb",
		"javascript:alert(1)",
		"https://chatgpt.com/connector/oauth/{callback_id}",
		" " + testRedirect,
	}
	for _, uri := range bad {
		rec := f.get(PathAuthorize + "?" + authzQuery(set("redirect_uri", uri)).Encode())
		if rec.Code != http.StatusBadRequest {
			t.Errorf("redirect_uri %q: status %d, want 400 error page", uri, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Errorf("redirect_uri %q: redirected to %q", uri, loc)
		}
		if uri != "" && strings.Contains(rec.Body.String(), "evil.example.com") {
			t.Errorf("redirect_uri %q: error page reflects input", uri)
		}
		// POST 경로도 같다.
		form := f.openConsent(authzQuery())
		form.Set("redirect_uri", uri)
		form.Set("owner_key", testOwnerKey)
		prec := f.postForm(PathAuthorize, form, "")
		if prec.Code != http.StatusBadRequest || prec.Header().Get("Location") != "" {
			t.Errorf("POST redirect_uri %q: status %d, location %q", uri, prec.Code, prec.Header().Get("Location"))
		}
	}
}

func TestAuthorize_UnknownClientOrDuplicateParams_ErrorPage(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cases := map[string]string{
		"unknown client": PathAuthorize + "?" + authzQuery(set("client_id", "evil")).Encode(),
		"missing client": PathAuthorize + "?" + authzQuery(del("client_id")).Encode(),
		"duplicate redirect_uri": PathAuthorize + "?" + authzQuery().Encode() +
			"&redirect_uri=" + url.QueryEscape("https://evil.example.com/cb"),
		"duplicate client_id": PathAuthorize + "?" + authzQuery().Encode() + "&client_id=evil",
	}
	for name, target := range cases {
		rec := f.get(target)
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
			t.Errorf("%s: status %d location %q", name, rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestAuthorize_ErrorRedirectsCarryIssAndState(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cases := map[string]struct {
		q       url.Values
		wantErr string
	}{
		"plain method":      {authzQuery(set("code_challenge_method", "plain")), "invalid_request"},
		"missing method":    {authzQuery(del("code_challenge_method")), "invalid_request"},
		"missing challenge": {authzQuery(del("code_challenge")), "invalid_request"},
		"short challenge":   {authzQuery(set("code_challenge", "abc")), "invalid_request"},
		"plain verifier as challenge": {authzQuery(set("code_challenge", testVerifier+"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")),
			"invalid_request"},
		"token response type": {authzQuery(set("response_type", "token")), "unsupported_response_type"},
		"missing response":    {authzQuery(del("response_type")), "unsupported_response_type"},
		"wrong resource":      {authzQuery(set("resource", "https://evil.example.com/mcp")), "invalid_target"},
		"resource w/o path":   {authzQuery(set("resource", testBaseURL)), "invalid_target"},
		"state too long":      {authzQuery(set("state", strings.Repeat("s", stateMaxBytes+1))), "invalid_request"},
	}
	for name, c := range cases {
		rec := f.get(PathAuthorize + "?" + c.q.Encode())
		if rec.Code != http.StatusSeeOther {
			t.Errorf("%s: status %d, want 303", name, rec.Code)
			continue
		}
		u := location(t, rec)
		if got := u.Scheme + "://" + u.Host + u.Path; got != testRedirect {
			t.Errorf("%s: redirected to %q", name, got)
		}
		q := u.Query()
		if q.Get("error") != c.wantErr {
			t.Errorf("%s: error = %q, want %q", name, q.Get("error"), c.wantErr)
		}
		if q.Get("iss") != testBaseURL {
			t.Errorf("%s: iss = %q, want %q (RFC 9207 requires iss on every response)", name, q.Get("iss"), testBaseURL)
		}
		if name != "state too long" && q.Get("state") != testState {
			t.Errorf("%s: state = %q", name, q.Get("state"))
		}
		if q.Get("code") != "" {
			t.Errorf("%s: error response carries a code", name)
		}
	}
}

func TestAuthorize_ScopeHandling(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cases := map[string]string{
		"brain:read":                  ScopeRead,
		"":                            ScopeRead,
		"openid email brain:read":     ScopeRead,
		"openid profile email":        ScopeRead, // 알 수 없는 것뿐이어도 발급 가능한 유일한 범위
		"brain:write":                 ScopeRead, // 쓰기 스코프는 요청해도 얻을 수 없다
		"brain:read brain:write":      ScopeRead,
		"offline_access brain:read x": ScopeRead,
	}
	for requested, want := range cases {
		q := authzQuery(set("scope", requested))
		if requested == "" {
			q = authzQuery(del("scope"))
		}
		code := f.approve(q, testOwnerKey)
		_, out, _ := f.token(tokenForm(code))
		if out.Scope != want {
			t.Errorf("requested %q: token scope = %q, want %q", requested, out.Scope, want)
		}
		claims, err := f.srv.signer.verify(out.AccessToken, f.clock.Now())
		if err != nil || claims.Scope != want {
			t.Errorf("requested %q: jwt scope = %v err=%v", requested, claims, err)
		}
	}
}

func TestAuthorize_ConsentPageContent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := f.get(PathAuthorize + "?" + authzQuery(set("scope", "openid brain:read")).Encode())
	body := rec.Body.String()
	for _, want := range []string{"second-brain", ScopeRead, `name="owner_key"`, `type="password"`, `name="form_token"`, `method="post"`} {
		if !strings.Contains(body, want) {
			t.Errorf("consent page lacks %q", want)
		}
	}
	if strings.Contains(body, "openid") && !strings.Contains(body, `name="scope" value="openid brain:read"`) {
		t.Errorf("page shows non-granted scope outside the hidden round-trip field")
	}
	h := rec.Header()
	if !strings.Contains(h.Get("Cache-Control"), "no-store") || h.Get("X-Frame-Options") != "DENY" ||
		!strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
		h.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("missing hardening headers: %v", h)
	}
}

func TestAuthorize_HTMLInjectionInParamsIsEscaped(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	payload := `"><script>alert(1)</script>`
	rec := f.get(PathAuthorize + "?" + authzQuery(set("state", payload)).Encode())
	if strings.Contains(rec.Body.String(), "<script>") {
		t.Fatal("state reflected unescaped")
	}
	// 왕복해도 원문 그대로 돌아온다(이스케이프가 값을 망가뜨리지 않는다).
	form := hiddenFields(t, rec.Body.String())
	if form.Get("state") != payload {
		t.Fatalf("state round trip = %q", form.Get("state"))
	}
}

// ---------------------------------------------------------------------------
// 동의 제출: 소유자 키, CSRF, 일회용
// ---------------------------------------------------------------------------

func TestConsent_Success_RedirectCarriesCodeStateIss(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	form := f.openConsent(authzQuery())
	form.Set("owner_key", testOwnerKey)
	rec := f.postForm(PathAuthorize, form, "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	u := location(t, rec)
	q := u.Query()
	if q.Get("code") == "" || q.Get("state") != testState || q.Get("iss") != testBaseURL {
		t.Fatalf("redirect query = %v", q)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != testRedirect {
		t.Fatalf("redirected to %q", got)
	}
	if rec.Header().Get("Referrer-Policy") != "no-referrer" || !strings.Contains(rec.Header().Get("Cache-Control"), "no-store") {
		t.Errorf("redirect lacks no-referrer/no-store: %v", rec.Header())
	}
}

func TestConsent_RedirectURIExistingQueryPreserved(t *testing.T) {
	t.Parallel()
	const withQuery = "https://chatgpt.com/connector/oauth/abc?tenant=1"
	f := newFixture(t, func(c *Config) { c.RedirectURIs = append(c.RedirectURIs, withQuery) })
	form := f.openConsent(authzQuery(set("redirect_uri", withQuery)))
	form.Set("owner_key", testOwnerKey)
	u := location(t, f.postForm(PathAuthorize, form, ""))
	q := u.Query()
	if q.Get("tenant") != "1" || q.Get("code") == "" || q.Get("iss") != testBaseURL {
		t.Fatalf("redirect query = %v", q)
	}
}

func TestConsent_WrongKey(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, key := range []string{"", "wrong", testOwnerKey + "x", strings.ToUpper(testOwnerKey), " " + testOwnerKey, ownerHash(testOwnerKey), strings.Repeat("a", 5000)} {
		form := f.openConsent(authzQuery())
		form.Set("owner_key", key)
		rec := f.postForm(PathAuthorize, form, fmt.Sprintf("192.0.2.%d:1", 100+f.nextHit))
		f.nextHit++
		if rec.Code != http.StatusForbidden {
			t.Errorf("key %q: status %d, want 403", truncate(key), rec.Code)
		}
		if rec.Header().Get("Location") != "" {
			t.Errorf("key %q: redirected", truncate(key))
		}
		if key != "" && strings.Contains(rec.Body.String(), key) {
			t.Errorf("key %q: response echoes the submitted key", truncate(key))
		}
		// 실패 응답은 새 폼 토큰을 담은 동의 화면이다.
		if !strings.Contains(rec.Body.String(), `name="form_token"`) {
			t.Errorf("key %q: no retry form", truncate(key))
		}
	}
	// 해시 자체를 키로 내도 통과하지 못한다(저장된 해시가 곧 비밀번호가 되지 않는다).
}

func truncate(s string) string {
	if len(s) > 12 {
		return s[:12] + "..."
	}
	return s
}

func TestConsent_Deny(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	form := f.openConsent(authzQuery())
	form.Set("action", "deny")
	rec := f.postForm(PathAuthorize, form, "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d", rec.Code)
	}
	q := location(t, rec).Query()
	if q.Get("error") != "access_denied" || q.Get("iss") != testBaseURL || q.Get("state") != testState || q.Get("code") != "" {
		t.Fatalf("deny redirect = %v", q)
	}
}

func TestConsent_CSRF(t *testing.T) {
	t.Parallel()
	otherRedirect := "https://chatgpt.com/connector/oauth/other"
	f := newFixture(t, func(c *Config) { c.RedirectURIs = append(c.RedirectURIs, otherRedirect) })

	submit := func(mut func(url.Values)) *httptest.ResponseRecorder {
		form := f.openConsent(authzQuery())
		form.Set("owner_key", testOwnerKey)
		mut(form)
		return f.postForm(PathAuthorize, form, fmt.Sprintf("192.0.2.%d:1", 100+f.nextHit))
	}
	mustReject := func(name string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code == http.StatusSeeOther {
			t.Errorf("%s: accepted (redirect to %s)", name, rec.Header().Get("Location"))
		}
	}

	// 대조군: 변조 없이는 통과한다.
	if rec := submit(func(url.Values) {}); rec.Code != http.StatusSeeOther {
		t.Fatalf("control submit failed: %d", rec.Code)
	}

	mustReject("no token", submit(func(v url.Values) { v.Del("form_token") }))
	mustReject("empty token", submit(func(v url.Values) { v.Set("form_token", "") }))
	mustReject("garbage token", submit(func(v url.Values) { v.Set("form_token", "a.b") }))
	mustReject("tampered mac", submit(func(v url.Values) {
		tok := []byte(v.Get("form_token"))
		if tok[len(tok)-2] == 'A' {
			tok[len(tok)-2] = 'B'
		} else {
			tok[len(tok)-2] = 'A'
		}
		v.Set("form_token", string(tok))
	}))
	// 폼 토큰이 묶인 파라미터를 바꿔치기(다른 허용 redirect_uri, 다른 챌린지, 다른 state)
	mustReject("swapped redirect_uri", submit(func(v url.Values) { v.Set("redirect_uri", otherRedirect) }))
	mustReject("swapped code_challenge", submit(func(v url.Values) {
		v.Set("code_challenge", pkceS256("another-verifier-another-verifier-another-0123"))
	}))
	mustReject("swapped state", submit(func(v url.Values) { v.Set("state", "other") }))
	mustReject("swapped scope", submit(func(v url.Values) { v.Set("scope", "openid") }))

	// 다른 서명 키로 만든 서버의 토큰
	other := newFixture(t, func(c *Config) { c.SigningKey = []byte(strings.Repeat("z", 40)) })
	foreign := other.openConsent(authzQuery()).Get("form_token")
	mustReject("token from another server", submit(func(v url.Values) { v.Set("form_token", foreign) }))

	// 토큰 값을 URL 쿼리로 보내도 소용없다(본문 폼만 본다).
	form := f.openConsent(authzQuery())
	req := httptest.NewRequest(http.MethodPost, PathAuthorize+"?form_token="+url.QueryEscape(form.Get("form_token")), strings.NewReader(
		url.Values{"owner_key": {testOwnerKey}, "response_type": {"code"}, "client_id": {testClientID}, "redirect_uri": {testRedirect},
			"code_challenge": {pkceS256(testVerifier)}, "code_challenge_method": {"S256"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "192.0.2.77:1"
	mustReject("token only in query string", f.do(req))
}

func TestConsent_FormTokenIsOneTime(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	form := f.openConsent(authzQuery())
	form.Set("owner_key", testOwnerKey)
	if rec := f.postForm(PathAuthorize, form, ""); rec.Code != http.StatusSeeOther {
		t.Fatalf("first submit: %d", rec.Code)
	}
	rec := f.postForm(PathAuthorize, form, "")
	if rec.Code == http.StatusSeeOther {
		t.Fatal("replayed form was accepted a second time")
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("replay status = %d, want 400", rec.Code)
	}
}

func TestConsent_FormTokenExpires(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	form := f.openConsent(authzQuery())
	form.Set("owner_key", testOwnerKey)
	f.clock.Add(formTokenTTL + time.Second)
	if rec := f.postForm(PathAuthorize, form, ""); rec.Code == http.StatusSeeOther {
		t.Fatal("expired form token accepted")
	}
}

// ---------------------------------------------------------------------------
// 속도 제한과 클라이언트 IP 판정
// ---------------------------------------------------------------------------

// attempt 는 새 폼을 열어 키를 제출한다. remote 와 hdr 은 연결 주소·헤더다.
func (f *fixture) attempt(key, remote string, hdr map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	form := f.openConsent(authzQuery())
	form.Set("owner_key", key)
	req := httptest.NewRequest(http.MethodPost, PathAuthorize, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = remote
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return f.do(req)
}

func TestRateLimit_PerIP_LockoutAppliesEvenToCorrectKey(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const ip = "198.51.100.7:4000"
	for i := 0; i < attemptsPerIP; i++ {
		if rec := f.attempt("wrong", ip, nil); rec.Code != http.StatusForbidden {
			t.Fatalf("attempt %d: %d", i, rec.Code)
		}
	}
	rec := f.attempt(testOwnerKey, ip, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked out client with correct key: status %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" || rec.Header().Get("Location") != "" {
		t.Fatalf("lockout headers: %v", rec.Header())
	}
	// 잠긴 동안의 요청은 잠금을 연장하지 않는다: 15분이 지나면 풀린다.
	f.clock.Add(attemptWindow + time.Second)
	if rec := f.attempt(testOwnerKey, ip, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("after window: status %d, want 303", rec.Code)
	}
}

func TestRateLimit_OtherIPUnaffected(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for i := 0; i < attemptsPerIP; i++ {
		f.attempt("wrong", "198.51.100.7:1", nil)
	}
	if rec := f.attempt(testOwnerKey, "198.51.100.8:1", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("different IP blocked: %d", rec.Code)
	}
}

func TestRateLimit_SuccessDoesNotConsumeBudget(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const ip = "198.51.100.9:1"
	for i := 0; i < attemptsPerIP-1; i++ {
		f.attempt("wrong", ip, nil)
	}
	for i := 0; i < 3; i++ { // 성공은 예약을 되돌린다
		if rec := f.attempt(testOwnerKey, ip, nil); rec.Code != http.StatusSeeOther {
			t.Fatalf("success %d: %d", i, rec.Code)
		}
	}
	if rec := f.attempt("wrong", ip, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("5th failure: %d", rec.Code)
	}
	if rec := f.attempt(testOwnerKey, ip, nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after 5 failures: %d", rec.Code)
	}
}

func TestRateLimit_GlobalCap(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// 전체 상한은 운영에서 500 이라 테스트에서는 20 으로 낮춰 동작(상한에 닿으면 모두 잠김,
	// 윈도우가 지나면 풀림)만 본다.
	const globalCap = 20
	f.srv.limiter.globalMax = globalCap
	n := 0
	for ip := 1; n < globalCap; ip++ {
		for j := 0; j < attemptsPerIP-1 && n < globalCap; j++ {
			f.attempt("wrong", fmt.Sprintf("203.0.113.%d:1", ip), nil)
			n++
		}
	}
	if rec := f.attempt(testOwnerKey, "203.0.113.250:1", nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("global cap not enforced: %d", rec.Code)
	}
	f.clock.Add(attemptWindow + time.Second)
	if rec := f.attempt(testOwnerKey, "203.0.113.250:1", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("global lockout did not lift: %d", rec.Code)
	}
}

func TestRateLimit_SpoofedHeadersFromUntrustedPeerAreIgnored(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const attacker = "198.51.100.66:1000" // 신뢰 프록시 대역 밖
	for i := 0; i < attemptsPerIP; i++ {
		// 요청마다 다른 CF-Connecting-IP / X-Forwarded-For 를 위조한다.
		hdr := map[string]string{
			"CF-Connecting-IP": fmt.Sprintf("203.0.113.%d", i+1),
			"X-Forwarded-For":  fmt.Sprintf("203.0.113.%d", i+100),
			"X-Real-IP":        fmt.Sprintf("203.0.113.%d", i+200),
		}
		f.attempt("wrong", attacker, hdr)
	}
	hdr := map[string]string{"CF-Connecting-IP": "203.0.113.200", "X-Forwarded-For": "203.0.113.201"}
	if rec := f.attempt(testOwnerKey, attacker, hdr); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("header spoofing bypassed the limiter: status %d", rec.Code)
	}
}

func TestRateLimit_TrustedProxyUsesCFConnectingIP(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const proxy = "172.18.0.1:5555" // 도커 게이트웨이(기본 신뢰 대역)
	cf := func(ip string) map[string]string { return map[string]string{"CF-Connecting-IP": ip} }

	for i := 0; i < attemptsPerIP; i++ {
		f.attempt("wrong", proxy, cf("203.0.113.5"))
	}
	if rec := f.attempt(testOwnerKey, proxy, cf("203.0.113.5")); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("real client IP not limited: %d", rec.Code)
	}
	// 같은 프록시 뒤의 다른 클라이언트는 영향받지 않는다.
	if rec := f.attempt(testOwnerKey, proxy, cf("203.0.113.6")); rec.Code != http.StatusSeeOther {
		t.Fatalf("other client behind proxy blocked: %d", rec.Code)
	}
}

func TestClientKey(t *testing.T) {
	t.Parallel()
	trusted := defaultTrustedProxies
	mk := func(remote string, hdr map[string]string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		return r
	}
	cases := []struct {
		name string
		req  *http.Request
		want string
	}{
		{"untrusted ignores CF header", mk("198.51.100.1:1", map[string]string{"CF-Connecting-IP": "1.2.3.4"}), "198.51.100.1"},
		{"loopback trusted", mk("127.0.0.1:1", map[string]string{"CF-Connecting-IP": "1.2.3.4"}), "1.2.3.4"},
		{"ipv6 loopback trusted", mk("[::1]:1", map[string]string{"CF-Connecting-IP": "1.2.3.4"}), "1.2.3.4"},
		{"docker bridge trusted", mk("172.17.0.1:1", map[string]string{"CF-Connecting-IP": "1.2.3.4"}), "1.2.3.4"},
		{"lan not trusted by default", mk("192.168.0.20:1", map[string]string{"CF-Connecting-IP": "1.2.3.4"}), "192.168.0.20"},
		{"10/8 not trusted by default", mk("10.1.2.3:1", map[string]string{"CF-Connecting-IP": "1.2.3.4"}), "10.1.2.3"},
		{"trusted but header garbage", mk("127.0.0.1:1", map[string]string{"CF-Connecting-IP": "not-an-ip"}), "127.0.0.1"},
		{"trusted but header absent", mk("127.0.0.1:1", nil), "127.0.0.1"},
		{"XFF never used", mk("127.0.0.1:1", map[string]string{"X-Forwarded-For": "1.2.3.4"}), "127.0.0.1"},
		{"ipv6 client grouped by /64", mk("127.0.0.1:1", map[string]string{"CF-Connecting-IP": "2001:db8:1:2:aaaa:bbbb:cccc:dddd"}), "2001:db8:1:2::/64"},
		{"v4-mapped unwrapped", mk("[::ffff:198.51.100.1]:1", nil), "198.51.100.1"},
		{"unparseable remote", mk("garbage", map[string]string{"CF-Connecting-IP": "1.2.3.4"}), "unknown"},
	}
	for _, c := range cases {
		if got := clientKey(c.req, trusted); got != c.want {
			t.Errorf("%s: clientKey = %q, want %q", c.name, got, c.want)
		}
	}
	// IPv6 /64 안에서 주소를 바꿔도 같은 키다.
	a := clientKey(mk("127.0.0.1:1", map[string]string{"CF-Connecting-IP": "2001:db8:1:2::1"}), trusted)
	b := clientKey(mk("127.0.0.1:1", map[string]string{"CF-Connecting-IP": "2001:db8:1:2:ffff::9"}), trusted)
	if a != b {
		t.Errorf("same /64 produced different keys: %q vs %q", a, b)
	}
}

// TestRateLimit_ConcurrentGuessesAreCounted 는 동시에 들어온 추측이 모두 한도를
// 통과하는 경쟁(TOCTOU)이 없음을 확인한다: 키를 비교하는 시도는 정확히 한도만큼이다.
func TestRateLimit_ConcurrentGuessesAreCounted(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const n = 40
	forms := make([]url.Values, n)
	for i := range forms {
		forms[i] = f.openConsent(authzQuery())
		forms[i].Set("owner_key", "wrong")
	}
	var wg sync.WaitGroup
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(form url.Values) {
			defer wg.Done()
			codes <- f.postForm(PathAuthorize, form, "198.51.100.50:1").Code
		}(forms[i])
	}
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for c := range codes {
		counts[c]++
	}
	if counts[http.StatusForbidden] != attemptsPerIP || counts[http.StatusTooManyRequests] != n-attemptsPerIP {
		t.Fatalf("status distribution = %v, want %d x 403 and %d x 429", counts, attemptsPerIP, n-attemptsPerIP)
	}
}

// ---------------------------------------------------------------------------
// 토큰 엔드포인트
// ---------------------------------------------------------------------------

func TestToken_AuthorizationCode_Success(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	code := f.approve(authzQuery(), testOwnerKey)
	status, out, rec := f.token(tokenForm(code))
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, rec.Body.String())
	}
	if out.TokenType != "Bearer" || out.ExpiresIn != 3600 || out.Scope != ScopeRead || out.RefreshToken == "" {
		t.Fatalf("response = %+v", out)
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Pragma") != "no-cache" ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Errorf("headers = %v", rec.Header())
	}
	c, err := f.srv.signer.verify(out.AccessToken, f.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if c.Issuer != testBaseURL || c.Audience != testBaseURL+"/mcp" || c.Subject != "owner" || c.Scope != ScopeRead ||
		c.ID == "" || c.ExpiresAt-c.IssuedAt != 3600 || c.NotBefore != c.IssuedAt {
		t.Fatalf("claims = %+v", c)
	}
	// 리프레시 토큰은 해시로만 저장된다.
	if f.store.Len() != 1 {
		t.Fatalf("store rows = %d", f.store.Len())
	}
	for k := range f.store.rows {
		if strings.Contains(k, out.RefreshToken) {
			t.Fatal("refresh token stored in the clear")
		}
	}
}

func TestToken_CodeBindingFailures(t *testing.T) {
	t.Parallel()
	otherRedirect := "https://chatgpt.com/connector/oauth/other"
	cases := map[string]struct {
		mod     func(url.Values)
		wantErr string
	}{
		"wrong verifier":        {set("code_verifier", "wrong-verifier-wrong-verifier-wrong-verifier-0123"), "invalid_grant"},
		"missing verifier":      {del("code_verifier"), "invalid_grant"},
		"short verifier":        {set("code_verifier", "short"), "invalid_grant"},
		"challenge as verifier": {set("code_verifier", pkceS256(testVerifier)), "invalid_grant"},
		"wrong redirect_uri":    {set("redirect_uri", otherRedirect), "invalid_grant"},
		"missing redirect_uri":  {del("redirect_uri"), "invalid_grant"},
		"wrong resource":        {set("resource", "https://evil.example.com/mcp"), "invalid_target"},
	}
	for name, c := range cases {
		f := newFixture(t, func(cfg *Config) { cfg.RedirectURIs = append(cfg.RedirectURIs, otherRedirect) })
		code := f.approve(authzQuery(), testOwnerKey)
		status, out, _ := f.token(tokenForm(code, c.mod))
		if status != http.StatusBadRequest || out.Error != c.wantErr || out.AccessToken != "" {
			t.Errorf("%s: status %d error %q", name, status, out.Error)
		}
		// 실패한 시도가 코드를 소모했으므로 올바른 검증자로도 이미 늦다.
		if status, out, _ := f.token(tokenForm(code)); status == http.StatusOK {
			t.Errorf("%s: code still usable after a failed attempt (%+v)", name, out)
		}
	}
}

func TestToken_ClientMismatchAndUnknownClient(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	code := f.approve(authzQuery(), testOwnerKey)
	status, out, _ := f.token(tokenForm(code, set("client_id", "evil")))
	if status != http.StatusBadRequest || out.Error != "invalid_client" {
		t.Fatalf("status %d error %q", status, out.Error)
	}
	status, out, _ = f.token(tokenForm(code, del("client_id")))
	if status != http.StatusBadRequest || out.Error != "invalid_client" {
		t.Fatalf("missing client_id: status %d error %q", status, out.Error)
	}
}

func TestToken_PKCEDowngradeNotPossible(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// plain 으로 인가 시도 → 코드 발급 자체가 안 된다.
	rec := f.get(PathAuthorize + "?" + authzQuery(set("code_challenge_method", "plain"), set("code_challenge", testVerifier)).Encode())
	q := location(t, rec).Query()
	if q.Get("code") != "" || q.Get("error") != "invalid_request" {
		t.Fatalf("plain PKCE not rejected: %v", q)
	}
	// 챌린지 없이도 마찬가지다.
	rec = f.get(PathAuthorize + "?" + authzQuery(del("code_challenge"), del("code_challenge_method")).Encode())
	if q := location(t, rec).Query(); q.Get("code") != "" || q.Get("error") != "invalid_request" {
		t.Fatalf("missing PKCE not rejected: %v", q)
	}
	// 동의 폼에서 챌린지를 지워도(토큰 결합 때문에) 코드가 나오지 않는다.
	form := f.openConsent(authzQuery())
	form.Set("owner_key", testOwnerKey)
	form.Del("code_challenge")
	form.Del("code_challenge_method")
	if rec := f.postForm(PathAuthorize, form, ""); rec.Code == http.StatusSeeOther && location(t, rec).Query().Get("code") != "" {
		t.Fatal("PKCE stripped from the consent form still produced a code")
	}
}

func TestToken_CodeExpires(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	code := f.approve(authzQuery(), testOwnerKey)
	f.clock.Add(DefaultCodeTTL + time.Second)
	status, out, _ := f.token(tokenForm(code))
	if status != http.StatusBadRequest || out.Error != "invalid_grant" {
		t.Fatalf("expired code: %d %q", status, out.Error)
	}
	// 대조군: 만료 직전에는 통한다.
	f2 := newFixture(t)
	code2 := f2.approve(authzQuery(), testOwnerKey)
	f2.clock.Add(DefaultCodeTTL - time.Second)
	if status, _, _ := f2.token(tokenForm(code2)); status != http.StatusOK {
		t.Fatalf("code rejected before TTL: %d", status)
	}
}

func TestToken_CodeReplay_RevokesIssuedTokens(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	code := f.approve(authzQuery(), testOwnerKey)
	status, first, _ := f.token(tokenForm(code))
	if status != http.StatusOK {
		t.Fatalf("first exchange: %d", status)
	}
	// 같은 코드를 다시 낸다(탈취자 시나리오).
	status, second, _ := f.token(tokenForm(code))
	if status != http.StatusBadRequest || second.Error != "invalid_grant" || second.AccessToken != "" {
		t.Fatalf("replay: %d %+v", status, second)
	}
	// 첫 교환으로 받은 리프레시 토큰은 폐기됐다.
	status, out, _ := f.token(refreshForm(first.RefreshToken))
	if status != http.StatusBadRequest || out.Error != "invalid_grant" {
		t.Fatalf("refresh token survived code replay: %d %+v", status, out)
	}
}

func TestToken_CodeIsSingleUse_Concurrent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	code := f.approve(authzQuery(), testOwnerKey)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := f.postForm(PathToken, tokenForm(code), "")
			if rec.Code == http.StatusOK {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	// 동시 중복 제시는 재사용으로 판정된다. 발급 도중 재사용이 감지되면 승자도 토큰을 받지
	// 못하고 계보가 폐기된다(C7). 어느 쪽이든 두 번 이상 성공하면 안 된다.
	if ok > 1 {
		t.Fatalf("%d concurrent exchanges succeeded, want at most 1", ok)
	}
}

func TestToken_RequestShape(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	code := f.approve(authzQuery(), testOwnerKey)

	// GET 은 허용되지 않는다.
	if rec := f.get(PathToken); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /token = %d", rec.Code)
	}
	// JSON 본문은 거절한다.
	req := httptest.NewRequest(http.MethodPost, PathToken, strings.NewReader(`{"grant_type":"authorization_code"}`))
	req.Header.Set("Content-Type", "application/json")
	if rec := f.do(req); rec.Code != http.StatusBadRequest {
		t.Errorf("json body = %d", rec.Code)
	}
	// 쿼리스트링의 파라미터는 무시한다.
	req = httptest.NewRequest(http.MethodPost, PathToken+"?"+tokenForm(code).Encode(), nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rec := f.do(req); rec.Code == http.StatusOK {
		t.Error("token parameters accepted from the query string")
	}
	// 같은 파라미터 중복
	req = httptest.NewRequest(http.MethodPost, PathToken, strings.NewReader(tokenForm(code).Encode()+"&code=other"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rec := f.do(req); rec.Code != http.StatusBadRequest {
		t.Errorf("duplicate param = %d", rec.Code)
	}
	// 알 수 없는 grant_type
	if status, out, _ := f.token(url.Values{"grant_type": {"password"}, "client_id": {testClientID}}); status != http.StatusBadRequest || out.Error != "unsupported_grant_type" {
		t.Errorf("password grant: %d %q", status, out.Error)
	}
	if status, out, _ := f.token(url.Values{"grant_type": {"client_credentials"}, "client_id": {testClientID}}); status != http.StatusBadRequest || out.Error != "unsupported_grant_type" {
		t.Errorf("client_credentials grant: %d %q", status, out.Error)
	}
	// 본문 크기 상한
	big := strings.NewReader("grant_type=authorization_code&code=" + strings.Repeat("a", tokenBodyMaxBytes*2))
	req = httptest.NewRequest(http.MethodPost, PathToken, big)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rec := f.do(req); rec.Code != http.StatusBadRequest {
		t.Errorf("oversized body = %d", rec.Code)
	}
	// 대조군: 위 거절들이 코드를 건드리지 않았고(쿼리 파라미터는 무시) 정상 교환은 된다.
	// (JSON 등은 본문 파싱 전에 거절되어 코드를 소모하지 않는다.)
	if status, _, _ := f.token(tokenForm(code)); status != http.StatusOK {
		t.Errorf("control exchange failed: %d", status)
	}
}

// ---------------------------------------------------------------------------
// 리프레시 토큰: 회전, 재사용 탐지
// ---------------------------------------------------------------------------

func TestRefresh_Rotation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	t1 := f.login()
	f.clock.Add(10 * time.Minute)
	status, t2, _ := f.token(refreshForm(t1.RefreshToken))
	if status != http.StatusOK {
		t.Fatalf("refresh: %d %q", status, t2.Error)
	}
	if t2.RefreshToken == "" || t2.RefreshToken == t1.RefreshToken || t2.AccessToken == t1.AccessToken || t2.Scope != ScopeRead {
		t.Fatalf("rotation did not produce fresh tokens: %+v", t2)
	}
	if _, err := f.srv.signer.verify(t2.AccessToken, f.clock.Now()); err != nil {
		t.Fatalf("new access token invalid: %v", err)
	}
	// 새 토큰으로 한 번 더 회전된다.
	status, t3, _ := f.token(refreshForm(t2.RefreshToken))
	if status != http.StatusOK || t3.RefreshToken == t2.RefreshToken {
		t.Fatalf("second rotation: %d %+v", status, t3)
	}
}

func TestRefresh_ReuseRevokesWholeFamily(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	t1 := f.login()
	_, t2, _ := f.token(refreshForm(t1.RefreshToken))
	_, t3, _ := f.token(refreshForm(t2.RefreshToken))

	// 이미 회전된 t1 을 다시 낸다.
	status, out, _ := f.token(refreshForm(t1.RefreshToken))
	if status != http.StatusBadRequest || out.Error != "invalid_grant" {
		t.Fatalf("reuse: %d %q", status, out.Error)
	}
	// 계보의 가장 새 토큰(t3)까지 폐기된다.
	for name, rt := range map[string]string{"t2 (rotated)": t2.RefreshToken, "t3 (latest)": t3.RefreshToken} {
		status, out, _ := f.token(refreshForm(rt))
		if status == http.StatusOK || out.Error != "invalid_grant" {
			t.Errorf("%s still usable after reuse detection: %d %q", name, status, out.Error)
		}
	}
	if !strings.Contains(f.logs.String(), "reuse detected") {
		t.Error("reuse was not logged")
	}
}

func TestRefresh_ReuseDoesNotAffectOtherFamilies(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	a := f.login()
	b := f.login()
	_, a2, _ := f.token(refreshForm(a.RefreshToken))
	f.token(refreshForm(a.RefreshToken)) // 재사용 → 계보 A 폐기
	if status, _, _ := f.token(refreshForm(a2.RefreshToken)); status == http.StatusOK {
		t.Fatal("family A survived")
	}
	if status, _, _ := f.token(refreshForm(b.RefreshToken)); status != http.StatusOK {
		t.Fatalf("family B was revoked by A's reuse: %d", status)
	}
}

func TestRefresh_ConcurrentSameTokenRotatesOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	t1 := f.login()
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if f.postForm(PathToken, refreshForm(t1.RefreshToken), "").Code == http.StatusOK {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("%d concurrent refreshes succeeded, want exactly 1", ok)
	}
}

func TestRefresh_Expiry(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	t1 := f.login()
	f.clock.Add(DefaultRefreshTokenTTL - time.Hour)
	status, t2, _ := f.token(refreshForm(t1.RefreshToken))
	if status != http.StatusOK {
		t.Fatalf("refresh before TTL: %d", status)
	}
	// 회전된 새 토큰은 새 30일을 받는다.
	f.clock.Add(DefaultRefreshTokenTTL - time.Hour)
	if status, _, _ := f.token(refreshForm(t2.RefreshToken)); status != http.StatusOK {
		t.Fatalf("rotated token did not get a fresh TTL: %d", status)
	}
	f2 := newFixture(t)
	old := f2.login()
	f2.clock.Add(DefaultRefreshTokenTTL + time.Second)
	if status, out, _ := f2.token(refreshForm(old.RefreshToken)); status != http.StatusBadRequest || out.Error != "invalid_grant" {
		t.Fatalf("expired refresh token: %d %q", status, out.Error)
	}
}

func TestRefresh_InvalidInputs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	t1 := f.login()
	for name, form := range map[string]url.Values{
		"unknown token":           refreshForm("not-a-real-token"),
		"empty token":             refreshForm(""),
		"wrong client":            refreshForm(t1.RefreshToken, set("client_id", "evil")),
		"access token as refresh": refreshForm(t1.AccessToken),
	} {
		status, out, _ := f.token(form)
		if status == http.StatusOK || out.AccessToken != "" {
			t.Errorf("%s: accepted", name)
		}
	}
	// 잘못된 resource 는 회전 전에 거절되어 토큰을 소모하지 않는다.
	if status, out, _ := f.token(refreshForm(t1.RefreshToken, set("resource", "https://evil.example.com/mcp"))); status != http.StatusBadRequest || out.Error != "invalid_target" {
		t.Errorf("wrong resource: %d %q", status, out.Error)
	}
	if status, _, _ := f.token(refreshForm(t1.RefreshToken)); status != http.StatusOK {
		t.Errorf("token was consumed by a rejected request: %d", status)
	}
}

func TestRefresh_ScopeParamCannotEscalate(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	t1 := f.login()
	status, t2, _ := f.token(refreshForm(t1.RefreshToken, set("scope", "brain:read brain:write")))
	if status != http.StatusOK || t2.Scope != ScopeRead {
		t.Fatalf("scope escalation via refresh: %d %+v", status, t2)
	}
	c, _ := f.srv.signer.verify(t2.AccessToken, f.clock.Now())
	if c == nil || c.Scope != ScopeRead {
		t.Fatalf("claims = %+v", c)
	}
}

// ---------------------------------------------------------------------------
// 리소스 게이트
// ---------------------------------------------------------------------------

func TestGate_NoAuthorizationHeader_PassesThroughAsMixedMode(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// 혼합 모드: 헤더가 없는 initialize/tools/list 는 통과한다(도구 호출 거절은 도구 계층의 몫).
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)),
		httptest.NewRequest(http.MethodGet, "/mcp/sse", nil),
		httptest.NewRequest(http.MethodPost, "/anything", nil),
	} {
		rec := f.do(req)
		if rec.Code != http.StatusOK || rec.Body.String() != "mcp-ok" {
			t.Errorf("%s %s: status %d body %q, want pass-through", req.Method, req.URL.Path, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("X-Principal"); got != fmt.Sprint(int(PrincipalNone), ":") {
			t.Errorf("principal = %q, want none", got)
		}
	}
}

func TestChallenge_Format(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	prm := testBaseURL + "/.well-known/oauth-protected-resource/mcp"
	cases := []struct {
		o    ChallengeOpts
		want string
	}{
		{ChallengeOpts{Error: "invalid_token"}, `Bearer resource_metadata="` + prm + `", error="invalid_token"`},
		{ChallengeOpts{}, `Bearer resource_metadata="` + prm + `"`},
		{ChallengeOpts{Error: "insufficient_scope", Description: "You need to login to continue", Scope: ScopeRead},
			`Bearer resource_metadata="` + prm + `", error="insufficient_scope", error_description="You need to login to continue", scope="brain:read"`},
	}
	for _, c := range cases {
		if got := f.srv.Challenge(c.o); got != c.want {
			t.Errorf("Challenge(%+v) = %q\n want %q", c.o, got, c.want)
		}
	}
}

func TestGate_InvalidCredentials(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	login := f.login()
	otherKey := newFixture(t, func(c *Config) { c.SigningKey = []byte(strings.Repeat("q", 40)) })
	foreign := otherKey.login()

	// 다른 리소스(aud)용으로 같은 키로 발급된 토큰(audience confusion)
	sameKeySigner := &tokenSigner{key: f.cfg.SigningKey, issuer: testBaseURL, resource: testBaseURL + "/other"}
	audConfused, _ := sameKeySigner.sign(Claims{
		Issuer: testBaseURL, Audience: testBaseURL + "/other", Subject: SubjectOwner, Scope: ScopeRead, ID: "x",
		IssuedAt: f.clock.Now().Unix(), NotBefore: f.clock.Now().Unix(), ExpiresAt: f.clock.Now().Add(time.Hour).Unix(),
	})

	// 헤더가 있으면 어떤 모양이든 유효해야 한다. 아니면 401 invalid_token (ChatGPT 가 토큰을 갱신한다).
	want := `Bearer resource_metadata="` + testBaseURL + `/.well-known/oauth-protected-resource/mcp", error="invalid_token"`
	cases := map[string]string{
		"garbage":                   "Bearer garbage",
		"empty bearer":              "Bearer ",
		"basic scheme":              "Basic " + testStaticKey,
		"static key without scheme": testStaticKey,
		"wrong static key":          "Bearer " + testStaticKey + "x",
		"jwt from another key":      "Bearer " + foreign.AccessToken,
		"jwt for another audience":  "Bearer " + audConfused,
		"refresh token":             "Bearer " + login.RefreshToken,
	}
	for name, h := range cases {
		rec := f.get("/mcp", "Authorization", h)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, rec.Code)
			continue
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != want {
			t.Errorf("%s: WWW-Authenticate = %q\n want %q", name, got, want)
		}
		if strings.Contains(rec.Body.String(), "mcp-ok") {
			t.Errorf("%s: request reached the protected handler", name)
		}
	}

	// 만료
	f.clock.Add(time.Hour + time.Second)
	rec := f.get("/mcp", "Authorization", "Bearer "+login.AccessToken)
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != want {
		t.Errorf("expired jwt: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}

func TestGate_ValidCredentials(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	login := f.login()

	rec := f.get("/mcp", "Authorization", "Bearer "+testStaticKey)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("X-Principal"), fmt.Sprint(int(PrincipalStatic))+":") {
		t.Errorf("static key: %d %q", rec.Code, rec.Header().Get("X-Principal"))
	}
	rec = f.get("/mcp", "Authorization", "Bearer "+login.AccessToken)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Principal") != fmt.Sprint(int(PrincipalOAuth), ":", ScopeRead) {
		t.Errorf("jwt: %d %q", rec.Code, rec.Header().Get("X-Principal"))
	}
	// 스킴 이름 대소문자는 구분하지 않는다(RFC 6750).
	rec = f.get("/mcp", "Authorization", "bearer "+login.AccessToken)
	if rec.Code != http.StatusOK {
		t.Errorf("lowercase scheme: %d", rec.Code)
	}
}

func TestPrincipalScopes(t *testing.T) {
	t.Parallel()
	static := Principal{Kind: PrincipalStatic}
	oauth := Principal{Kind: PrincipalOAuth, Scopes: []string{ScopeRead}}
	none := Principal{}
	if !static.HasScope(ScopeRead) || !static.HasScope(ScopeWrite) {
		t.Error("static key must have every scope")
	}
	if !oauth.HasScope(ScopeRead) || oauth.HasScope(ScopeWrite) {
		t.Error("oauth principal must be read-only")
	}
	if none.HasScope(ScopeRead) || none.HasScope(ScopeWrite) {
		t.Error("no principal has no scope")
	}
	if Supports(ScopeWrite) || !Supports(ScopeRead) {
		t.Error("Supports() must advertise read only")
	}
}

// ---------------------------------------------------------------------------
// 로그에 비밀이 남지 않는다
// ---------------------------------------------------------------------------

func TestLogs_NeverContainSecrets(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.attempt("a-wrong-owner-key-attempt", "198.51.100.1:1", nil) // 실패 로그
	form := f.openConsent(authzQuery())
	form.Set("owner_key", testOwnerKey)
	rec := f.postForm(PathAuthorize, form, "")
	code := location(t, rec).Query().Get("code")
	_, tok, _ := f.token(tokenForm(code))
	f.token(tokenForm(code))               // 코드 재사용
	f.token(refreshForm(tok.RefreshToken)) // 회전
	f.token(refreshForm(tok.RefreshToken)) // 재사용
	f.get("/mcp", "Authorization", "Bearer "+tok.AccessToken+"tampered")

	logs := f.logs.String()
	if logs == "" {
		t.Fatal("expected some log output")
	}
	for name, secret := range map[string]string{
		"owner key":          testOwnerKey,
		"wrong owner key":    "a-wrong-owner-key-attempt",
		"owner key hash":     ownerHash(testOwnerKey),
		"signing key":        testSigningKey,
		"static api key":     testStaticKey,
		"auth code":          code,
		"access token":       tok.AccessToken,
		"refresh token":      tok.RefreshToken,
		"form token":         form.Get("form_token"),
		"pkce verifier":      testVerifier,
		"jwt signature part": strings.Split(tok.AccessToken, ".")[2],
	} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs contain the %s", name)
		}
	}
}

// 기본 컨텍스트 함수 경로가 주체를 싣는지(미들웨어가 의존한다).
func TestAuthContextFunc(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	login := f.login()
	fn := f.srv.AuthContextFunc(testStaticKey)
	for hdr, wantKind := range map[string]PrincipalKind{
		"Bearer " + testStaticKey:     PrincipalStatic,
		"Bearer " + login.AccessToken: PrincipalOAuth,
		"Bearer nope":                 PrincipalNone,
		"":                            PrincipalNone,
	} {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if hdr != "" {
			r.Header.Set("Authorization", hdr)
		}
		p, ok := PrincipalFrom(fn(context.Background(), r))
		if !ok || p.Kind != wantKind {
			t.Errorf("%q: kind = %v ok=%v, want %v", truncate(hdr), p.Kind, ok, wantKind)
		}
	}
}
