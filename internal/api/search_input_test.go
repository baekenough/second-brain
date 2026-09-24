package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
)

// ---------------------------------------------------------------------------
// #282: 검색 진입점의 입력 검증·본문 상한·요청 타임아웃.
//
// 모든 검사는 실제 핸들러 경로로 한다. 검증에 걸린 요청은 가짜 검색기까지
// 내려가지 않아야 하고(= DB 에 가기 전 거부), 응답 본문에는 질의 원문이
// 섞이지 않아야 한다. 질의에는 leak 표식을 섞어 두고 응답에서 찾는다.
// ---------------------------------------------------------------------------

// searchLeakMarker 는 응답·로그에 질의 원문이 새는지 확인하는 표식이다.
const searchLeakMarker = "zq-leak-marker-282"

// callCountingSearcher 는 호출 횟수만 세는 search.DocumentSearcher 가짜다.
type callCountingSearcher struct{ calls atomic.Int32 }

func (c *callCountingSearcher) Search(context.Context, model.SearchQuery) ([]*model.SearchResult, error) {
	c.calls.Add(1)
	return nil, nil
}

// slowSearcher 는 ctx 가 끝날 때까지 막혀 있는 느린 검색기다. err 가 nil 이면
// ctx.Err() 를, 아니면 err 를 돌려준다. 후자는 context 오류를 감싸지 않은
// 채로 올라오는 레인 오류(예: SQLSTATE 57014 PgError)를 흉내 낸다.
type slowSearcher struct{ err error }

func (s slowSearcher) Search(ctx context.Context, _ model.SearchQuery) ([]*model.SearchResult, error) {
	<-ctx.Done()
	if s.err != nil {
		return nil, s.err
	}
	return nil, ctx.Err()
}

func newSearchInputTestServer(docs search.DocumentSearcher) *Server {
	svc := search.NewService(docs, askDisabledEmbedder{})
	return NewServer(nil, svc, nil, nil, nil, "", "")
}

func assertNoLeak(t *testing.T, where, body string) {
	t.Helper()
	if strings.Contains(body, searchLeakMarker) {
		t.Errorf("%s leaks the query text: %s", where, body)
	}
}

func TestSearchHandler_POST_RejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tooLong := searchLeakMarker + strings.Repeat("가", search.MaxQueryBytes/3)
	cases := []struct {
		name     string
		body     []byte
		wantCode int
	}{
		// JSON 이스케이프 \u0000 은 디코딩 뒤 NUL 바이트가 된다 — 22021 의 실제 경로.
		{name: "nul_escape", body: []byte(`{"query":"` + searchLeakMarker + `\u0000x"}`), wantCode: http.StatusBadRequest},
		// 이스케이프가 아닌 날 바이트. encoding/json 은 이걸 U+FFFD 로 바꿔 버리므로
		// 본문 단계에서 거부해야 한다.
		{name: "raw_invalid_utf8", body: []byte("{\"query\":\"" + searchLeakMarker + "\xff\xfe\"}"), wantCode: http.StatusBadRequest},
		{name: "too_long", body: mustJSON(t, map[string]any{"query": tooLong}), wantCode: http.StatusBadRequest},
		{name: "nul_in_exclude_source_types", body: []byte(`{"query":"ok","exclude_source_types":["sms\u0000"]}`), wantCode: http.StatusBadRequest},
		{name: "nul_in_source_type", body: []byte(`{"query":"ok","source_type":"sms\u0000"}`), wantCode: http.StatusBadRequest},
		{name: "body_over_cap", body: mustJSON(t, map[string]any{"query": "ok", "pad": strings.Repeat("a", searchRequestMaxBytes)}), wantCode: http.StatusRequestEntityTooLarge},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			docs := &callCountingSearcher{}
			srv := newSearchInputTestServer(docs)

			req := httptest.NewRequest(http.MethodPost, "/api/v1/search", bytes.NewReader(tc.body))
			w := httptest.NewRecorder()
			srv.searchHandler(w, req)

			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body = %s", w.Code, tc.wantCode, w.Body.String())
			}
			if got := docs.calls.Load(); got != 0 {
				t.Errorf("searcher called %d times; input must be rejected before the store", got)
			}
			assertNoLeak(t, "response body", w.Body.String())
		})
	}
}

func TestSearchHandler_POST_ValidQuery_OK(t *testing.T) {
	t.Parallel()

	docs := &callCountingSearcher{}
	srv := newSearchInputTestServer(docs)

	// 상한과 정확히 같은 길이는 통과해야 한다(경계값).
	body := mustJSON(t, map[string]any{"query": strings.Repeat("a", search.MaxQueryBytes)})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/search", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.searchHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if got := docs.calls.Load(); got != 1 {
		t.Errorf("searcher called %d times, want 1", got)
	}
}

func TestSearchGetHandler_RejectsInvalidInput(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		// 퍼센트 디코딩 결과가 그대로 문자열이 되므로 GET 은 날 바이트가 실제로
		// 들어오는 경로다.
		"nul":              "q=" + searchLeakMarker + "%00x",
		"invalid_utf8":     "q=" + searchLeakMarker + "%FF%FE",
		"too_long":         "q=" + url.QueryEscape(searchLeakMarker+strings.Repeat("a", search.MaxQueryBytes)),
		"nul_source_type":  "q=ok&source_type=sms%00",
		"utf8_source_type": "q=ok&source_type=%C3",
	}
	for name, rawQuery := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			docs := &callCountingSearcher{}
			srv := newSearchInputTestServer(docs)

			req := httptest.NewRequest(http.MethodGet, "/api/v1/search?"+rawQuery, nil)
			w := httptest.NewRecorder()
			srv.searchGetHandler(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
			}
			if got := docs.calls.Load(); got != 0 {
				t.Errorf("searcher called %d times; input must be rejected before the store", got)
			}
			assertNoLeak(t, "response body", w.Body.String())
		})
	}
}

func TestSearchGetHandler_ValidQuery_OK(t *testing.T) {
	t.Parallel()

	docs := &callCountingSearcher{}
	srv := newSearchInputTestServer(docs)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q="+url.QueryEscape("지난주 통화"), nil)
	w := httptest.NewRecorder()
	srv.searchGetHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if got := docs.calls.Load(); got != 1 {
		t.Errorf("searcher called %d times, want 1", got)
	}
}

// syncBuffer 는 여러 고루틴이 동시에 쓰는 slog 출력을 받기 위한 버퍼다.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestSearchHandlers_Timeout 은 느린 검색기로 요청 타임아웃을 확인한다.
//
// t.Parallel 을 쓰지 않는다: slog 기본 로거를 바꿔 로그에 질의 원문이 남지
// 않는지까지 확인하기 때문이다(병렬 테스트는 이 테스트가 끝난 뒤 시작된다).
func TestSearchHandlers_Timeout(t *testing.T) {
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	searchErrs := map[string]error{
		"context_error": nil,
		// context 오류를 감싸지 않은 오류. 오류 체인만 보고 판정하면 500 으로
		// 오분류된다 — ctx 상태로 판정하는지를 고정한다.
		"unwrapped_57014": errors.New("ERROR: canceling statement due to user request (SQLSTATE 57014)"),
	}
	for errName, searchErr := range searchErrs {
		srv := newSearchInputTestServer(slowSearcher{err: searchErr}).WithSearchTimeout(50 * time.Millisecond)

		requests := map[string]func() (*httptest.ResponseRecorder, time.Duration){
			"POST": func() (*httptest.ResponseRecorder, time.Duration) {
				body := mustJSON(t, map[string]any{"query": searchLeakMarker})
				req := httptest.NewRequest(http.MethodPost, "/api/v1/search", bytes.NewReader(body))
				w := httptest.NewRecorder()
				start := time.Now()
				srv.searchHandler(w, req)
				return w, time.Since(start)
			},
			"GET": func() (*httptest.ResponseRecorder, time.Duration) {
				req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q="+searchLeakMarker, nil)
				w := httptest.NewRecorder()
				start := time.Now()
				srv.searchGetHandler(w, req)
				return w, time.Since(start)
			},
		}
		for method, do := range requests {
			t.Run(errName+"/"+method, func(t *testing.T) {
				w, elapsed := do()
				if w.Code != http.StatusGatewayTimeout {
					t.Fatalf("status = %d, want 504; body = %s", w.Code, w.Body.String())
				}
				var resp map[string]string
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				if resp["error"] != "search timed out" {
					t.Errorf("error = %q, want %q", resp["error"], "search timed out")
				}
				assertNoLeak(t, "response body", w.Body.String())
				if elapsed > 5*time.Second {
					t.Errorf("handler took %v; the timeout did not bound it", elapsed)
				}
			})
		}
	}

	if !strings.Contains(logs.String(), "search: timed out") {
		t.Errorf("expected a timeout log line; got:\n%s", logs.String())
	}
	assertNoLeak(t, "log output", logs.String())
}

// TestSearchHandler_ClientCancelIsNotTimeout 은 클라이언트가 먼저 끊은 경우
// (부모 ctx 취소)를 504 로 잘못 분류하지 않는지 확인한다.
func TestSearchHandler_ClientCancelIsNotTimeout(t *testing.T) {
	t.Parallel()

	srv := newSearchInputTestServer(slowSearcher{}).WithSearchTimeout(time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=ok", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.searchGetHandler(w, req)

	if w.Code == http.StatusGatewayTimeout {
		t.Fatalf("status = 504 for a client cancel; want a non-timeout status")
	}
}

// graphqlSearch 는 변수로 질의를 넘기는 GraphQL search 요청을 보낸다.
func graphqlSearch(t *testing.T, srv *Server, query string) (int, string) {
	t.Helper()
	body := mustJSON(t, map[string]any{
		"query":     `query($q: String!) { search(query: $q) { count } }`,
		"variables": map[string]any{"q": query},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/graphql", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func TestGraphQLSearch_RejectsInvalidInput(t *testing.T) {
	t.Parallel()

	docs := &callCountingSearcher{}
	srv := newSearchInputTestServer(docs)

	for name, q := range map[string]string{
		"nul":      searchLeakMarker + "\x00",
		"too_long": searchLeakMarker + strings.Repeat("a", search.MaxQueryBytes),
	} {
		_, body := graphqlSearch(t, srv, q)
		if !strings.Contains(body, `"errors"`) {
			t.Errorf("%s: expected a GraphQL error, got %s", name, body)
		}
		assertNoLeak(t, name+" response body", body)
	}
	if got := docs.calls.Load(); got != 0 {
		t.Errorf("searcher called %d times; input must be rejected before the store", got)
	}
}

func TestGraphQLSearch_Timeout(t *testing.T) {
	t.Parallel()

	srv := newSearchInputTestServer(slowSearcher{}).WithSearchTimeout(50 * time.Millisecond)
	_, body := graphqlSearch(t, srv, searchLeakMarker)

	// 요청 전체 타임아웃(guardGraphQL)과 필드 타임아웃이 같은 값이라 어느 쪽이
	// 먼저 끝나느냐에 따라 문구가 다르다: 리졸버가 먼저면 "search timed out",
	// graphql-go 실행기가 먼저 ctx 종료를 보면 "context deadline exceeded".
	if !isGraphQLTimeoutBody(body) {
		t.Errorf("expected a timeout error, got %s", body)
	}
	assertNoLeak(t, "response body", body)
}

func TestGraphQL_BodyOverCapRejected(t *testing.T) {
	t.Parallel()

	// 질의 자체는 정상이고 쓰이지 않는 변수로만 본문을 키운다. 그래야 질의
	// 길이 검증이 아니라 본문 상한이 거부했다는 것을 구분할 수 있다.
	post := func(pad int) int32 {
		docs := &callCountingSearcher{}
		srv := newSearchInputTestServer(docs)
		body := mustJSON(t, map[string]any{
			"query":     `query($q: String!) { search(query: $q) { count } }`,
			"variables": map[string]any{"q": "ok", "pad": strings.Repeat("a", pad)},
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/graphql", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
		return docs.calls.Load()
	}

	if got := post(16); got != 1 {
		t.Fatalf("control: small body reached the searcher %d times, want 1", got)
	}
	if got := post(graphqlRequestMaxBytes); got != 0 {
		t.Errorf("searcher called %d times for a body over the cap", got)
	}
}

func TestAskHandler_RejectsInvalidQuestion(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		body     []byte
		wantCode int
	}{
		{name: "nul_escape", body: []byte(`{"question":"` + searchLeakMarker + `\u0000"}`), wantCode: http.StatusBadRequest},
		{name: "raw_invalid_utf8", body: []byte("{\"question\":\"" + searchLeakMarker + "\xff\"}"), wantCode: http.StatusBadRequest},
		{name: "over_question_budget", body: mustJSON(t, map[string]any{"question": strings.Repeat("a", askQuestionBytes+1)}), wantCode: http.StatusBadRequest},
		{name: "body_over_cap", body: mustJSON(t, map[string]any{"question": "ok", "pad": strings.Repeat("a", askRequestMaxBytes)}), wantCode: http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			docs := &callCountingSearcher{}
			srv := newSearchInputTestServer(docs)

			req := httptest.NewRequest(http.MethodPost, "/api/v1/ask", bytes.NewReader(tc.body))
			w := httptest.NewRecorder()
			srv.askHandler(w, req)

			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body = %s", w.Code, tc.wantCode, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
				t.Errorf("SSE stream opened for rejected input (Content-Type %q)", ct)
			}
			if got := docs.calls.Load(); got != 0 {
				t.Errorf("searcher called %d times; input must be rejected before retrieval", got)
			}
			assertNoLeak(t, "response body", w.Body.String())
		})
	}
}

func TestGraphEntitiesHandler_RejectsInvalidInput(t *testing.T) {
	t.Parallel()

	// s.graph 가 nil 이므로 검증을 통과해 그래프 조회까지 가면 패닉이 난다 —
	// 400 이 나오면 조회 전에 거부됐다는 뜻이다.
	srv := newSearchInputTestServer(&callCountingSearcher{})
	for name, rawQuery := range map[string]string{
		"nul":          "q=" + searchLeakMarker + "%00",
		"invalid_utf8": "q=" + searchLeakMarker + "%FF",
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/graph/entities?"+rawQuery, nil)
		w := httptest.NewRecorder()
		srv.graphEntitiesHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body = %s", name, w.Code, w.Body.String())
		}
		assertNoLeak(t, name+" response body", w.Body.String())
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
