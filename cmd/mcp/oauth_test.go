package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/baekenough/second-brain/internal/mcpauth"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
)

// ---------------------------------------------------------------------------
// 픽스처: 실제 도구 4개 + OAuth 를 켠 실제 HTTP 핸들러(buildMCPHTTP)
// ---------------------------------------------------------------------------

const (
	e2eStaticKey  = "static-api-key-for-e2e-tests"
	e2eOwnerKey   = "owner-key-for-e2e-tests-0123456789abcdef"
	e2eSigningKey = "signing-key-for-e2e-tests-0123456789abcdef-xyz"
	e2eRedirect   = "https://chatgpt.com/connector_platform_oauth_redirect"
	e2eVerifier   = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
)

type e2eEnv struct {
	t        *testing.T
	ts       *httptest.Server
	base     string
	oauth    *mcpauth.Server
	docs     *fakeOccurredDocSearcher
	upserter *fakeMCPDocUpserter
}

func e2eConfigEnv(base string) func(string) string {
	sum := sha256.Sum256([]byte(e2eOwnerKey))
	m := map[string]string{
		mcpauth.EnvPublicBaseURL:  base,
		mcpauth.EnvRedirectURIs:   e2eRedirect,
		mcpauth.EnvOwnerKeySHA256: hex.EncodeToString(sum[:]),
		mcpauth.EnvSigningKey:     e2eSigningKey,
	}
	return func(k string) string { return m[k] }
}

// newMCPForTest 는 main 과 같은 부품(oauthServerOptions, buildMCPHTTP)으로 서버를
// 조립한다. withOAuth=false 면 OAuth 를 끈 기존 조립이다.
func newMCPForTest(t *testing.T, withOAuth bool) *e2eEnv {
	t.Helper()
	return newMCPForTestWithStore(t, withOAuth, mcpauth.NewMemoryRefreshStore())
}

// chatGPTRealDocument 는 https://chatgpt.com/oauth/client.json 이 실제로 서빙하는 문서(바이트 그대로)다.
const chatGPTRealDocument = `{"client_id":"https://chatgpt.com/oauth/client.json","client_uri":"https://chatgpt.com/","redirect_uris":["https://chatgpt.com/connector_platform_oauth_redirect"],"token_endpoint_auth_method":"private_key_jwt","token_endpoint_auth_methods_supported":["none","private_key_jwt"],"grant_types":["authorization_code","refresh_token"],"response_types":["code"],"client_name":"ChatGPT","logo_uri":"https://persistent.oaistatic.com/sonic/misc/openai-logo.png","token_endpoint_auth_signing_alg":"RS256","jwks_uri":"https://chatgpt.com/oauth/jwks.json"}`

// newMCPForTestWithStore 는 리프레시 토큰 저장소를 바꿔 끼울 수 있는 변형이다.
func newMCPForTestWithStore(t *testing.T, withOAuth bool, rs mcpauth.RefreshStore, srvOpts ...mcpauth.Option) *e2eEnv {
	t.Helper()
	env := &e2eEnv{t: t, docs: &fakeOccurredDocSearcher{}, upserter: &fakeMCPDocUpserter{}}

	ts := httptest.NewUnstartedServer(nil)
	t.Cleanup(ts.Close)
	env.base = "http://" + ts.Listener.Addr().String()

	opts := []mcpserver.ServerOption{mcpserver.WithToolCapabilities(false)}
	if withOAuth {
		cfg, err := mcpauth.ConfigFromEnv(e2eConfigEnv(env.base))
		if err != nil || cfg == nil {
			t.Fatalf("config: %v %v", cfg, err)
		}
		env.oauth, err = mcpauth.NewServer(*cfg, rs, srvOpts...)
		if err != nil {
			t.Fatal(err)
		}
		opts = append(opts, oauthServerOptions(env.oauth)...)
	}
	s := mcpserver.NewMCPServer("second-brain", "1.0.0", opts...)
	registerSearchTool(s, search.NewService(env.docs, disabledEmbeddingEngine{}), false, 0)
	registerGetDocumentTool(s, &fakeDocGetter{})
	registerStatsTool(s, &fakeStatsProvider{})
	registerAddNoteTool(s, env.upserter, &fakeMCPChunkWriter{}, &fakeMCPEmbedder{}, e2eStaticKey)

	custom := &http.Server{}
	_, handler := buildMCPHTTP(s, e2eStaticKey, env.oauth, custom)
	ts.Config.Handler = handler
	ts.Start()
	env.ts = ts
	return env
}

// noRedirect 는 리다이렉트를 따라가지 않는 클라이언트다(Location 을 직접 본다).
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (e *e2eEnv) getJSON(u string) map[string]any {
	e.t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("GET %s = %d", u, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		e.t.Fatal(err)
	}
	return m
}

// rpc 는 MCP streamable HTTP 의 최소 클라이언트다.
type rpc struct {
	t       *testing.T
	url     string
	bearer  string
	session string
	id      int
}

func (c *rpc) post(method string, params any) (*http.Response, map[string]any) {
	c.t.Helper()
	c.id++
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.session = sid
	}
	var msg map[string]any
	payload := raw
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		sc := bufio.NewScanner(bytes.NewReader(raw))
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			if after, ok := strings.CutPrefix(sc.Text(), "data:"); ok {
				payload = []byte(strings.TrimSpace(after))
			}
		}
	}
	_ = json.Unmarshal(payload, &msg)
	return resp, msg
}

func (c *rpc) initialize() (*http.Response, map[string]any) {
	return c.post("initialize", map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "e2e", "version": "0"},
	})
}

func (c *rpc) listTools() []map[string]any {
	c.t.Helper()
	_, msg := c.post("tools/list", map[string]any{})
	res, _ := msg["result"].(map[string]any)
	if res == nil {
		c.t.Fatalf("tools/list: %v", msg)
	}
	var out []map[string]any
	for _, x := range res["tools"].([]any) {
		out = append(out, x.(map[string]any))
	}
	return out
}

func toolNames(tools []map[string]any) []string {
	var names []string
	for _, t := range tools {
		names = append(names, t["name"].(string))
	}
	return names
}

// callResult 는 tools/call 결과를 isError, 본문 텍스트, _meta 로 푼다.
func (c *rpc) callTool(name string, args map[string]any) (isError bool, text string, meta map[string]any) {
	c.t.Helper()
	_, msg := c.post("tools/call", map[string]any{"name": name, "arguments": args})
	res, _ := msg["result"].(map[string]any)
	if res == nil {
		c.t.Fatalf("tools/call %s: no result: %v", name, msg)
	}
	isError, _ = res["isError"].(bool)
	if content, ok := res["content"].([]any); ok && len(content) > 0 {
		text, _ = content[0].(map[string]any)["text"].(string)
	}
	meta, _ = res["_meta"].(map[string]any)
	return
}

var hiddenInput = regexp.MustCompile(`<input type="hidden" name="([^"]*)" value="([^"]*)">`)

type authFlow struct {
	clientID      string
	redirect      string
	authzEndpoint string
	tokenEndpoint string
	issuer        string
}

// challengeFrom 은 도구 결과 _meta["mcp/www_authenticate"] 의 첫 도전 문자열이다.
func challengeFrom(t *testing.T, meta map[string]any) string {
	t.Helper()
	arr, _ := meta["mcp/www_authenticate"].([]any)
	if len(arr) != 1 {
		t.Fatalf("_meta = %v, want one mcp/www_authenticate challenge", meta)
	}
	return arr[0].(string)
}

// discover 는 혼합 모드 디스커버리를 따라간다: 인증 없는 initialize/tools/list 는 통과하고,
// 인증이 필요한 도구 호출이 도구 오류 + _meta["mcp/www_authenticate"] 로 PRM 위치를 알려 주며,
// 그 PRM → AS 메타데이터에서 엔드포인트를 찾는다.
func (e *e2eEnv) discover() authFlow {
	e.t.Helper()
	c := &rpc{t: e.t, url: e.base + "/mcp"}
	resp, msg := c.initialize()
	if resp.StatusCode != http.StatusOK || msg["result"] == nil {
		e.t.Fatalf("unauthenticated initialize = %d %v, want 200 (mixed mode)", resp.StatusCode, msg)
	}
	tools := c.listTools()
	if got := strings.Join(toolNames(tools), ","); got != "get_document,search,stats" {
		e.t.Fatalf("unauthenticated tools/list = %s, want the read tools only (add_note hidden)", got)
	}
	for _, tool := range tools {
		meta, _ := tool["_meta"].(map[string]any)
		schemes, _ := meta["securitySchemes"].([]any)
		if len(schemes) != 1 {
			e.t.Fatalf("%v: _meta.securitySchemes = %v", tool["name"], meta)
		}
		sch := schemes[0].(map[string]any)
		scopes, _ := sch["scopes"].([]any)
		if sch["type"] != "oauth2" || len(scopes) != 1 || scopes[0] != "brain:read" {
			e.t.Fatalf("%v: security scheme = %v", tool["name"], sch)
		}
		// 공식 문서: securitySchemes 는 도구 기술자의 최상위 필드(inputSchema, annotations 와 같은 층).
		top, _ := tool["securitySchemes"].([]any)
		if len(top) != 1 || fmt.Sprint(top[0]) != fmt.Sprint(sch) {
			e.t.Fatalf("%v: top-level securitySchemes = %v, want the same value as _meta", tool["name"], tool["securitySchemes"])
		}
		if _, ok := tool["inputSchema"]; !ok {
			e.t.Fatalf("%v: inputSchema lost in rewrite", tool["name"])
		}
	}
	isErr, _, meta := c.callTool("search", map[string]any{"query": "secret"})
	if !isErr {
		e.t.Fatal("unauthenticated search call succeeded")
	}
	ch := challengeFrom(e.t, meta)
	m := regexp.MustCompile(`resource_metadata="([^"]+)"`).FindStringSubmatch(ch)
	if m == nil || !strings.HasPrefix(ch, "Bearer ") || !strings.Contains(ch, `error="insufficient_scope"`) ||
		!strings.Contains(ch, `scope="brain:read"`) {
		e.t.Fatalf("tool-level challenge = %q", ch)
	}
	prm := e.getJSON(m[1])
	if prm["resource"] != e.base+"/mcp" {
		e.t.Fatalf("PRM resource = %v", prm["resource"])
	}
	servers := prm["authorization_servers"].([]any)
	issuer := servers[0].(string)
	as := e.getJSON(issuer + "/.well-known/oauth-authorization-server")
	if as["issuer"] != issuer {
		e.t.Fatalf("issuer %v != %v", as["issuer"], issuer)
	}
	return authFlow{
		clientID:      "dots",
		redirect:      e2eRedirect,
		authzEndpoint: as["authorization_endpoint"].(string),
		tokenEndpoint: as["token_endpoint"].(string),
		issuer:        issuer,
	}
}

func s256(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorize 는 동의 화면을 거쳐 인가 코드를 받는다. 리다이렉트의 state·iss 를 검증한다.
func (e *e2eEnv) authorize(f authFlow, scope string) (code string) {
	e.t.Helper()
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {f.clientID},
		"redirect_uri":          {f.redirect},
		"scope":                 {scope},
		"state":                 {"st-123"},
		"code_challenge":        {s256(e2eVerifier)},
		"code_challenge_method": {"S256"},
		"resource":              {e.base + "/mcp"},
	}
	resp, err := noRedirect.Get(f.authzEndpoint + "?" + q.Encode())
	if err != nil {
		e.t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(page), "owner_key") {
		e.t.Fatalf("consent page: %d %s", resp.StatusCode, page)
	}
	form := url.Values{}
	for _, m := range hiddenInput.FindAllStringSubmatch(string(page), -1) {
		form.Set(m[1], html.UnescapeString(m[2]))
	}
	form.Set("owner_key", e2eOwnerKey)
	form.Set("action", "approve")

	resp, err = noRedirect.PostForm(f.authzEndpoint, form)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		e.t.Fatalf("consent submit = %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Scheme+"://"+loc.Host+loc.Path != f.redirect {
		e.t.Fatalf("redirected to %s", loc)
	}
	rq := loc.Query()
	if rq.Get("state") != "st-123" || rq.Get("iss") != f.issuer || rq.Get("code") == "" {
		e.t.Fatalf("redirect query = %v", rq)
	}
	return rq.Get("code")
}

type tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
}

func (e *e2eEnv) token(f authFlow, form url.Values) (int, tokens) {
	e.t.Helper()
	resp, err := http.PostForm(f.tokenEndpoint, form)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out tokens
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// ---------------------------------------------------------------------------
// 전체 흐름
// ---------------------------------------------------------------------------

func TestOAuthEndToEnd(t *testing.T) {
	runOAuthEndToEnd(t, newMCPForTest(t, true))
}

// TestOAuthEndToEnd_Postgres 는 같은 흐름을 실제 Postgres 리프레시 저장소(운영과
// 같은 store.MCPOAuthStore)로 돈다. TEST_DATABASE_URL 이 없으면 건너뛴다.
func TestOAuthEndToEnd_Postgres(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres-backed OAuth flow")
	}
	ctx := context.Background()
	pg, err := store.NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pg.Close)
	// 서버 컨테이너가 기동 때 하는 것과 같이 마이그레이션을 적용한다.
	if err := pg.RunMigrations(ctx, filepath.Join("..", "..", "migrations"), 1536); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.Pool().Exec(context.Background(), `DELETE FROM mcp_oauth_refresh_tokens WHERE client_id = 'dots'`)
	})
	runOAuthEndToEnd(t, newMCPForTestWithStore(t, true, store.NewMCPOAuthStore(pg)))
}

func runOAuthEndToEnd(t *testing.T, e *e2eEnv) {
	t.Helper()
	runOAuthFlow(t, e, nil)
}

// TestOAuthEndToEnd_CIMD 는 URL 형태 client_id(Client ID Metadata Document)로 같은 흐름을
// 돈다. 사용자가 콜백 URL 을 붙여 넣을 필요가 없는 경로다.
func TestOAuthEndToEnd_CIMD(t *testing.T) {
	const id = "https://chatgpt.com/oauth/client.json"
	const cb = "https://chatgpt.com/connector_platform_oauth_redirect"
	fetch := func(_ context.Context, u string) ([]byte, error) {
		if u != id {
			return nil, io.EOF
		}
		return []byte(chatGPTRealDocument), nil
	}
	e := newMCPForTestWithStore(t, true, mcpauth.NewMemoryRefreshStore(), mcpauth.WithCIMDFetcher(fetch))
	meta := e.getJSON(e.base + "/.well-known/oauth-authorization-server")
	if meta["client_id_metadata_document_supported"] != true {
		t.Fatalf("AS metadata does not advertise CIMD: %v", meta)
	}
	runOAuthFlow(t, e, func(f *authFlow) { f.clientID, f.redirect = id, cb })
}

func runOAuthFlow(t *testing.T, e *e2eEnv, mod func(*authFlow)) {
	t.Helper()

	// 1) 디스커버리(혼합 모드): 인증 없는 initialize/tools/list 통과 → 도구 호출의
	//    _meta 도전 → PRM → AS 메타데이터.
	f := e.discover()
	if mod != nil {
		mod(&f)
	}

	// 2) 동의 → 코드(+state, iss) → 토큰 교환. ChatGPT 가 OIDC 스코프를 함께 요청해도 된다.
	code := e.authorize(f, "openid email brain:read")
	status, t1 := e.token(f, url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {f.redirect},
		"client_id": {f.clientID}, "code_verifier": {e2eVerifier}, "resource": {e.base + "/mcp"},
	})
	if status != http.StatusOK || t1.TokenType != "Bearer" || t1.Scope != "brain:read" || t1.ExpiresIn != 3600 ||
		t1.AccessToken == "" || t1.RefreshToken == "" {
		t.Fatalf("token response: %d %+v", status, t1)
	}

	// 3) JWT 로 MCP 사용: initialize, tools/list, search.
	c := &rpc{t: t, url: e.base + "/mcp", bearer: t1.AccessToken}
	resp, msg := c.initialize()
	if resp.StatusCode != http.StatusOK || msg["result"] == nil {
		t.Fatalf("initialize with JWT: %d %v", resp.StatusCode, msg)
	}
	tools := c.listTools()
	names := toolNames(tools)
	if strings.Join(names, ",") != "get_document,search,stats" {
		t.Fatalf("OAuth session tools = %v, want only the read tools (add_note hidden)", names)
	}
	for _, tool := range tools {
		meta, _ := tool["_meta"].(map[string]any)
		schemes, _ := meta["securitySchemes"].([]any)
		if len(schemes) != 1 {
			t.Fatalf("%v: _meta.securitySchemes = %v", tool["name"], meta)
		}
		sch := schemes[0].(map[string]any)
		scopes, _ := sch["scopes"].([]any)
		if sch["type"] != "oauth2" || len(scopes) != 1 || scopes[0] != "brain:read" {
			t.Fatalf("%v: security scheme = %v", tool["name"], sch)
		}
	}
	isErr, text, _ := c.callTool("search", map[string]any{"query": "hello"})
	if isErr || !strings.Contains(text, `"query":"hello"`) {
		t.Fatalf("search with JWT: isError=%v text=%s", isErr, text)
	}
	if !e.docs.called {
		t.Fatal("search did not reach the document searcher")
	}
	if isErr, _, _ := c.callTool("stats", nil); isErr {
		t.Error("stats with JWT failed")
	}
	if isErr, text, _ := c.callTool("get_document", map[string]any{"id": "123e4567-e89b-12d3-a456-426614174000"}); isErr {
		t.Errorf("get_document with JWT failed: %s", text)
	}

	// 4) 쓰기 도구는 OAuth 토큰을 거절한다(목록에서 숨겨도 직접 호출은 가능하므로).
	isErr, text, meta := c.callTool("add_note", map[string]any{"title": "x", "content": "y"})
	if !isErr || !strings.Contains(text, "not available to OAuth clients") {
		t.Fatalf("add_note with JWT: isError=%v text=%s", isErr, text)
	}
	if meta != nil {
		t.Errorf("write-tool denial must not carry a re-auth challenge (it cannot be fixed by re-authorizing): %v", meta)
	}
	if e.upserter.lastDoc != nil {
		t.Fatal("add_note wrote a document for an OAuth token")
	}

	// 5) 정적 API 키(hermes)는 그대로: 전 도구가 보이고 add_note 가 동작한다.
	h := &rpc{t: t, url: e.base + "/mcp", bearer: e2eStaticKey}
	if resp, _ := h.initialize(); resp.StatusCode != http.StatusOK {
		t.Fatalf("static key initialize: %d", resp.StatusCode)
	}
	if got := strings.Join(toolNames(h.listTools()), ","); got != "add_note,get_document,search,stats" {
		t.Fatalf("static key tools = %s", got)
	}
	if isErr, text, _ := h.callTool("add_note", map[string]any{"title": "hermes note", "content": "body"}); isErr {
		t.Fatalf("static key add_note: %s", text)
	}
	if e.upserter.lastDoc == nil || e.upserter.lastDoc.Title != "hermes note" {
		t.Fatal("static key add_note did not write")
	}

	// 6) 리프레시 회전.
	status, t2 := e.token(f, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {t1.RefreshToken}, "client_id": {f.clientID}})
	if status != http.StatusOK || t2.RefreshToken == "" || t2.RefreshToken == t1.RefreshToken || t2.AccessToken == t1.AccessToken {
		t.Fatalf("refresh: %d %+v", status, t2)
	}
	c2 := &rpc{t: t, url: e.base + "/mcp", bearer: t2.AccessToken}
	c2.initialize()
	if isErr, _, _ := c2.callTool("search", map[string]any{"query": "again"}); isErr {
		t.Fatal("search with rotated access token failed")
	}

	// 7) 재사용 탐지: 이미 회전된 토큰을 다시 내면 계보 전체가 죽는다.
	status, bad := e.token(f, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {t1.RefreshToken}, "client_id": {f.clientID}})
	if status != http.StatusBadRequest || bad.Error != "invalid_grant" {
		t.Fatalf("reuse: %d %+v", status, bad)
	}
	status, bad = e.token(f, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {t2.RefreshToken}, "client_id": {f.clientID}})
	if status != http.StatusBadRequest || bad.Error != "invalid_grant" {
		t.Fatalf("latest token after reuse: %d %+v", status, bad)
	}
}

// ---------------------------------------------------------------------------
// 게이트·도구 계층 단위
// ---------------------------------------------------------------------------

func TestOAuthGate_InvalidBearerIs401_NoHeaderIsToolLevel(t *testing.T) {
	e := newMCPForTest(t, true)
	want := `Bearer resource_metadata="` + e.base + `/.well-known/oauth-protected-resource/mcp", error="invalid_token"`
	for name, bearer := range map[string]string{"garbage": "garbage", "wrong static": e2eStaticKey + "x"} {
		c := &rpc{t: t, url: e.base + "/mcp", bearer: bearer}
		resp, _ := c.post("tools/call", map[string]any{"name": "search", "arguments": map[string]any{"query": "secret"}})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, resp.StatusCode)
		}
		if got := resp.Header.Get("WWW-Authenticate"); got != want {
			t.Errorf("%s: WWW-Authenticate = %q\n want %q", name, got, want)
		}
	}
	if e.docs.called {
		t.Fatal("a request with an invalid bearer reached the search backend")
	}
	// 헤더가 없는 호출은 HTTP 401 이 아니라 도구 오류이고, 검색 백엔드까지 가지 않는다.
	c := &rpc{t: t, url: e.base + "/mcp"}
	c.initialize()
	isErr, text, meta := c.callTool("search", map[string]any{"query": "secret"})
	if !isErr || !strings.Contains(text, "authentication required") {
		t.Fatalf("unauthenticated call: isError=%v text=%q", isErr, text)
	}
	if ch := challengeFrom(t, meta); !strings.Contains(ch, `resource_metadata="`+e.base+`/.well-known/oauth-protected-resource/mcp"`) {
		t.Fatalf("challenge = %q", ch)
	}
	if e.docs.called {
		t.Fatal("an unauthenticated tool call reached the search backend")
	}
	// 인증 없이는 쓰기 도구도 쓸 수 없고(재인가로 해결 안 됨 → 도전 없음), 아무것도 쓰이지 않는다.
	isErr, text, meta = c.callTool("add_note", map[string]any{"title": "x", "content": "y"})
	if !isErr || meta != nil || e.upserter.lastDoc != nil {
		t.Fatalf("unauthenticated add_note: isError=%v meta=%v wrote=%v", isErr, meta, e.upserter.lastDoc != nil)
	}
}

func TestOAuthToolMiddleware_InsufficientScope(t *testing.T) {
	t.Parallel()
	cfg, _ := mcpauth.ConfigFromEnv(e2eConfigEnv("https://brain-mcp.example.com"))
	as, err := mcpauth.NewServer(*cfg, mcpauth.NewMemoryRefreshStore())
	if err != nil {
		t.Fatal(err)
	}
	var called atomic.Bool
	s := mcpserver.NewMCPServer("t", "0", append(oauthServerOptions(as), mcpserver.WithToolCapabilities(false))...)
	s.AddTool(mcp.NewTool("search"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		called.Store(true)
		return mcp.NewToolResultText("ok"), nil
	})
	// 등록되지 않은 새 도구도 기본 거부다(requiredScope 의 default deny).
	s.AddTool(mcp.NewTool("brand_new_tool"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		called.Store(true)
		return mcp.NewToolResultText("ok"), nil
	})

	call := func(ctx context.Context, tool string) *mcp.CallToolResult {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": tool, "arguments": map[string]any{}}})
		out := s.HandleMessage(ctx, raw)
		b, _ := json.Marshal(out)
		var env struct {
			Result mcp.CallToolResult `json:"result"`
		}
		if err := json.Unmarshal(b, &env); err != nil {
			t.Fatalf("decode: %v: %s", err, b)
		}
		return &env.Result
	}

	// 스코프가 빈 OAuth 토큰: search 는 insufficient_scope + 재인가 도전.
	noScope := mcpauth.WithPrincipal(context.Background(), mcpauth.Principal{Kind: mcpauth.PrincipalOAuth})
	res := call(noScope, "search")
	if !res.IsError || called.Load() {
		t.Fatalf("isError=%v handlerCalled=%v", res.IsError, called.Load())
	}
	if res.Meta == nil {
		t.Fatal("no _meta on insufficient_scope result")
	}
	chal, _ := res.Meta.AdditionalFields["mcp/www_authenticate"].([]any)
	if len(chal) != 1 {
		t.Fatalf("mcp/www_authenticate = %v", res.Meta.AdditionalFields)
	}
	want := `Bearer resource_metadata="https://brain-mcp.example.com/.well-known/oauth-protected-resource/mcp", error="insufficient_scope", error_description="The access token lacks the required scope", scope="brain:read"`
	if chal[0] != want {
		t.Fatalf("challenge = %q\nwant      %q", chal[0], want)
	}

	// 인증 없음(혼합 모드) 또는 주체가 ctx 에 없음(배선 실수): 거절 + 로그인 도전. fail closed.
	wantAnon := `Bearer resource_metadata="https://brain-mcp.example.com/.well-known/oauth-protected-resource/mcp", error="insufficient_scope", error_description="You need to login to continue", scope="brain:read"`
	for name, ctx := range map[string]context.Context{
		"no principal in ctx": context.Background(),
		"anonymous":           mcpauth.WithPrincipal(context.Background(), mcpauth.Principal{}),
	} {
		res := call(ctx, "search")
		if !res.IsError || called.Load() || res.Meta == nil {
			t.Fatalf("%s: tool ran or no challenge (isError=%v)", name, res.IsError)
		}
		if got, _ := res.Meta.AdditionalFields["mcp/www_authenticate"].([]any); len(got) != 1 || got[0] != wantAnon {
			t.Fatalf("%s: challenge = %v", name, got)
		}
	}
	// 읽기 스코프가 있으면 통과한다.
	withRead := mcpauth.WithPrincipal(context.Background(), mcpauth.Principal{Kind: mcpauth.PrincipalOAuth, Scopes: []string{mcpauth.ScopeRead}})
	if res := call(withRead, "search"); res.IsError || !called.Load() {
		t.Fatalf("read scope denied: %+v", res)
	}
	// 등록 안 된 도구는 읽기 스코프로도 거부(default deny).
	called.Store(false)
	if res := call(withRead, "brand_new_tool"); !res.IsError || called.Load() {
		t.Fatal("unregistered tool allowed for an OAuth token")
	}
	// 정적 키 주체는 전권.
	static := mcpauth.WithPrincipal(context.Background(), mcpauth.Principal{Kind: mcpauth.PrincipalStatic})
	if res := call(static, "brand_new_tool"); res.IsError || !called.Load() {
		t.Fatal("static key principal denied")
	}
}

func TestRequiredScope_TableCoversRegisteredTools(t *testing.T) {
	t.Parallel()
	s := mcpserver.NewMCPServer("t", "0")
	registerSearchTool(s, search.NewService(&fakeOccurredDocSearcher{}, disabledEmbeddingEngine{}), false, 0)
	registerGetDocumentTool(s, &fakeDocGetter{})
	registerStatsTool(s, &fakeStatsProvider{})
	registerAddNoteTool(s, &fakeMCPDocUpserter{}, &fakeMCPChunkWriter{}, &fakeMCPEmbedder{}, "")
	for name := range s.ListTools() {
		if _, ok := toolScopes[name]; !ok {
			t.Errorf("tool %q has no entry in toolScopes (it would be write-only)", name)
		}
	}
	if requiredScope("add_note") != mcpauth.ScopeWrite || requiredScope("search") != mcpauth.ScopeRead {
		t.Error("unexpected scopes")
	}
}

func TestAddNoteHandler_RejectsOAuthPrincipalDirectly(t *testing.T) {
	t.Parallel()
	up := &fakeMCPDocUpserter{}
	s := mcpserver.NewMCPServer("t", "0")
	registerAddNoteTool(s, up, &fakeMCPChunkWriter{}, &fakeMCPEmbedder{}, "")
	ctx := context.WithValue(context.Background(), mcpAuthKey{}, true)
	ctx = mcpauth.WithPrincipal(ctx, mcpauth.Principal{Kind: mcpauth.PrincipalOAuth, Scopes: []string{mcpauth.ScopeRead}})
	res := callTool(t, s, "add_note", ctx, map[string]any{"title": "t", "content": "c"})
	if !isErrorResult(res) || up.lastDoc != nil {
		t.Fatalf("handler wrote for an OAuth principal: %s", resultText(res))
	}
	// 대조군: 정적 키 주체는 쓴다.
	ctx = context.WithValue(context.Background(), mcpAuthKey{}, true)
	ctx = mcpauth.WithPrincipal(ctx, mcpauth.Principal{Kind: mcpauth.PrincipalStatic})
	res = callTool(t, s, "add_note", ctx, map[string]any{"title": "t", "content": "c"})
	if isErrorResult(res) || up.lastDoc == nil {
		t.Fatalf("static principal denied: %s", resultText(res))
	}
}

func TestOAuthPrereq(t *testing.T) {
	t.Parallel()
	cfg, _ := mcpauth.ConfigFromEnv(e2eConfigEnv("https://brain-mcp.example.com"))
	if oauthPrereqError(cfg, "") == nil {
		t.Error("OAuth without API_KEY must refuse to start")
	}
	if err := oauthPrereqError(cfg, "k"); err != nil {
		t.Errorf("unexpected: %v", err)
	}
	if err := oauthPrereqError(nil, ""); err != nil {
		t.Errorf("OAuth disabled must not require anything: %v", err)
	}
}

// ---------------------------------------------------------------------------
// OAuth 꺼짐: 기존 동작 그대로
// ---------------------------------------------------------------------------

// legacyHandler 는 OAuth 도입 전 run() 의 조립을 그대로 재현한다.
func legacyHandler(s *mcpserver.MCPServer, apiKey string) http.Handler {
	custom := &http.Server{}
	httpSrv := mcpserver.NewStreamableHTTPServer(s,
		mcpserver.WithHTTPContextFunc(mcpAuthContextFunc(apiKey)),
		mcpserver.WithStreamableHTTPServer(custom),
	)
	return newMCPHTTPHandler(httpSrv)
}

func TestOAuthDisabled_BehaviourUnchanged(t *testing.T) {
	t.Parallel()
	build := func() *mcpserver.MCPServer {
		s := mcpserver.NewMCPServer("second-brain", "1.0.0", mcpserver.WithToolCapabilities(false))
		registerSearchTool(s, search.NewService(&fakeOccurredDocSearcher{}, disabledEmbeddingEngine{}), false, 0)
		registerGetDocumentTool(s, &fakeDocGetter{})
		registerStatsTool(s, &fakeStatsProvider{})
		registerAddNoteTool(s, &fakeMCPDocUpserter{}, &fakeMCPChunkWriter{}, &fakeMCPEmbedder{}, e2eStaticKey)
		return s
	}
	_, newH := buildMCPHTTP(build(), e2eStaticKey, nil, &http.Server{})
	oldH := legacyHandler(build(), e2eStaticKey)

	type probe struct {
		name, method, path, auth, body string
	}
	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"x","version":"0"}}}`
	listBody := `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`
	callBody := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search","arguments":{"query":"q"}}}`
	probes := []probe{
		{"unauthenticated initialize", "POST", "/mcp", "", initBody},
		{"unauthenticated tools/list", "POST", "/mcp", "", listBody},
		{"unauthenticated search", "POST", "/mcp", "", callBody},
		{"static key search", "POST", "/mcp", "Bearer " + e2eStaticKey, callBody},
		{"wrong key search", "POST", "/mcp", "Bearer nope", callBody},
		{"oauth discovery path", "GET", "/.well-known/oauth-authorization-server", "", ""},
		{"prm path", "GET", "/.well-known/oauth-protected-resource", "", ""},
		{"authorize path", "GET", "/authorize?client_id=dots", "", ""},
		{"token path", "POST", "/token", "", "grant_type=refresh_token"},
	}
	for _, p := range probes {
		do := func(h http.Handler) (int, string, string) {
			req := httptest.NewRequest(p.method, p.path, strings.NewReader(p.body))
			req.Header.Set("Content-Type", "application/json")
			if p.method == "POST" {
				req.Header.Set("Accept", "application/json, text/event-stream")
			}
			// OAuth 도입 전 핸들러는 경로와 무관하게 GET 을 SSE 스트림으로 연다(끝나지
			// 않는다). 두 핸들러가 같은 모양으로 열리는지 비교하려고 짧은 시한을 건다.
			ctx, cancel := context.WithTimeout(req.Context(), 200*time.Millisecond)
			defer cancel()
			req = req.WithContext(ctx)
			if p.auth != "" {
				req.Header.Set("Authorization", p.auth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			return rec.Code, rec.Header().Get("Content-Type"), rec.Body.String()
		}
		oc, oct, ob := do(oldH)
		nc, nct, nb := do(newH)
		if p.method == "GET" {
			// GET 은 SSE 스트림이 열린 채 시한에 걸려 끊기므로 본문·헤더는 타이밍에 따라 달라질 수
			// 있다. 상태 코드가 같고 OAuth 문서(메타데이터)를 서빙하지 않는지만 본다.
			if oc != nc || strings.Contains(nb, "authorization_endpoint") || strings.Contains(nb, `"issuer"`) {
				t.Errorf("%s differs from the pre-OAuth behaviour: old %d new %d %q", p.name, oc, nc, nb)
			}
			continue
		}
		if oc != nc || oct != nct || ob != nb {
			t.Errorf("%s differs from the pre-OAuth behaviour:\n old %d %s %q\n new %d %s %q", p.name, oc, oct, ob, nc, nct, nb)
		}
	}

	// 이 시나리오들이 의미 있으려면 대표 동작 자체는 확인해 둔다.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(initBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	newH.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"serverInfo"`) {
		t.Fatalf("unauthenticated initialize no longer works with OAuth off: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("WWW-Authenticate") != "" {
		t.Error("OAuth off must not emit WWW-Authenticate")
	}
}
