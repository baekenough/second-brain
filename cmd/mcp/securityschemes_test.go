package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/baekenough/second-brain/internal/mcpauth"
)

const toolsListRequest = `{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{}}`

// 읽기 도구(_meta.securitySchemes 있음)와 쓰기 도구(없음)를 담은 tools/list 결과.
const toolsListResultJSON = `{"jsonrpc":"2.0","id":7,"result":{"tools":[` +
	`{"name":"search","description":"a <b> & c","inputSchema":{"type":"object"},"_meta":{"securitySchemes":[{"type":"oauth2","scopes":["brain:read"]}],"other":1}},` +
	`{"name":"add_note","inputSchema":{"type":"object"}}]}}`

func decodeTools(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var msg struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("not JSON: %v: %s", err, raw)
	}
	return msg.Result.Tools
}

func assertRewritten(t *testing.T, tools []map[string]any) {
	t.Helper()
	if len(tools) != 2 {
		t.Fatalf("tools = %v", tools)
	}
	search := tools[0]
	top, _ := search["securitySchemes"].([]any)
	meta, _ := search["_meta"].(map[string]any)
	if len(top) != 1 || meta == nil || meta["securitySchemes"] == nil || meta["other"] != float64(1) {
		t.Fatalf("search tool = %v", search)
	}
	if a, b := top[0].(map[string]any), meta["securitySchemes"].([]any)[0].(map[string]any); a["type"] != b["type"] || a["type"] != "oauth2" {
		t.Fatalf("top-level %v != _meta %v", a, b)
	}
	if search["description"] != "a <b> & c" {
		t.Errorf("description changed: %q", search["description"])
	}
	if _, ok := tools[1]["securitySchemes"]; ok {
		t.Errorf("tool without _meta.securitySchemes got one: %v", tools[1])
	}
	if _, ok := search["inputSchema"]; !ok {
		t.Error("inputSchema lost")
	}
}

func TestAddTopLevelSecuritySchemes_JSON(t *testing.T) {
	t.Parallel()
	out := addTopLevelSecuritySchemes([]byte(toolsListResultJSON), "application/json; charset=utf-8")
	assertRewritten(t, decodeTools(t, out))
	// HTML 문자를 이스케이프하지 않는다.
	if !bytes.Contains(out, []byte("a <b> & c")) {
		t.Errorf("html characters were escaped: %s", out)
	}
}

func TestAddTopLevelSecuritySchemes_Batch(t *testing.T) {
	t.Parallel()
	batch := `[` + toolsListResultJSON + `,{"jsonrpc":"2.0","id":8,"result":{}}]`
	out := addTopLevelSecuritySchemes([]byte(batch), "application/json")
	var msgs []json.RawMessage
	if err := json.Unmarshal(out, &msgs); err != nil || len(msgs) != 2 {
		t.Fatalf("batch broken: %v %s", err, out)
	}
	assertRewritten(t, decodeTools(t, msgs[0]))
	if string(msgs[1]) != `{"jsonrpc":"2.0","id":8,"result":{}}` {
		t.Errorf("other batch member changed: %s", msgs[1])
	}
}

func TestAddTopLevelSecuritySchemes_SSE(t *testing.T) {
	t.Parallel()
	sse := "event: message\ndata: " + toolsListResultJSON + "\n\n"
	out := addTopLevelSecuritySchemes([]byte(sse), "text/event-stream")
	lines := strings.Split(string(out), "\n")
	if lines[0] != "event: message" || !strings.HasPrefix(lines[1], "data: ") || lines[2] != "" || lines[3] != "" {
		t.Fatalf("SSE framing changed: %q", out)
	}
	assertRewritten(t, decodeTools(t, []byte(strings.TrimPrefix(lines[1], "data: "))))

	// CRLF, `data:`(공백 없음), 다른 이벤트 섞임, 다중 data 이벤트
	crlf := "id: 1\r\ndata:" + toolsListResultJSON + "\r\n\r\n" + `data: {"jsonrpc":"2.0","method":"notifications/x"}` + "\r\n\r\n"
	out = addTopLevelSecuritySchemes([]byte(crlf), "text/event-stream")
	parts := strings.Split(string(out), "\r\n")
	if parts[0] != "id: 1" || !strings.HasPrefix(parts[1], "data:") || parts[2] != "" || parts[4] != "" {
		t.Fatalf("CRLF framing changed: %q", out)
	}
	assertRewritten(t, decodeTools(t, []byte(strings.TrimPrefix(parts[1], "data:"))))
	if parts[3] != `data: {"jsonrpc":"2.0","method":"notifications/x"}` {
		t.Errorf("notification line changed: %q", parts[3])
	}
}

func TestAddTopLevelSecuritySchemes_UntouchedWhenNothingToDo(t *testing.T) {
	t.Parallel()
	cases := map[string][2]string{
		"no meta":           {`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"x","inputSchema":{}}]}}`, "application/json"},
		"already top-level": {`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"x","securitySchemes":[],"_meta":{"securitySchemes":[1]}}]}}`, "application/json"},
		"tool call result":  {`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"hi"}],"_meta":{"securitySchemes":[1]}}}`, "application/json"},
		"error response":    {`{"jsonrpc":"2.0","id":1,"error":{"code":-1,"message":"x"}}`, "application/json"},
		"not json":          {`hello`, "application/json"},
		"other type":        {toolsListResultJSON, "text/plain"},
		"empty":             {``, "application/json"},
		"sse ping":          {": ping\n\n", "text/event-stream"},
	}
	for name, c := range cases {
		if out := addTopLevelSecuritySchemes([]byte(c[0]), c[1]); string(out) != c[0] {
			t.Errorf("%s: changed\n in  %q\n out %q", name, c[0], out)
		}
	}
}

func TestRequestHasMethod(t *testing.T) {
	t.Parallel()
	yes := []string{toolsListRequest, ` ` + toolsListRequest + "\n", `[` + toolsListRequest + `,{"method":"x"}]`}
	no := []string{``, `x`, `{"method":"tools/call"}`, `[{"method":"tools/call"}]`, `{"result":{"method":"tools/list"}}`, `[`}
	for _, b := range yes {
		if !requestHasMethod([]byte(b), "tools/list") {
			t.Errorf("should detect: %q", b)
		}
	}
	for _, b := range no {
		if requestHasMethod([]byte(b), "tools/list") {
			t.Errorf("should not detect: %q", b)
		}
	}
}

// downstream 은 요청 본문에 맞춰 고정 응답을 내는 가짜 MCP 핸들러다.
func downstream(contentType, listBody string, status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Mcp-Session-Id", "sess-1")
		w.Header().Set("Content-Type", contentType)
		body := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}],"_meta":{"securitySchemes":[{"type":"oauth2"}]}}}`
		if requestHasMethod(b, "tools/list") {
			body = listBody
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = io.WriteString(w, body)
	})
}

func serve(h http.Handler, method, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/mcp", strings.NewReader(body))
	h.ServeHTTP(rec, req)
	return rec
}

func TestTopLevelSecuritySchemes_Handler_JSON(t *testing.T) {
	t.Parallel()
	h := topLevelSecuritySchemes(downstream("application/json", toolsListResultJSON, 0))
	rec := serve(h, http.MethodPost, toolsListRequest)
	assertRewritten(t, decodeTools(t, rec.Body.Bytes()))
	if rec.Code != 200 || rec.Header().Get("Mcp-Session-Id") != "sess-1" ||
		rec.Header().Get("Content-Length") != strconv.Itoa(rec.Body.Len()) {
		t.Fatalf("status/headers: %d %v (body %d bytes)", rec.Code, rec.Header(), rec.Body.Len())
	}
}

func TestTopLevelSecuritySchemes_Handler_SSE(t *testing.T) {
	t.Parallel()
	sse := "event: message\ndata: " + toolsListResultJSON + "\n\n"
	h := topLevelSecuritySchemes(downstream("text/event-stream", sse, 0))
	rec := serve(h, http.MethodPost, toolsListRequest)
	data := strings.TrimPrefix(strings.Split(rec.Body.String(), "\n")[1], "data: ")
	assertRewritten(t, decodeTools(t, []byte(data)))
	if rec.Header().Get("Content-Length") != strconv.Itoa(rec.Body.Len()) {
		t.Fatalf("content-length %q for %d bytes", rec.Header().Get("Content-Length"), rec.Body.Len())
	}
}

func TestTopLevelSecuritySchemes_Handler_OtherResponsesByteIdentical(t *testing.T) {
	t.Parallel()
	list := downstream("application/json", toolsListResultJSON, 0)
	h := topLevelSecuritySchemes(list)
	other := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search"}}`
	want := httptest.NewRecorder()
	list.ServeHTTP(want, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(other)))
	got := serve(h, http.MethodPost, other)
	if got.Body.String() != want.Body.String() || got.Code != want.Code || got.Header().Get("Content-Length") != want.Header().Get("Content-Length") {
		t.Fatalf("tools/call response changed:\n got  %q\n want %q", got.Body.String(), want.Body.String())
	}
	// GET(SSE 스트림 열기)은 본문을 읽지도, 가로채지도 않는다.
	var called atomic.Bool
	g := topLevelSecuritySchemes(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		_, _ = io.WriteString(w, toolsListResultJSON)
	}))
	rec := serve(g, http.MethodGet, "")
	if !called.Load() || rec.Body.String() != toolsListResultJSON {
		t.Fatalf("GET response changed: %q", rec.Body.String())
	}
	// 변환 대상이 없는 tools/list 응답도 바이트 그대로.
	plain := `{"jsonrpc":"2.0","id":7,"result":{"tools":[{"name":"x","inputSchema":{}}]}}`
	rec = serve(topLevelSecuritySchemes(downstream("application/json", plain, 0)), http.MethodPost, toolsListRequest)
	if rec.Body.String() != plain {
		t.Fatalf("untouched tools/list changed: %q", rec.Body.String())
	}
	// 오류 상태는 변환하지 않고 상태 코드를 유지한다.
	rec = serve(topLevelSecuritySchemes(downstream("application/json", toolsListResultJSON, http.StatusBadRequest)), http.MethodPost, toolsListRequest)
	if rec.Code != http.StatusBadRequest || rec.Body.String() != toolsListResultJSON {
		t.Fatalf("error status response changed: %d %q", rec.Code, rec.Body.String())
	}
}

func TestTopLevelSecuritySchemes_BodyReadErrorReachesHandler(t *testing.T) {
	t.Parallel()
	var gotErr error
	h := topLevelSecuritySchemes(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, gotErr = io.ReadAll(r.Body)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", io.MultiReader(strings.NewReader("{"), errReader{errors.New("boom")}))
	h.ServeHTTP(rec, req)
	if gotErr == nil || gotErr.Error() != "boom" {
		t.Fatalf("read error swallowed: %v", gotErr)
	}
}

// ---------------------------------------------------------------------------
// C2: 세션 유휴 정리
// ---------------------------------------------------------------------------

func TestOAuthSessions_AreSweptWhenIdle(t *testing.T) {
	cfg, _ := mcpauth.ConfigFromEnv(e2eConfigEnv("http://127.0.0.1:1"))
	as, err := mcpauth.NewServer(*cfg, mcpauth.NewMemoryRefreshStore())
	if err != nil {
		t.Fatal(err)
	}
	var unregistered atomic.Int32
	hooks := &mcpserver.Hooks{}
	hooks.AddOnUnregisterSession(func(context.Context, mcpserver.ClientSession) { unregistered.Add(1) })
	s := mcpserver.NewMCPServer("t", "0", mcpserver.WithToolCapabilities(false), mcpserver.WithHooks(hooks))
	_, h := buildMCPHTTPWithTTL(s, e2eStaticKey, as, &http.Server{}, 1100*time.Millisecond)
	ts := httptest.NewServer(h)
	defer ts.Close()

	// 인증 없는 initialize 로 세션을 만든다(혼합 모드에서 허용).
	c := &rpc{t: t, url: ts.URL + "/mcp"}
	if resp, _ := c.initialize(); resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize: %d", resp.StatusCode)
	}
	deadline := time.Now().Add(8 * time.Second)
	for unregistered.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if unregistered.Load() == 0 {
		t.Fatal("idle session was never swept (WithSessionIdleTTL not applied?)")
	}
}

// ---------------------------------------------------------------------------
// C3: 본문 상한 — 정적 키가 아니면 1MB
// ---------------------------------------------------------------------------

func TestOAuthBodyCap_NonStaticPrincipalsLimitedStaticKeepsLargeCap(t *testing.T) {
	e := newMCPForTest(t, true)
	pad := strings.Repeat("a", 2<<20) // 2MB: 1MB 상한은 넘고 mcpRequestMaxBytes 보다는 작다
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"` + pad + `","version":"0"}}}`
	post := func(bearer string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, e.base+"/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if _, out := post(""); !strings.Contains(out, `"error"`) || strings.Contains(out, `"serverInfo"`) {
		t.Errorf("anonymous 2MB body was processed: %.200s", out)
	}
	if st, out := post(e2eStaticKey); st != http.StatusOK || !strings.Contains(out, `"serverInfo"`) {
		t.Errorf("static key 2MB body rejected: %d %.200s", st, out)
	}
	// OAuth 토큰도 1MB 상한.
	f := e.discover()
	code := e.authorize(f, "brain:read")
	status, tk := e.token(f, urlValues(f, code))
	if status != http.StatusOK {
		t.Fatalf("token: %d", status)
	}
	if _, out := post(tk.AccessToken); !strings.Contains(out, `"error"`) || strings.Contains(out, `"serverInfo"`) {
		t.Errorf("oauth 2MB body was processed: %.200s", out)
	}
}

func urlValues(f authFlow, code string) url.Values {
	return url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {f.redirect},
		"client_id": {f.clientID}, "code_verifier": {e2eVerifier},
	}
}
