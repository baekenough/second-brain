package mcpauth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const (
	cimdID       = "https://chatgpt.com/oauth/client.json"
	cimdCallback = "https://chatgpt.com/connector/oauth/abc123"
)

func cimdDoc(id string, mods ...func(map[string]any)) []byte {
	m := map[string]any{
		"client_id":                  id,
		"client_name":                "ChatGPT",
		"redirect_uris":              []string{cimdCallback},
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	}
	for _, f := range mods {
		f(m)
	}
	b, _ := json.Marshal(m)
	return b
}

type fakeCIMD struct {
	calls atomic.Int32
	docs  map[string][]byte
	err   error
}

func (f *fakeCIMD) fetch(_ context.Context, u string) ([]byte, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	if d, ok := f.docs[u]; ok {
		return d, nil
	}
	return nil, errors.New("not found")
}

func newCIMDFixture(t *testing.T, f *fakeCIMD) *fixture {
	t.Helper()
	return newFixtureWith(t, []Option{WithCIMDFetcher(f.fetch)})
}

func cimdQuery(id string, mods ...func(url.Values)) url.Values {
	base := []func(url.Values){set("client_id", id), set("redirect_uri", cimdCallback)}
	return authzQuery(append(base, mods...)...)
}

func TestCIMD_ClientIDPatternMatching(t *testing.T) {
	t.Parallel()
	r, err := newCIMDRegistry(defaultCIMDPatterns, []string{"chatgpt.com"}, nil, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	ok := []string{
		"https://chatgpt.com/oauth/client.json",
		"https://chatgpt.com/oauth/abc123/client.json",
		"https://ChatGPT.com/oauth/abc-1_2.3~x/client.json",
	}
	bad := []string{
		"",
		"http://chatgpt.com/oauth/client.json",
		"https://chatgpt.com/oauth/client.json?x=1",
		"https://chatgpt.com/oauth/client.json#f",
		"https://chatgpt.com/oauth/client.json/",
		"https://chatgpt.com//oauth/client.json",
		"https://chatgpt.com/oauth/../oauth/client.json",
		"https://chatgpt.com/oauth/./client.json",
		"https://chatgpt.com/oauth/%2e%2e/client.json",
		"https://chatgpt.com/oauth/a%2Fb/client.json",
		"https://chatgpt.com/oauth/a/b/client.json",
		"https://chatgpt.com/oauth/../client.json",
		"https://chatgpt.com/oauth/other.json",
		"https://chatgpt.com:8443/oauth/client.json",
		"https://evil.example.com/oauth/client.json",
		"https://chatgpt.com.evil.example.com/oauth/client.json",
		"https://user@chatgpt.com/oauth/client.json",
		"https://evil.example.com@chatgpt.com/oauth/client.json",
		"https://chatgpt.com\\@evil.example.com/oauth/client.json",
		"https://127.0.0.1/oauth/client.json",
		"https://chatgpt.com/oauth/" + strings.Repeat("a", 129) + "/client.json",
		"https://chatgpt.com/oauth/a b/client.json",
		"dots",
	}
	for _, id := range ok {
		if !r.matches(id) {
			t.Errorf("%q should match", id)
		}
	}
	for _, id := range bad {
		if r.matches(id) {
			t.Errorf("%q must not match", id)
		}
	}
}

func TestCIMD_DocumentValidation(t *testing.T) {
	t.Parallel()
	hosts := map[string]bool{"chatgpt.com": true}
	good := cimdDoc(cimdID)
	c, err := parseCIMDDocument(cimdID, good, hosts)
	if err != nil || c.ID != cimdID || c.Name != "ChatGPT" || len(c.RedirectURIs) != 1 || c.RedirectURIs[0] != cimdCallback {
		t.Fatalf("control: %+v %v", c, err)
	}
	// token_endpoint_auth_method 가 없어도 된다(공개 클라이언트).
	if _, err := parseCIMDDocument(cimdID, cimdDoc(cimdID, func(m map[string]any) { delete(m, "token_endpoint_auth_method") }), hosts); err != nil {
		t.Errorf("absent auth method rejected: %v", err)
	}
	cases := map[string]func(map[string]any){
		"client_id mismatch":    func(m map[string]any) { m["client_id"] = "https://chatgpt.com/oauth/other/client.json" },
		"client_id missing":     func(m map[string]any) { delete(m, "client_id") },
		"private_key_jwt":       func(m map[string]any) { m["token_endpoint_auth_method"] = "private_key_jwt" },
		"client_secret_basic":   func(m map[string]any) { m["token_endpoint_auth_method"] = "client_secret_basic" },
		"client_secret present": func(m map[string]any) { m["client_secret"] = "s3cret" },
		"no redirect_uris":      func(m map[string]any) { m["redirect_uris"] = []string{} },
		"too many redirect_uris": func(m map[string]any) {
			m["redirect_uris"] = strings.Split(strings.Repeat("https://chatgpt.com/a,", 11), ",")
		},
		"foreign redirect host":   func(m map[string]any) { m["redirect_uris"] = []string{"https://evil.example.com/cb"} },
		"one foreign of many":     func(m map[string]any) { m["redirect_uris"] = []string{cimdCallback, "https://evil.example.com/cb"} },
		"lookalike redirect host": func(m map[string]any) { m["redirect_uris"] = []string{"https://chatgpt.com.evil.example.com/cb"} },
		"userinfo redirect":       func(m map[string]any) { m["redirect_uris"] = []string{"https://evil.example.com@chatgpt.com/cb"} },
		"http redirect":           func(m map[string]any) { m["redirect_uris"] = []string{"http://chatgpt.com/cb"} },
		"custom scheme redirect":  func(m map[string]any) { m["redirect_uris"] = []string{"app://chatgpt.com/cb"} },
		"redirect with fragment":  func(m map[string]any) { m["redirect_uris"] = []string{cimdCallback + "#x"} },
		"redirect with port":      func(m map[string]any) { m["redirect_uris"] = []string{"https://chatgpt.com:8443/cb"} },
		"redirect wildcard":       func(m map[string]any) { m["redirect_uris"] = []string{"https://chatgpt.com/*"} },
		"bad grant type":          func(m map[string]any) { m["grant_types"] = []string{"implicit"} },
		"bad response type":       func(m map[string]any) { m["response_types"] = []string{"token"} },
	}
	for name, mod := range cases {
		if _, err := parseCIMDDocument(cimdID, cimdDoc(cimdID, mod), hosts); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := parseCIMDDocument(cimdID, []byte("not json"), hosts); err == nil {
		t.Error("garbage accepted")
	}
}

func TestCIMD_Registry_FetchOnlyAllowlistedAndCaches(t *testing.T) {
	t.Parallel()
	ff := &fakeCIMD{docs: map[string][]byte{cimdID: cimdDoc(cimdID)}}
	f := newCIMDFixture(t, ff)

	// 허용 패턴 밖의 URL 은 가져오지도 않는다(SSRF 표면 최소화).
	for _, id := range []string{"https://evil.example.com/oauth/client.json", "http://chatgpt.com/oauth/client.json", "https://169.254.169.254/latest/meta-data/", "https://chatgpt.com/oauth/a/b/client.json"} {
		rec := f.get(PathAuthorize + "?" + cimdQuery(id).Encode())
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want error page", id, rec.Code)
		}
	}
	if ff.calls.Load() != 0 {
		t.Fatalf("fetched %d non-allowlisted URLs", ff.calls.Load())
	}

	// 허용 URL: 첫 요청에서 가져오고 이후에는 캐시를 쓴다.
	for i := 0; i < 3; i++ {
		if rec := f.get(PathAuthorize + "?" + cimdQuery(cimdID).Encode()); rec.Code != http.StatusOK {
			t.Fatalf("allowlisted CIMD client: %d %s", rec.Code, rec.Body.String())
		}
	}
	if ff.calls.Load() != 1 {
		t.Fatalf("fetch count = %d, want 1 (cached)", ff.calls.Load())
	}
	// 캐시 만료 후 다시 가져온다.
	f.clock.Add(cimdCacheTTL + time.Second)
	f.get(PathAuthorize + "?" + cimdQuery(cimdID).Encode())
	if ff.calls.Load() != 2 {
		t.Fatalf("fetch count after TTL = %d, want 2", ff.calls.Load())
	}
}

func TestCIMD_FullFlow(t *testing.T) {
	t.Parallel()
	ff := &fakeCIMD{docs: map[string][]byte{cimdID: cimdDoc(cimdID)}}
	f := newCIMDFixture(t, ff)

	// 동의 화면에 클라이언트 이름이 보인다.
	rec := f.get(PathAuthorize + "?" + cimdQuery(cimdID).Encode())
	if !strings.Contains(rec.Body.String(), "ChatGPT") || !strings.Contains(rec.Body.String(), cimdID) {
		t.Fatalf("consent page does not identify the CIMD client: %s", rec.Body.String())
	}

	form := f.openConsent(cimdQuery(cimdID))
	form.Set("owner_key", testOwnerKey)
	prec := f.postForm(PathAuthorize, form, "")
	if prec.Code != http.StatusSeeOther {
		t.Fatalf("consent submit: %d %s", prec.Code, prec.Body.String())
	}
	u := location(t, prec)
	if got := u.Scheme + "://" + u.Host + u.Path; got != cimdCallback {
		t.Fatalf("redirected to %q", got)
	}
	code := u.Query().Get("code")

	tf := tokenForm(code, set("client_id", cimdID), set("redirect_uri", cimdCallback))
	status, tok, _ := f.token(tf)
	if status != http.StatusOK || tok.RefreshToken == "" {
		t.Fatalf("token: %d %+v", status, tok)
	}
	claims, err := f.srv.signer.verify(tok.AccessToken, f.clock.Now())
	if err != nil || claims.ClientID != cimdID {
		t.Fatalf("claims = %+v err=%v", claims, err)
	}
	// 리프레시도 CIMD client_id 로.
	status, tok2, _ := f.token(refreshForm(tok.RefreshToken, set("client_id", cimdID)))
	if status != http.StatusOK || tok2.RefreshToken == tok.RefreshToken {
		t.Fatalf("refresh: %d %+v", status, tok2)
	}
	// 다른 client_id 로는 안 된다.
	if status, _, _ := f.token(refreshForm(tok2.RefreshToken, set("client_id", testClientID))); status == http.StatusOK {
		t.Fatal("refresh token usable by a different client")
	}
}

func TestCIMD_RedirectURIMustBeInDocument(t *testing.T) {
	t.Parallel()
	ff := &fakeCIMD{docs: map[string][]byte{cimdID: cimdDoc(cimdID)}}
	f := newCIMDFixture(t, ff)
	// 문서에 없는 redirect_uri: 정적 클라이언트의 허용 콜백이어도 CIMD 클라이언트에는 쓸 수 없다.
	for _, uri := range []string{testRedirect, "https://chatgpt.com/connector/oauth/other", "https://evil.example.com/cb"} {
		rec := f.get(PathAuthorize + "?" + cimdQuery(cimdID, set("redirect_uri", uri)).Encode())
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
			t.Errorf("redirect_uri %q: status %d location %q", uri, rec.Code, rec.Header().Get("Location"))
		}
	}
	// 반대로 정적 클라이언트에 CIMD 문서의 콜백을 쓸 수도 없다.
	rec := f.get(PathAuthorize + "?" + authzQuery(set("redirect_uri", cimdCallback)).Encode())
	if rec.Code != http.StatusBadRequest {
		t.Errorf("static client accepted a CIMD-only callback: %d", rec.Code)
	}
}

func TestCIMD_BadDocumentsAreRefused(t *testing.T) {
	t.Parallel()
	docs := map[string]func(map[string]any){
		"private_key_jwt": func(m map[string]any) { m["token_endpoint_auth_method"] = "private_key_jwt" },
		"id mismatch":     func(m map[string]any) { m["client_id"] = "https://chatgpt.com/oauth/x/client.json" },
		"foreign redir":   func(m map[string]any) { m["redirect_uris"] = []string{"https://evil.example.com/cb"} },
	}
	for name, mod := range docs {
		ff := &fakeCIMD{docs: map[string][]byte{cimdID: cimdDoc(cimdID, mod)}}
		f := newCIMDFixture(t, ff)
		rec := f.get(PathAuthorize + "?" + cimdQuery(cimdID).Encode())
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
	}
}

func TestCIMD_FailureCachingAndStaleIfError(t *testing.T) {
	t.Parallel()
	ff := &fakeCIMD{docs: map[string][]byte{cimdID: cimdDoc(cimdID)}}
	f := newCIMDFixture(t, ff)
	if rec := f.get(PathAuthorize + "?" + cimdQuery(cimdID).Encode()); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d", rec.Code)
	}
	// 캐시가 만료된 뒤 문서 서버가 죽어도 이전 성공값으로 (24시간 안에는) 계속 동작한다.
	ff.err = errors.New("down")
	f.clock.Add(cimdCacheTTL + time.Minute)
	if rec := f.get(PathAuthorize + "?" + cimdQuery(cimdID).Encode()); rec.Code != http.StatusOK {
		t.Fatalf("stale-if-error: %d", rec.Code)
	}
	// 24시간이 지나면 더는 쓰지 않는다.
	f.clock.Add(cimdStaleTTL + cimdCacheTTL)
	if rec := f.get(PathAuthorize + "?" + cimdQuery(cimdID).Encode()); rec.Code != http.StatusBadRequest {
		t.Fatalf("too stale: %d", rec.Code)
	}
	// 처음부터 실패하는 클라이언트는 실패도 캐시되어 반복 호출이 외부로 새지 않는다.
	ff2 := &fakeCIMD{err: errors.New("down")}
	f2 := newCIMDFixture(t, ff2)
	for i := 0; i < 5; i++ {
		f2.get(PathAuthorize + "?" + cimdQuery("https://chatgpt.com/oauth/zzz/client.json").Encode())
	}
	if ff2.calls.Load() != 1 {
		t.Fatalf("failing client fetched %d times, want 1 (negative cache)", ff2.calls.Load())
	}
}

func TestCIMD_DisabledByConfig(t *testing.T) {
	t.Parallel()
	ff := &fakeCIMD{docs: map[string][]byte{cimdID: cimdDoc(cimdID)}}
	f := newFixtureWith(t, []Option{WithCIMDFetcher(ff.fetch)}, func(c *Config) { c.CIMDClientIDPatterns = nil })
	if rec := f.get(PathAuthorize + "?" + cimdQuery(cimdID).Encode()); rec.Code != http.StatusBadRequest {
		t.Fatalf("CIMD client accepted while disabled: %d", rec.Code)
	}
	if ff.calls.Load() != 0 {
		t.Fatal("fetched while disabled")
	}
	m := getJSON(t, f, "/.well-known/oauth-authorization-server")
	if _, present := m["client_id_metadata_document_supported"]; present {
		t.Error("advertises CIMD while disabled")
	}
}

// ---------------------------------------------------------------------------
// SSRF 방어
// ---------------------------------------------------------------------------

func TestIsPublicIP(t *testing.T) {
	t.Parallel()
	public := []string{"8.8.8.8", "1.1.1.1", "104.18.0.1", "2606:4700:4700::1111", "::ffff:8.8.8.8"}
	private := []string{
		"127.0.0.1", "127.1.2.3", "::1", "0.0.0.0", "::", "10.0.0.1", "172.16.0.1", "172.31.255.255", "192.168.1.1",
		"169.254.169.254", "169.254.0.1", "fe80::1", "fc00::1", "fd00::1", "100.64.0.1", "100.127.255.255",
		"192.0.0.1", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "224.0.0.1", "239.255.255.250",
		"255.255.255.255", "240.0.0.1", "ff02::1", "2001:db8::1", "64:ff9b::7f00:1", "2002:7f00:1::1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254",
	}
	for _, s := range public {
		if !isPublicIP(netip.MustParseAddr(s)) {
			t.Errorf("%s should be public", s)
		}
	}
	for _, s := range private {
		if isPublicIP(netip.MustParseAddr(s)) {
			t.Errorf("%s must not be public", s)
		}
	}
	if isPublicIP(netip.Addr{}) {
		t.Error("zero address must not be public")
	}
}

func TestGuardDial(t *testing.T) {
	t.Parallel()
	var rc syscall.RawConn
	if err := guardDial("tcp4", "8.8.8.8:443", rc); err != nil {
		t.Errorf("public dial rejected: %v", err)
	}
	for _, addr := range []string{"127.0.0.1:443", "10.0.0.5:443", "169.254.169.254:80", "[::1]:443", "[fe80::1]:443"} {
		if err := guardDial("tcp4", addr, rc); err == nil {
			t.Errorf("%s allowed", addr)
		}
	}
	if err := guardDial("unix", "/var/run/docker.sock", rc); err == nil {
		t.Error("unix socket allowed")
	}
	if err := guardDial("tcp", "not-an-addr", rc); err == nil {
		t.Error("garbage allowed")
	}
}

// 기본 가져오기(운영 구성)가 루프백 서버에 연결 자체를 하지 않는다.
func TestSecureFetcher_DefaultConfigRefusesLoopback(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()
	// 서버 인증서를 신뢰시켜도(= TLS 오류가 아니라 주소 검사가 막는지 보려고) 막혀야 한다.
	tlsCfg := &tls.Config{RootCAs: ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
	fetch := newSecureFetcher(tlsCfg, nil)
	for _, target := range []string{ts.URL, strings.Replace(ts.URL, "127.0.0.1", "localhost", 1)} {
		if _, err := fetch(context.Background(), target+"/oauth/client.json"); err == nil {
			t.Errorf("%s: fetched a loopback server", target)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("server received %d requests; the dial guard must stop the connection", hits.Load())
	}
}

// 주소 검사를 풀어 둔 상태에서 응답 처리 규칙(리다이렉트·크기·콘텐츠 타입·시간)을 본다.
func TestSecureFetcher_ResponseRules(t *testing.T) {
	t.Parallel()
	var redirectTargetHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" {
			http.Error(w, "accept", http.StatusNotAcceptable)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"client_id":"x"}`))
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/target", http.StatusFound)
	})
	mux.HandleFunc("/target", func(w http.ResponseWriter, _ *http.Request) {
		redirectTargetHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/html", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html></html>`))
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"x":"` + strings.Repeat("a", cimdMaxDocBytes) + `"}`))
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("/404", http.NotFound)
	ts := httptest.NewTLSServer(mux)
	defer ts.Close()

	allow := func(string, string, syscall.RawConn) error { return nil }
	fetch := newSecureFetcher(&tls.Config{RootCAs: ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}, allow)
	ctx := context.Background()

	body, err := fetch(ctx, ts.URL+"/ok")
	if err != nil || string(body) != `{"client_id":"x"}` {
		t.Fatalf("control fetch: %q %v", body, err)
	}
	for _, p := range []string{"/redirect", "/html", "/big", "/404"} {
		if _, err := fetch(ctx, ts.URL+p); err == nil {
			t.Errorf("%s: accepted", p)
		}
	}
	if redirectTargetHits.Load() != 0 {
		t.Error("redirect was followed")
	}
	slowCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := fetch(slowCtx, ts.URL+"/slow"); err == nil || time.Since(start) > 2*time.Second {
		t.Errorf("slow server not cut off quickly (err=%v after %v)", err, time.Since(start))
	}
	// 인증서를 신뢰하지 않으면 실패한다(TLS 검증을 끄지 않았다).
	insecure := newSecureFetcher(nil, allow)
	if _, err := insecure(ctx, ts.URL+"/ok"); err == nil {
		t.Error("untrusted certificate accepted")
	}
}

// ---------------------------------------------------------------------------
// RFC 9207: 모든 인가 응답의 iss 는 메타데이터 issuer 와 글자까지 같다
// ---------------------------------------------------------------------------

func TestIssuerIdentification_ExactOnEveryAuthorizationResponse(t *testing.T) {
	t.Parallel()
	// 대문자 호스트·포트·끝 슬래시가 섞인 입력도 정규화되어야 한다.
	env := validEnv()
	env[EnvPublicBaseURL] = "https://Brain-MCP.Example.com:8443/"
	cfg2, err := ConfigFromEnv(envOf(env))
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, func(c *Config) { *c = *cfg2 })

	meta := getJSON(t, f, "/.well-known/oauth-authorization-server")
	prm := getJSON(t, f, "/.well-known/oauth-protected-resource")
	issuer := meta["issuer"].(string)
	if issuer != "https://brain-mcp.example.com:8443" || strings.HasSuffix(issuer, "/") {
		t.Fatalf("issuer = %q", issuer)
	}
	if prm["authorization_servers"].([]any)[0] != issuer {
		t.Fatalf("PRM authorization_servers %v != issuer", prm["authorization_servers"])
	}
	res := cfg2.Resource
	withRes := func(mods ...func(url.Values)) url.Values {
		return authzQuery(append([]func(url.Values){set("resource", res)}, mods...)...)
	}

	var responses []*httptest.ResponseRecorder
	// 성공
	form := f.openConsent(withRes())
	form.Set("owner_key", testOwnerKey)
	responses = append(responses, f.postForm(PathAuthorize, form, ""))
	// 거부
	form = f.openConsent(withRes())
	form.Set("action", "deny")
	responses = append(responses, f.postForm(PathAuthorize, form, ""))
	// 각종 오류 리다이렉트
	for _, q := range []url.Values{
		withRes(set("code_challenge_method", "plain")),
		withRes(del("code_challenge")),
		withRes(set("response_type", "token")),
		withRes(set("resource", "https://evil.example.com/mcp")),
	} {
		responses = append(responses, f.get(PathAuthorize+"?"+q.Encode()))
	}
	for i, rec := range responses {
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("response %d: status %d", i, rec.Code)
		}
		if iss := location(t, rec).Query().Get("iss"); iss != issuer {
			t.Errorf("response %d: iss = %q, want exactly %q", i, iss, issuer)
		}
	}
	// 대조군: 코드/오류가 실제로 들어 있는 응답들이다.
	if location(t, responses[0]).Query().Get("code") == "" || location(t, responses[1]).Query().Get("error") != "access_denied" {
		t.Fatal("control: unexpected response contents")
	}
}
