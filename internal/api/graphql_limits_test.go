package api

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// #282 보안 리뷰 후속: GraphQL 요청 단위 상한(guardGraphQL).
//
// 리뷰에서 재현된 세 가지 — 별칭으로 한 요청에 검색 2000회, 쿼리스트링으로
// 본문 상한 우회(1만 회), 필드마다 새로 걸리는 타임아웃 — 이 수정 후에는
// 막히는지를 실제 Handler() 경로로 고정한다.
// ---------------------------------------------------------------------------

// gqlAliasDoc 은 search 필드를 n 개의 별칭으로 반복하는 문서를 만든다.
func gqlAliasDoc(n int) string {
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "a%d:search(query:\"x\"){count} ", i)
	}
	b.WriteString("}")
	return b.String()
}

// postGraphQL 은 JSON 본문으로 GraphQL 요청을 보낸다.
func postGraphQL(t *testing.T, srv *Server, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/graphql", bytes.NewReader(mustJSON(t, payload)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func TestGraphQLGuard_AliasAmplification(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		doc       string
		wantCode  int
		wantCalls int32
	}{
		{name: "at_limit", doc: gqlAliasDoc(graphqlMaxSearchFields), wantCode: http.StatusOK, wantCalls: graphqlMaxSearchFields},
		{name: "one_over_limit", doc: gqlAliasDoc(graphqlMaxSearchFields + 1), wantCode: http.StatusBadRequest},
		// 리뷰 재현값: 64KB 본문 안에 별칭 2000개.
		{name: "review_repro_2000", doc: gqlAliasDoc(2000), wantCode: http.StatusBadRequest},
		{
			name: "fragment_spread_expanded",
			doc: `query { ...F ...F }
			      fragment F on Query { a:search(query:"x"){count} b:search(query:"x"){count} c:search(query:"x"){count} }`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "inline_fragment_expanded",
			doc:      `{ ... on Query { ` + strings.Trim(gqlAliasDoc(graphqlMaxSearchFields+1), "{}") + ` } }`,
			wantCode: http.StatusBadRequest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			docs := &callCountingSearcher{}
			srv := newSearchInputTestServer(docs)

			w := postGraphQL(t, srv, map[string]any{"query": tc.doc})

			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body = %.300s", w.Code, tc.wantCode, w.Body.String())
			}
			if got := docs.calls.Load(); got != tc.wantCalls {
				t.Errorf("searcher called %d times, want %d", got, tc.wantCalls)
			}
		})
	}
}

func TestGraphQLGuard_CuratedLimit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		payload map[string]any
	}{
		{
			name:    "two_literal_curated",
			payload: map[string]any{"query": `{ a:search(query:"x", curated:true){count} b:search(query:"y", curated:true){count} }`},
		},
		{
			// 변수로 넘긴 curated 도 실제 값으로 센다.
			name: "curated_via_variables",
			payload: map[string]any{
				"query":     `query($c: Boolean) { a:search(query:"x", curated:$c){count} b:search(query:"y", curated:$c){count} }`,
				"variables": map[string]any{"c": true},
			},
		},
		{
			name:    "curated_via_variable_default",
			payload: map[string]any{"query": `query($c: Boolean = true) { a:search(query:"x", curated:$c){count} b:search(query:"y", curated:$c){count} }`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			docs := &callCountingSearcher{}
			srv := newSearchInputTestServer(docs)

			w := postGraphQL(t, srv, tc.payload)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
			}
			if got := docs.calls.Load(); got != 0 {
				t.Errorf("searcher called %d times, want 0", got)
			}
		})
	}

	// 대조군: curated 가 false 로 풀리면 두 검색 모두 허용된다.
	docs := &callCountingSearcher{}
	srv := newSearchInputTestServer(docs)
	w := postGraphQL(t, srv, map[string]any{
		"query":     `query($c: Boolean) { a:search(query:"x", curated:$c){count} b:search(query:"y", curated:false){count} }`,
		"variables": map[string]any{"c": false},
	})
	if w.Code != http.StatusOK || docs.calls.Load() != 2 {
		t.Errorf("control: status = %d, calls = %d; want 200 and 2", w.Code, docs.calls.Load())
	}
}

func TestGraphQLGuard_QueryStringCannotBypassLimits(t *testing.T) {
	t.Parallel()

	t.Run("oversized_query_string_rejected", func(t *testing.T) {
		t.Parallel()
		docs := &callCountingSearcher{}
		srv := newSearchInputTestServer(docs)

		// 리뷰 재현값: 쿼리스트링에 별칭 1만 개, 본문은 비어 있음.
		req := httptest.NewRequest(http.MethodPost, "/api/v1/graphql?query="+url.QueryEscape(gqlAliasDoc(10000)), strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusRequestURITooLong {
			t.Fatalf("status = %d, want 414", w.Code)
		}
		if got := docs.calls.Load(); got != 0 {
			t.Errorf("searcher called %d times, want 0", got)
		}
	})

	t.Run("small_query_string_still_counted", func(t *testing.T) {
		t.Parallel()
		docs := &callCountingSearcher{}
		srv := newSearchInputTestServer(docs)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/graphql?query="+url.QueryEscape(gqlAliasDoc(graphqlMaxSearchFields+1)), nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
		if got := docs.calls.Load(); got != 0 {
			t.Errorf("searcher called %d times, want 0", got)
		}
	})

	// 대조군: GET 쿼리스트링 GraphQL 은 여전히 동작한다(기존 사용 방식 유지).
	t.Run("get_query_string_works", func(t *testing.T) {
		t.Parallel()
		docs := &callCountingSearcher{}
		srv := newSearchInputTestServer(docs)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/graphql?query="+url.QueryEscape(gqlAliasDoc(1)), nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusOK || docs.calls.Load() != 1 {
			t.Errorf("status = %d, calls = %d; want 200 and 1", w.Code, docs.calls.Load())
		}
	})
}

// TestGraphQLGuard_RequestWideTimeout 은 요청 전체가 searchTimeout 하나로
// 묶이는지 본다. 필드별 타임아웃만 있으면 별칭 N 개는 벽시계 N×timeout 이
// 걸린다(리뷰 재현). 요청 ctx 에 타임아웃이 걸리면 첫 필드가 시간을 다 쓰고
// 나머지는 이미 끝난 ctx 로 즉시 돌아와 약 1×timeout 이 된다.
func TestGraphQLGuard_RequestWideTimeout(t *testing.T) {
	t.Parallel()

	const timeout = 200 * time.Millisecond
	srv := newSearchInputTestServer(slowSearcher{}).WithSearchTimeout(timeout)

	start := time.Now()
	w := postGraphQL(t, srv, map[string]any{"query": gqlAliasDoc(graphqlMaxSearchFields)})
	elapsed := time.Since(start)

	if elapsed < timeout {
		t.Errorf("wall = %v, shorter than the timeout; the slow searcher was not reached", elapsed)
	}
	// 필드별 타임아웃이었다면 5×200ms = 1s. 2.5×timeout 이면 둘을 가른다.
	if limit := timeout * 5 / 2; elapsed > limit {
		t.Errorf("wall = %v for %d aliases, want about 1×timeout (< %v)", elapsed, graphqlMaxSearchFields, limit)
	}
	if !isGraphQLTimeoutBody(w.Body.String()) {
		t.Errorf("body = %s, want a timeout error", w.Body.String())
	}
}

// isGraphQLTimeoutBody 는 GraphQL 응답이 타임아웃 오류인지 본다. 문구는
// 리졸버("search timed out")와 graphql-go 실행기("context deadline exceeded")
// 두 가지이며, 둘 다 요청 내용을 담지 않는다.
func isGraphQLTimeoutBody(body string) bool {
	return strings.Contains(body, "search timed out") || strings.Contains(body, "context deadline exceeded")
}

func TestGraphQLGuard_FragmentCycleRejected(t *testing.T) {
	t.Parallel()

	// 수정 전에는 graphql-go 검증기가 스택 오버플로(fatal error)로 테스트
	// 프로세스 전체를 죽였다. 필드 하위 선택 안에 숨은 순환도 잡아야 한다.
	for name, doc := range map[string]string{
		"root_level":      `query { ...A } fragment A on Query { ...B } fragment B on Query { ...A }`,
		"self":            `query { ...A } fragment A on Query { ...A }`,
		"nested_in_field": `query { search(query:"x") { ...R } } fragment R on SearchQueryResult { count ...R }`,
	} {
		docs := &callCountingSearcher{}
		srv := newSearchInputTestServer(docs)
		w := postGraphQL(t, srv, map[string]any{"query": doc})
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "fragment cycle") {
			t.Errorf("%s: status = %d body = %s; want 400 fragment cycle", name, w.Code, w.Body.String())
		}
		if got := docs.calls.Load(); got != 0 {
			t.Errorf("%s: searcher called %d times, want 0", name, got)
		}
	}
}
