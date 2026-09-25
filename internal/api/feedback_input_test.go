package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// #286 항목 1: feedback 류 진입점의 입력 상한·NUL·UTF-8 검증.
//
// 모든 검사는 실제 핸들러 경로(Handler())로 한다. 거부된 요청은 저장소
// 가짜까지 내려가지 않아야 하고(호출 수 0 = DB 에 가기 전 거부), 응답
// 본문에는 입력 원문이 섞이지 않아야 한다. 거부 대상 필드에는 누출 표식을
// 섞어 두고 응답에서 찾는다.

// feedbackLeakMarker 는 응답·로그에 입력 원문이 새는지 확인하는 표식이다.
const feedbackLeakMarker = "SENTINEL-피드백-7f3a"

// countingFeedbackRecorder 는 호출 수와 저장 요청 크기를 세는 FeedbackRecorder 가짜다.
type countingFeedbackRecorder struct {
	mu         sync.Mutex
	calls      int
	savedBytes int
	last       store.Feedback
	err        error
}

func (c *countingFeedbackRecorder) Record(_ context.Context, f store.Feedback) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.last = f
	if f.Comment != nil {
		c.savedBytes += len(*f.Comment)
	}
	if c.err != nil {
		return 0, c.err
	}
	return int64(c.calls), nil
}

func (c *countingFeedbackRecorder) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// countingEvidenceVoter 는 호출 수와 마지막 document_id 를 기록하는 EvidenceVoter 가짜다.
type countingEvidenceVoter struct {
	calls     atomic.Int32
	err       error
	mu        sync.Mutex
	lastDocID string
}

func (c *countingEvidenceVoter) UpsertEvidence(_ context.Context, v store.EvidenceVote) (int64, int16, error) {
	c.calls.Add(1)
	c.mu.Lock()
	c.lastDocID = v.DocumentID
	c.mu.Unlock()
	if c.err != nil {
		return 0, 0, c.err
	}
	return 1, v.Thumbs, nil
}

// newFeedbackInputServer 는 feedback·evidence·GraphQL 경로를 모두 가진 서버다.
func newFeedbackInputServer(fb FeedbackRecorder, voter EvidenceVoter) *Server {
	svc := search.NewService(&callCountingSearcher{}, askDisabledEmbedder{})
	srv := NewServer(nil, svc, fb, nil, nil, "", "")
	if voter != nil {
		srv = srv.WithEvidenceFeedback(voter)
	}
	return srv
}

func doRawRequest(srv *Server, method, path string, body []byte) *httptest.ResponseRecorder {
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// koreanOfBytes 는 정확히 n 바이트가 되는 한글 문자열을 만든다. 한글 한 자가
// 3바이트라 나머지는 ASCII 로 채운다 — 경계가 다중 바이트 문자 안에 걸리는
// 경우까지 바이트 기준으로 세는지 확인하려는 것이다.
func koreanOfBytes(n int) string {
	s := strings.Repeat("가", n/3) + strings.Repeat("a", n%3)
	if len(s) != n {
		panic(fmt.Sprintf("koreanOfBytes(%d) = %d bytes", n, len(s)))
	}
	return s
}

// nestedMetadata 는 깊이 depth 의 중첩 객체를 만든다. metadata 객체 자신이
// 깊이 1 이고, 가장 안쪽에는 문자열 leaf 가 있다.
func nestedMetadata(depth int, leaf string) map[string]any {
	m := map[string]any{"v": leaf}
	for i := 1; i < depth; i++ {
		m = map[string]any{"n": m}
	}
	return m
}

// metadataOfSerializedBytes 는 json.Marshal 결과가 정확히 n 바이트인 metadata 다.
func metadataOfSerializedBytes(t *testing.T, n int) map[string]any {
	t.Helper()
	// {"k":"<값>"} 의 틀이 8바이트다.
	m := map[string]any{"k": strings.Repeat("x", n-8)}
	if b := mustJSON(t, m); len(b) != n {
		t.Fatalf("metadata serialized to %d bytes, want %d", len(b), n)
	}
	return m
}

func TestFeedbackInput_REST(t *testing.T) {
	t.Parallel()

	base := func() map[string]any { return map[string]any{"source": "api", "thumbs": 1} }
	with := func(kv ...any) []byte {
		m := base()
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		b, _ := json.Marshal(m)
		return b
	}
	marker := feedbackLeakMarker

	cases := []struct {
		name     string
		body     []byte
		wantCode int
	}{
		{"valid_minimal", with(), http.StatusCreated},
		{"query_at_limit_korean", with("query", koreanOfBytes(feedbackQueryMaxBytes)), http.StatusCreated},
		{"query_one_over", with("query", koreanOfBytes(feedbackQueryMaxBytes+1)), http.StatusBadRequest},
		{"comment_at_limit", with("comment", koreanOfBytes(feedbackCommentMaxBytes)), http.StatusCreated},
		{"comment_one_over", with("comment", marker+koreanOfBytes(feedbackCommentMaxBytes)), http.StatusBadRequest},
		{"source_at_limit", with("source", strings.Repeat("s", feedbackSourceMaxBytes)), http.StatusCreated},
		{"source_one_over", with("source", strings.Repeat("s", feedbackSourceMaxBytes+1)), http.StatusBadRequest},
		{"session_id_one_over", with("session_id", strings.Repeat("s", feedbackIDMaxBytes+1)), http.StatusBadRequest},
		{"user_id_one_over", with("user_id", strings.Repeat("u", feedbackIDMaxBytes+1)), http.StatusBadRequest},
		{"query_nul", []byte(`{"source":"api","thumbs":1,"query":"` + marker + `\u0000"}`), http.StatusBadRequest},
		{"comment_nul", []byte(`{"source":"api","thumbs":1,"comment":"` + marker + `\u0000"}`), http.StatusBadRequest},
		{"source_nul", []byte(`{"source":"a\u0000","thumbs":1}`), http.StatusBadRequest},
		{"raw_invalid_utf8", []byte("{\"source\":\"api\",\"thumbs\":1,\"query\":\"" + marker + "\xff\"}"), http.StatusBadRequest},
		{"metadata_key_nul", []byte(`{"source":"api","thumbs":1,"metadata":{"` + marker + `\u0000":1}}`), http.StatusBadRequest},
		{"metadata_value_nul", []byte(`{"source":"api","thumbs":1,"metadata":{"k":"` + marker + `\u0000"}}`), http.StatusBadRequest},
		{"metadata_deep_array_nul", []byte(`{"source":"api","thumbs":1,"metadata":{"a":[[{"b":["` + marker + `\u0000"]}]]}}`), http.StatusBadRequest},
		{"metadata_at_size_limit", with("metadata", metadataOfSerializedBytes(t, feedbackMetadataMaxBytes)), http.StatusCreated},
		{"metadata_one_over_size", with("metadata", metadataOfSerializedBytes(t, feedbackMetadataMaxBytes+1)), http.StatusBadRequest},
		{"metadata_at_depth_limit", with("metadata", nestedMetadata(feedbackMetadataMaxDepth, "ok")), http.StatusCreated},
		{"metadata_one_over_depth", with("metadata", nestedMetadata(feedbackMetadataMaxDepth+1, marker)), http.StatusBadRequest},
		{"document_id_not_uuid", with("document_id", marker), http.StatusBadRequest},
		{"document_id_empty", with("document_id", ""), http.StatusBadRequest},
		{"document_id_uuid", with("document_id", uuid.NewString()), http.StatusCreated},
		{"body_one_over", with("comment", strings.Repeat("x", feedbackRequestMaxBytes)), http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &countingFeedbackRecorder{}
			srv := newFeedbackInputServer(rec, nil)

			w := doRawRequest(srv, http.MethodPost, "/api/v1/feedback", tc.body)

			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body = %.300s", w.Code, tc.wantCode, w.Body.String())
			}
			wantCalls := 0
			if tc.wantCode == http.StatusCreated {
				wantCalls = 1
			}
			if got := rec.count(); got != wantCalls {
				t.Errorf("recorder called %d times, want %d", got, wantCalls)
			}
			if strings.Contains(w.Body.String(), feedbackLeakMarker) {
				t.Errorf("response leaks the input: %.300s", w.Body.String())
			}
		})
	}
}

// TestFeedbackInput_ForeignKeyViolationIs400 은 존재하지 않는 document_id·
// chunk_id 가 FK 위반(23503)으로 500 이 되지 않고 400 이 되는지 본다(§2.2-6).
// 실제 PostgreSQL 이 돌려주는 모양은 feedback_input_db_test.go 가 확인한다.
func TestFeedbackInput_ForeignKeyViolationIs400(t *testing.T) {
	fkErr := fmt.Errorf("feedback record: %w", &pgconn.PgError{Code: "23503", ConstraintName: "feedback_document_id_fkey"})

	t.Run("rest", func(t *testing.T) {
		t.Parallel()
		srv := newFeedbackInputServer(&countingFeedbackRecorder{err: fkErr}, nil)
		w := doRawRequest(srv, http.MethodPost, "/api/v1/feedback",
			[]byte(`{"source":"api","thumbs":1,"document_id":"`+uuid.NewString()+`"}`))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "not found") {
			t.Errorf("status = %d body = %s; want 400 not found", w.Code, w.Body.String())
		}
	})

	t.Run("rest_other_error_stays_500", func(t *testing.T) {
		t.Parallel()
		srv := newFeedbackInputServer(&countingFeedbackRecorder{err: &pgconn.PgError{Code: "23505"}}, nil)
		w := doRawRequest(srv, http.MethodPost, "/api/v1/feedback", []byte(`{"source":"api","thumbs":1}`))
		if w.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", w.Code)
		}
	})

	t.Run("graphql", func(t *testing.T) {
		t.Parallel()
		srv := newFeedbackInputServer(&countingFeedbackRecorder{err: fkErr}, nil)
		w := postGraphQL(t, srv, map[string]any{
			"query":     `mutation($i: FeedbackInput!) { createFeedback(input:$i){id} }`,
			"variables": map[string]any{"i": map[string]any{"source": "api", "thumbs": 1, "documentId": uuid.NewString()}},
		})
		if !strings.Contains(w.Body.String(), "not found") || strings.Contains(w.Body.String(), "internal server error") {
			t.Errorf("body = %s; want a not-found error, not internal server error", w.Body.String())
		}
	})
}

func TestFeedbackInput_Evidence(t *testing.T) {
	t.Setenv("FEEDBACK_EVIDENCE_ENABLED", "1")

	valid := func() map[string]any {
		return map[string]any{
			"conversation_id": "conv-1",
			"query":           "질문",
			"document_id":     uuid.NewString(),
			"thumbs":          1,
			"rank":            0,
			"layer":           "observed",
		}
	}
	with := func(kv ...any) []byte {
		m := valid()
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		b, _ := json.Marshal(m)
		return b
	}
	docID := uuid.NewString()
	marker := feedbackLeakMarker

	cases := []struct {
		name     string
		body     []byte
		wantCode int
	}{
		{"valid", with(), http.StatusOK},
		{"layer_empty_allowed", with("layer", ""), http.StatusOK},
		{"layer_note", with("layer", "note"), http.StatusOK},
		{"layer_insight", with("layer", "insight"), http.StatusOK},
		{"layer_unknown", with("layer", marker), http.StatusBadRequest},
		// /ask 질문 상한 길이의 질문에도 투표할 수 있어야 한다(두 상한이 같은 상수).
		{"query_at_ask_limit", with("query", koreanOfBytes(askQuestionBytes)), http.StatusOK},
		{"query_one_over", with("query", marker+koreanOfBytes(askQuestionBytes)), http.StatusBadRequest},
		{"query_nul", []byte(`{"conversation_id":"c","query":"` + marker + `\u0000","document_id":"` + docID + `","thumbs":1}`), http.StatusBadRequest},
		{"conversation_id_nul", []byte(`{"conversation_id":"c\u0000","query":"q","document_id":"` + docID + `","thumbs":1}`), http.StatusBadRequest},
		{"conversation_id_one_over", with("conversation_id", strings.Repeat("c", feedbackIDMaxBytes+1)), http.StatusBadRequest},
		{"raw_invalid_utf8", []byte("{\"conversation_id\":\"c\",\"query\":\"" + marker + "\xff\",\"document_id\":\"" + docID + "\",\"thumbs\":1}"), http.StatusBadRequest},
		{"body_one_over", with("query", strings.Repeat("x", feedbackRequestMaxBytes)), http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			voter := &countingEvidenceVoter{}
			srv := newFeedbackInputServer(&countingFeedbackRecorder{}, voter)

			w := doRawRequest(srv, http.MethodPost, "/api/v1/feedback/evidence", tc.body)

			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body = %.300s", w.Code, tc.wantCode, w.Body.String())
			}
			wantCalls := int32(0)
			if tc.wantCode == http.StatusOK {
				wantCalls = 1
			}
			if got := voter.calls.Load(); got != wantCalls {
				t.Errorf("voter called %d times, want %d", got, wantCalls)
			}
			if strings.Contains(w.Body.String(), feedbackLeakMarker) {
				t.Errorf("response leaks the input: %.300s", w.Body.String())
			}
		})
	}

	t.Run("foreign_key_violation_is_400", func(t *testing.T) {
		voter := &countingEvidenceVoter{err: fmt.Errorf("wrap: %w", &pgconn.PgError{Code: "23503"})}
		srv := newFeedbackInputServer(&countingFeedbackRecorder{}, voter)
		w := doRawRequest(srv, http.MethodPost, "/api/v1/feedback/evidence", with())
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "not found") {
			t.Errorf("status = %d body = %s; want 400 not found", w.Code, w.Body.String())
		}
	})
}

// goldenJudgmentItems 는 서로 다른 문서 n 개에 대한 판정 목록이다.
func goldenJudgmentItems(n int) []map[string]any {
	out := make([]map[string]any, n)
	for i := range out {
		out[i] = map[string]any{"document_id": uuid.NewString(), "judgment": "relevant", "rank": i + 1}
	}
	return out
}

func TestFeedbackInput_Golden(t *testing.T) {
	t.Parallel()

	marker := feedbackLeakMarker
	judgments := func(n int) []byte {
		return mustJSON(t, map[string]any{"query_id": uuid.NewString(), "judgments": goldenJudgmentItems(n)})
	}
	feedback := func(kv ...any) []byte {
		m := map[string]any{"query_text": "질문", "source": "hermes", "judge": "llm", "judgments": goldenJudgmentItems(2)}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return mustJSON(t, m)
	}

	cases := []struct {
		name     string
		path     string
		body     []byte
		wantCode int
	}{
		{"judgments_at_limit", "/api/v1/golden/judgments", judgments(goldenJudgmentsMax), http.StatusOK},
		{"judgments_one_over", "/api/v1/golden/judgments", judgments(goldenJudgmentsMax + 1), http.StatusBadRequest},
		{"judgments_body_one_over", "/api/v1/golden/judgments",
			mustJSON(t, map[string]any{"query_id": uuid.NewString(), "pad": strings.Repeat("x", goldenRequestMaxBytes)}), http.StatusRequestEntityTooLarge},
		{"feedback_valid", "/api/v1/golden/feedback", feedback(), http.StatusOK},
		{"feedback_judgments_at_limit", "/api/v1/golden/feedback", feedback("judgments", goldenJudgmentItems(goldenJudgmentsMax)), http.StatusOK},
		{"feedback_judgments_one_over", "/api/v1/golden/feedback", feedback("judgments", goldenJudgmentItems(goldenJudgmentsMax+1)), http.StatusBadRequest},
		{"feedback_query_text_at_limit", "/api/v1/golden/feedback", feedback("query_text", koreanOfBytes(goldenQueryTextMaxBytes)), http.StatusOK},
		{"feedback_query_text_one_over", "/api/v1/golden/feedback", feedback("query_text", marker+koreanOfBytes(goldenQueryTextMaxBytes)), http.StatusBadRequest},
		{"feedback_query_text_nul", "/api/v1/golden/feedback",
			[]byte(`{"query_text":"` + marker + `\u0000","source":"hermes","judge":"llm","judgments":[]}`), http.StatusBadRequest},
		{"feedback_asked_at_one_over", "/api/v1/golden/feedback", feedback("asked_at", strings.Repeat("1", goldenAskedAtMaxBytes+1)), http.StatusBadRequest},
		{"feedback_raw_invalid_utf8", "/api/v1/golden/feedback",
			[]byte("{\"query_text\":\"" + marker + "\xff\",\"source\":\"hermes\",\"judge\":\"llm\",\"judgments\":[]}"), http.StatusBadRequest},
		{"feedback_body_one_over", "/api/v1/golden/feedback", feedback("note", strings.Repeat("x", goldenRequestMaxBytes)), http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := &stubGoldenSet{byTextID: uuid.New(), byTextFound: true, upsertSaved: 1}
			srv := newGoldenTestServer(stub, nil)

			w := doRawRequest(srv, http.MethodPost, tc.path, tc.body)

			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body = %.300s", w.Code, tc.wantCode, w.Body.String())
			}
			reachedStore := stub.upsertJudgments != nil || stub.byTextText != ""
			if wantReach := tc.wantCode == http.StatusOK; reachedStore != wantReach {
				t.Errorf("store reached = %v, want %v", reachedStore, wantReach)
			}
			if strings.Contains(w.Body.String(), feedbackLeakMarker) {
				t.Errorf("response leaks the input: %.300s", w.Body.String())
			}
		})
	}
}

// gqlFeedbackAliasDoc 은 createFeedback 을 n 개의 별칭으로 반복하는 mutation 이다.
// 모든 별칭이 변수 $i 하나를 재사용하므로 본문 상한이 저장량을 묶지 못한다.
func gqlFeedbackAliasDoc(n int) string {
	var b strings.Builder
	b.WriteString("mutation($i: FeedbackInput!) {")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "a%d:createFeedback(input:$i){id} ", i)
	}
	b.WriteString("}")
	return b.String()
}

func TestGraphQLGuard_CreateFeedbackLimit(t *testing.T) {
	t.Parallel()

	input := map[string]any{"source": "api", "thumbs": 1}

	cases := []struct {
		name      string
		payload   map[string]any
		wantCode  int
		wantCalls int
	}{
		{
			name:      "single_allowed",
			payload:   map[string]any{"query": gqlFeedbackAliasDoc(1), "variables": map[string]any{"i": input}},
			wantCode:  http.StatusOK,
			wantCalls: 1,
		},
		{
			name:     "two_aliases",
			payload:  map[string]any{"query": gqlFeedbackAliasDoc(2), "variables": map[string]any{"i": input}},
			wantCode: http.StatusBadRequest,
		},
		{
			name: "fragment_spread_hides_two",
			payload: map[string]any{
				"query": `mutation($i: FeedbackInput!) { ...F }
				          fragment F on Mutation { a:createFeedback(input:$i){id} b:createFeedback(input:$i){id} }`,
				"variables": map[string]any{"i": input},
			},
			wantCode: http.StatusBadRequest,
		},
		{
			name: "fragment_spread_twice",
			payload: map[string]any{
				"query": `mutation($i: FeedbackInput!) { ...F ...F }
				          fragment F on Mutation { a:createFeedback(input:$i){id} }`,
				"variables": map[string]any{"i": input},
			},
			wantCode: http.StatusBadRequest,
		},
		{
			name: "inline_fragment_hides_two",
			payload: map[string]any{
				"query":     `mutation($i: FeedbackInput!) { ... on Mutation { a:createFeedback(input:$i){id} b:createFeedback(input:$i){id} } }`,
				"variables": map[string]any{"i": input},
			},
			wantCode: http.StatusBadRequest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &countingFeedbackRecorder{}
			srv := newFeedbackInputServer(rec, nil)

			w := postGraphQL(t, srv, tc.payload)

			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body = %.300s", w.Code, tc.wantCode, w.Body.String())
			}
			if got := rec.count(); got != tc.wantCalls {
				t.Errorf("recorder called %d times, want %d", got, tc.wantCalls)
			}
		})
	}
}

// TestGraphQLGuard_CreateFeedbackAmplificationRepro 는 계획 §1.2 의 증폭
// 페이로드(64KB 본문, 별칭 1,000개, 20KB comment 변수 재사용)를 그대로 보낸다.
// 수정 전 코드는 이 요청 하나로 Recorder 를 1,000회 불러 comment 만 약 20MB 를
// 저장하려 했다. 수정 후에는 실행 전에 거부되어 0회여야 한다.
func TestGraphQLGuard_CreateFeedbackAmplificationRepro(t *testing.T) {
	t.Parallel()

	rec := &countingFeedbackRecorder{}
	srv := newFeedbackInputServer(rec, nil)
	payload := map[string]any{
		"query":     gqlFeedbackAliasDoc(1000),
		"variables": map[string]any{"i": map[string]any{"source": "api", "thumbs": 1, "comment": strings.Repeat("x", 20<<10)}},
	}
	if n := len(mustJSON(t, payload)); n > graphqlRequestMaxBytes {
		t.Fatalf("repro payload is %d bytes, must fit the %d-byte body limit", n, graphqlRequestMaxBytes)
	}

	w := postGraphQL(t, srv, payload)

	rec.mu.Lock()
	calls, saved := rec.calls, rec.savedBytes
	rec.mu.Unlock()
	if calls != 0 {
		t.Errorf("one request drove %d createFeedback executions (%d comment bytes); want 0", calls, saved)
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %.300s", w.Code, w.Body.String())
	}
}

// TestGraphQLGuard_MutationRequiresPOST 는 GET(링크 한 번)으로 쓰기가 실행되지
// 않는지 본다. GraphQL-over-HTTP 는 GET 을 query 전용으로 둔다.
func TestGraphQLGuard_MutationRequiresPOST(t *testing.T) {
	t.Parallel()

	vars := url.QueryEscape(`{"i":{"source":"api","thumbs":1}}`)
	mixed := `query Q { stats { total } } mutation M($i: FeedbackInput!) { createFeedback(input:$i){id} }`

	cases := []struct {
		name      string
		method    string
		rawQuery  string
		wantCode  int
		wantCalls int
	}{
		{"get_mutation", http.MethodGet, "query=" + url.QueryEscape(gqlFeedbackAliasDoc(1)) + "&variables=" + vars, http.StatusMethodNotAllowed, 0},
		{"get_mixed_selects_mutation", http.MethodGet, "query=" + url.QueryEscape(mixed) + "&operationName=M&variables=" + vars, http.StatusMethodNotAllowed, 0},
		// operationName 이 없어 실행 대상이 정해지지 않는 문서라도 mutation 이 있으면 거부(안전 쪽).
		{"get_mixed_unresolved", http.MethodGet, "query=" + url.QueryEscape(mixed) + "&variables=" + vars, http.StatusMethodNotAllowed, 0},
		{"get_mixed_unknown_name", http.MethodGet, "query=" + url.QueryEscape(mixed) + "&operationName=Nope&variables=" + vars, http.StatusMethodNotAllowed, 0},
		{"put_mutation", http.MethodPut, "query=" + url.QueryEscape(gqlFeedbackAliasDoc(1)) + "&variables=" + vars, http.StatusMethodNotAllowed, 0},
		// 대조군: POST 쿼리스트링 mutation 은 허용(쿼리스트링 우선 규칙은 그대로).
		{"post_query_string_mutation", http.MethodPost, "query=" + url.QueryEscape(gqlFeedbackAliasDoc(1)) + "&variables=" + vars, http.StatusOK, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &countingFeedbackRecorder{}
			srv := newFeedbackInputServer(rec, nil)

			req := httptest.NewRequest(tc.method, "/api/v1/graphql?"+tc.rawQuery, nil)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body = %.300s", w.Code, tc.wantCode, w.Body.String())
			}
			if tc.wantCode == http.StatusMethodNotAllowed && w.Header().Get("Allow") != http.MethodPost {
				t.Errorf("Allow = %q, want POST", w.Header().Get("Allow"))
			}
			if got := rec.count(); got != tc.wantCalls {
				t.Errorf("recorder called %d times, want %d", got, tc.wantCalls)
			}
		})
	}

	// 대조군: 같은 혼합 문서라도 GET 으로 query 연산을 고르면 실행된다.
	t.Run("get_mixed_selects_query", func(t *testing.T) {
		t.Parallel()
		rec := &countingFeedbackRecorder{}
		srv := newFeedbackInputServer(rec, nil)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/graphql?query="+url.QueryEscape(mixed)+"&operationName=Q", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code == http.StatusMethodNotAllowed || rec.count() != 0 {
			t.Errorf("status = %d, calls = %d; want the query operation to run", w.Code, rec.count())
		}
	})
}

// TestGraphQLFeedback_FieldValidation 은 createFeedback 이 REST 와 같은 검증을
// 거치는지 본다. GraphQL 경로는 본문 UTF-8 검사가 없으므로 이 검증이 유일한
// 방어선이다.
func TestGraphQLFeedback_FieldValidation(t *testing.T) {
	t.Parallel()

	marker := feedbackLeakMarker
	doc := `mutation($i: FeedbackInput!) { createFeedback(input:$i){id} }`

	cases := []struct {
		name    string
		input   map[string]any
		wantErr string
	}{
		{"comment_nul", map[string]any{"source": "api", "thumbs": 1, "comment": marker + "\x00"}, "comment: contains NUL"},
		{"query_one_over", map[string]any{"source": "api", "thumbs": 1, "query": marker + koreanOfBytes(feedbackQueryMaxBytes)}, "query: exceeds maximum length"},
		{"session_nul", map[string]any{"source": "api", "thumbs": 1, "sessionId": "s\x00"}, "session_id: contains NUL"},
		{"metadata_key_nul", map[string]any{"source": "api", "thumbs": 1, "metadata": map[string]any{marker + "\x00": 1}}, "metadata: contains NUL"},
		{"metadata_nested_value_nul", map[string]any{"source": "api", "thumbs": 1, "metadata": map[string]any{"a": []any{marker + "\x00"}}}, "metadata: contains NUL"},
		{"metadata_too_deep", map[string]any{"source": "api", "thumbs": 1, "metadata": nestedMetadata(feedbackMetadataMaxDepth+1, marker)}, "metadata: nested too deeply"},
		{"document_id_not_uuid", map[string]any{"source": "api", "thumbs": 1, "documentId": marker}, "document_id must be a UUID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &countingFeedbackRecorder{}
			srv := newFeedbackInputServer(rec, nil)

			w := postGraphQL(t, srv, map[string]any{"query": doc, "variables": map[string]any{"i": tc.input}})

			if !strings.Contains(w.Body.String(), tc.wantErr) {
				t.Errorf("body = %.300s; want an error containing %q", w.Body.String(), tc.wantErr)
			}
			if got := rec.count(); got != 0 {
				t.Errorf("recorder called %d times, want 0", got)
			}
			if strings.Contains(w.Body.String(), feedbackLeakMarker) {
				t.Errorf("response leaks the input: %.300s", w.Body.String())
			}
		})
	}
}

// TestGraphQLErrorReflection_NotLogged 는 항목 4b 의 판단을 고정한다.
// graphql-go 는 구문 오류(원문 줄)와 변수 강제 변환 오류(변수 값)를 응답의
// errors[].message 에 되돌린다. 이 반사는 요청자 자신에게만 가고 서버 로그에는
// 남지 않아야 한다(FormatErrorFn 을 쓰지 않는 근거).
//
// t.Parallel 을 쓰지 않는다: slog 기본 로거를 바꾼다.
func TestGraphQLErrorReflection_NotLogged(t *testing.T) {
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	marker := feedbackLeakMarker
	payloads := map[string]map[string]any{
		"syntax_error": {"query": `{ search(query:"` + marker + `") { count } ` + marker + ` ( }`},
		"variable_coercion_error": {
			"query":     `mutation($i: FeedbackInput!) { createFeedback(input:$i){id} }`,
			"variables": map[string]any{"i": map[string]any{"source": "api", "thumbs": marker}},
		},
	}
	for name, payload := range payloads {
		rec := &countingFeedbackRecorder{}
		srv := newFeedbackInputServer(rec, nil)
		w := postGraphQL(t, srv, payload)

		// 양성 대조군: 표식이 실제로 서버에 닿아 응답으로 반사됐다. 이게 없으면
		// "로그에 없음"은 표식이 애초에 처리되지 않아서일 수 있다.
		if !strings.Contains(w.Body.String(), marker) {
			t.Errorf("%s: response does not reflect the marker; control failed: %.300s", name, w.Body.String())
		}
		if rec.count() != 0 {
			t.Errorf("%s: recorder called %d times, want 0", name, rec.count())
		}
	}
	// 로그 캡처 자체가 동작하는지(requestLogger 가 경로를 남김) 확인한 뒤 표식을 찾는다.
	if !strings.Contains(logs.String(), "/api/v1/graphql") {
		t.Fatalf("log capture saw no request lines; control failed: %q", logs.String())
	}
	if strings.Contains(logs.String(), marker) {
		t.Errorf("server log contains the reflected input: %s", logs.String())
	}
}

// TestFeedbackInput_RejectionNotLogged 는 거부된 feedback 요청의 원문이 서버
// 로그에 남지 않는지 본다. t.Parallel 을 쓰지 않는다: slog 기본 로거를 바꾼다.
func TestFeedbackInput_RejectionNotLogged(t *testing.T) {
	t.Setenv("FEEDBACK_EVIDENCE_ENABLED", "1")
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	marker := feedbackLeakMarker
	srv := newFeedbackInputServer(&countingFeedbackRecorder{}, &countingEvidenceVoter{})
	for path, body := range map[string]string{
		"/api/v1/feedback":          `{"source":"api","thumbs":1,"comment":"` + marker + `\u0000","metadata":{"` + marker + `":"\u0000"}}`,
		"/api/v1/feedback/evidence": `{"conversation_id":"c","query":"` + marker + `\u0000","document_id":"` + uuid.NewString() + `","thumbs":1}`,
	} {
		w := doRawRequest(srv, http.MethodPost, path, []byte(body))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", path, w.Code)
		}
	}
	if !strings.Contains(logs.String(), "/api/v1/feedback") {
		t.Fatalf("log capture saw no request lines; control failed: %q", logs.String())
	}
	if strings.Contains(logs.String(), marker) {
		t.Errorf("server log contains the rejected input: %s", logs.String())
	}
}

// TestDecodeBoundedJSON_LoneSurrogateBecomesReplacementChar 는 항목 4a 의
// 판단(문서화만)을 고정한다. 원 본문이 유효한 UTF-8 인 "\ud800" 이스케이프는
// UTF-8 검사를 통과하고 encoding/json 이 U+FFFD 로 바꾼다. 거부하지 않는다.
func TestDecodeBoundedJSON_LoneSurrogateBecomesReplacementChar(t *testing.T) {
	t.Parallel()

	var dst struct {
		Query string `json:"query"`
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"query":"a\ud800b"}`))
	if err := decodeBoundedJSON(httptest.NewRecorder(), req, 1<<10, &dst); err != nil {
		t.Fatalf("decodeBoundedJSON: %v", err)
	}
	if dst.Query != "a�b" {
		t.Errorf("query = %q, want %q", dst.Query, "a�b")
	}
	if err := search.ValidateInputText("query", dst.Query, 0); err != nil {
		t.Errorf("U+FFFD should pass input validation, got %v", err)
	}
}

// uuidVariants 는 uuid.Parse 가 받아들이는 비표준 형식들이다. PostgreSQL uuid
// 입력은 이 중 urn 형식을 22P02 로 거부하므로, 검증을 통과한 값은 정규형으로
// 바뀌어 저장소에 가야 한다(#286 deep-verify).
func uuidVariants(id uuid.UUID) map[string]string {
	hex := strings.ReplaceAll(id.String(), "-", "")
	return map[string]string{
		"urn":       "urn:uuid:" + id.String(),
		"braces":    "{" + id.String() + "}",
		"hex32":     hex,
		"uppercase": strings.ToUpper(id.String()),
		"urn_upper": "urn:uuid:" + strings.ToUpper(id.String()),
		"canonical": id.String(),
	}
}

func TestFeedbackInput_DocumentIDCanonicalized(t *testing.T) {
	t.Setenv("FEEDBACK_EVIDENCE_ENABLED", "1")
	id := uuid.New()
	want := id.String()

	for name, variant := range uuidVariants(id) {
		t.Run("rest_"+name, func(t *testing.T) {
			rec := &countingFeedbackRecorder{}
			srv := newFeedbackInputServer(rec, nil)
			w := doRawRequest(srv, http.MethodPost, "/api/v1/feedback",
				mustJSON(t, map[string]any{"source": "api", "thumbs": 1, "document_id": variant}))
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201; body = %s", w.Code, w.Body.String())
			}
			rec.mu.Lock()
			got := rec.last.DocumentID
			rec.mu.Unlock()
			if got == nil || *got != want {
				t.Errorf("document_id passed to store = %v, want canonical %q", got, want)
			}
		})

		t.Run("evidence_"+name, func(t *testing.T) {
			voter := &countingEvidenceVoter{}
			srv := newFeedbackInputServer(&countingFeedbackRecorder{}, voter)
			w := doRawRequest(srv, http.MethodPost, "/api/v1/feedback/evidence",
				mustJSON(t, map[string]any{"conversation_id": "c", "query": "q", "document_id": variant, "thumbs": 1}))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
			voter.mu.Lock()
			got := voter.lastDocID
			voter.mu.Unlock()
			if got != want {
				t.Errorf("document_id passed to store = %q, want canonical %q", got, want)
			}
		})

		t.Run("graphql_"+name, func(t *testing.T) {
			rec := &countingFeedbackRecorder{}
			srv := newFeedbackInputServer(rec, nil)
			w := postGraphQL(t, srv, map[string]any{
				"query":     `mutation($i: FeedbackInput!) { createFeedback(input:$i){id} }`,
				"variables": map[string]any{"i": map[string]any{"source": "api", "thumbs": 1, "documentId": variant}},
			})
			rec.mu.Lock()
			got := rec.last.DocumentID
			rec.mu.Unlock()
			if got == nil || *got != want {
				t.Errorf("document_id passed to store = %v, want canonical %q; body = %s", got, want, w.Body.String())
			}
		})
	}
}

func TestFeedbackInput_GoldenForeignKeyAndRank(t *testing.T) {
	t.Parallel()

	fkErr := fmt.Errorf("golden: upsert judgment: %w", &pgconn.PgError{Code: "23503"})
	item := func(rank any) []map[string]any {
		return []map[string]any{{"document_id": uuid.NewString(), "judgment": "relevant", "rank": rank}}
	}
	judgments := func(items []map[string]any) []byte {
		return mustJSON(t, map[string]any{"query_id": uuid.NewString(), "judgments": items})
	}
	feedback := func(items []map[string]any) []byte {
		return mustJSON(t, map[string]any{"query_text": "q", "source": "hermes", "judge": "llm", "judgments": items})
	}

	cases := []struct {
		name      string
		path      string
		body      []byte
		upsertErr error
		wantCode  int
		wantMsg   string
	}{
		{"judgments_fk_is_400", "/api/v1/golden/judgments", judgments(item(1)), fkErr, http.StatusBadRequest, "not found"},
		{"feedback_fk_is_400", "/api/v1/golden/feedback", feedback(item(1)), fkErr, http.StatusBadRequest, "not found"},
		{"judgments_other_error_stays_500", "/api/v1/golden/judgments", judgments(item(1)), errors.New("boom"), http.StatusInternalServerError, "internal server error"},
		{"judgments_rank_zero", "/api/v1/golden/judgments", judgments(item(0)), nil, http.StatusOK, ""},
		{"judgments_rank_at_max", "/api/v1/golden/judgments", judgments(item(goldenRankMax)), nil, http.StatusOK, ""},
		{"judgments_rank_negative", "/api/v1/golden/judgments", judgments(item(-1)), nil, http.StatusBadRequest, "rank must be between"},
		{"judgments_rank_one_over", "/api/v1/golden/judgments", judgments(item(goldenRankMax + 1)), nil, http.StatusBadRequest, "rank must be between"},
		{"judgments_rank_int4_overflow", "/api/v1/golden/judgments", judgments(item(int64(1) << 31)), nil, http.StatusBadRequest, "rank must be between"},
		{"feedback_rank_negative", "/api/v1/golden/feedback", feedback(item(-1)), nil, http.StatusBadRequest, "rank must be between"},
		{"feedback_rank_one_over", "/api/v1/golden/feedback", feedback(item(goldenRankMax + 1)), nil, http.StatusBadRequest, "rank must be between"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := &stubGoldenSet{byTextID: uuid.New(), byTextFound: true, upsertSaved: 1, upsertErr: tc.upsertErr}
			srv := newGoldenTestServer(stub, nil)

			w := doRawRequest(srv, http.MethodPost, tc.path, tc.body)

			if w.Code != tc.wantCode || !strings.Contains(w.Body.String(), tc.wantMsg) {
				t.Fatalf("status = %d body = %s; want %d containing %q", w.Code, w.Body.String(), tc.wantCode, tc.wantMsg)
			}
			if strings.HasPrefix(tc.wantMsg, "rank") && stub.upsertJudgments != nil {
				t.Errorf("store reached despite an out-of-range rank")
			}
		})
	}
}
