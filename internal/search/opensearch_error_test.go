package search

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// #286 항목 2: OpenSearch 비-200 응답 본문이 오류 문자열·로그로 새지 않는지.
//
// query_shard_exception / parse_exception 의 reason 에는 질의 조각이 들어간다.
// 오류 문자열에는 상태 코드와 정제된 error.type 만 남아야 한다.

// osLeakMarker 는 오류 문자열·로그에 OpenSearch 응답 본문(= 질의 조각)이
// 새는지 확인하는 표식이다.
const osLeakMarker = "SENTINEL-질의조각-286"

func osErrorServer(t *testing.T, status int, body string) *OpenSearchClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewOpenSearchClient(srv.URL, "sb-chunks", 5*time.Second)
}

func TestOpenSearchClient_ErrorBodySanitized(t *testing.T) {
	t.Parallel()

	shardErr := `{"error":{"root_cause":[{"type":"query_shard_exception","reason":"failed to create query: ` + osLeakMarker + `"}],` +
		`"type":"query_shard_exception","reason":"failed to create query: ` + osLeakMarker + `","index":"sb-chunks"},"status":400}`

	cases := []struct {
		name     string
		status   int
		body     string
		wantType string
	}{
		{"query_shard_exception", http.StatusBadRequest, shardErr, "query_shard_exception"},
		{"parse_exception", http.StatusBadRequest, `{"error":{"type":"parse_exception","reason":"` + osLeakMarker + `"},"status":400}`, "parse_exception"},
		{"string_error", http.StatusInternalServerError, `{"error":"` + osLeakMarker + `"}`, "unknown"},
		{"not_json", http.StatusBadGateway, `<html>` + osLeakMarker + `</html>`, "unknown"},
		{"type_with_injection", http.StatusBadRequest, `{"error":{"type":"a\"b\n` + osLeakMarker + `"}}`, "unknown"},
		{"type_uppercase", http.StatusBadRequest, `{"error":{"type":"Query_Shard"}}`, "unknown"},
		{"type_too_long", http.StatusBadRequest, `{"error":{"type":"` + strings.Repeat("a", 65) + `"}}`, "unknown"},
		// 64KB 를 넘는 본문은 잘린 채 읽혀 JSON 해석에 실패한다 → unknown.
		{"oversized_body", http.StatusBadRequest,
			`{"error":{"reason":"` + strings.Repeat(osLeakMarker, (70<<10)/len(osLeakMarker)) + `","type":"query_shard_exception"}}`, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := osErrorServer(t, tc.status, tc.body)

			_, err := c.Search(context.Background(), model.SearchQuery{Query: "q"}, 10)
			if err == nil {
				t.Fatal("Search() returned nil error for a non-200 response")
			}
			msg := err.Error()
			if strings.Contains(msg, osLeakMarker) {
				t.Errorf("error leaks the response body: %.300s", msg)
			}
			if !strings.Contains(msg, "status "+strconv.Itoa(tc.status)) {
				t.Errorf("error = %q, want the status code", msg)
			}
			if want := "error.type=" + tc.wantType; !strings.Contains(msg, want) {
				t.Errorf("error = %q, want %q", msg, want)
			}
		})
	}
}

// TestOpenSearchClient_SuccessBodyCapped 는 200 응답 본문도 상한 안에서만
// 읽는지 본다. 정상 응답은 수백 KB 라 상한에 닿지 않는다.
func TestOpenSearchClient_SuccessBodyCapped(t *testing.T) {
	t.Parallel()

	big := `{"hits":{"hits":[]},"pad":"` + strings.Repeat("x", opensearchMaxResponseBytes) + `"}`
	c := osErrorServer(t, http.StatusOK, big)
	if _, err := c.Search(context.Background(), model.SearchQuery{Query: "q"}, 10); err == nil ||
		!strings.Contains(err.Error(), "exceeds") {
		t.Errorf("Search() err = %v, want a size-limit error", err)
	}

	// 대조군: 상한 안의 정상 응답은 그대로 해석된다.
	ok := osErrorServer(t, http.StatusOK, `{"hits":{"hits":[{"_score":1,"_source":{"document_id":"`+uuid.NewString()+`","chunk_index":0}}]}}`)
	res, err := ok.Search(context.Background(), model.SearchQuery{Query: "q"}, 10)
	if err != nil || len(res) != 1 {
		t.Errorf("control: Search() = %d results, err %v; want 1 result", len(res), err)
	}
}

type osSyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *osSyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *osSyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestServiceSearch_OpenSearchErrorNotLogged 는 search.Service 가 레인 실패를
// slog.Warn 으로 남길 때 응답 본문(질의 조각)이 로그에 없는지 본다.
//
// t.Parallel 을 쓰지 않는다: slog 기본 로거를 바꾼다.
func TestServiceSearch_OpenSearchErrorNotLogged(t *testing.T) {
	logs := &osSyncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	c := osErrorServer(t, http.StatusBadRequest,
		`{"error":{"type":"query_shard_exception","reason":"`+osLeakMarker+`"},"status":400}`)
	svc := NewService(&mockDocSearcher{}, disabledEmbedder{}).WithOpenSearch(c)

	if _, err := svc.Search(context.Background(), model.SearchQuery{Query: "q"}); err != nil {
		t.Fatalf("Search() error: %v (the lane failure must be soft)", err)
	}
	// 양성 대조군: 레인 실패 경고가 실제로 로그에 남았다.
	if !strings.Contains(logs.String(), "opensearch lane failed") || !strings.Contains(logs.String(), "query_shard_exception") {
		t.Fatalf("expected the lane-failure warning with the error type; log = %q", logs.String())
	}
	if strings.Contains(logs.String(), osLeakMarker) {
		t.Errorf("log leaks the OpenSearch response body: %s", logs.String())
	}
}

// TestOpenSearchClient_ErrorBodyDrainedForReuse 는 오류 본문을 64KB 만 읽고
// 끝내도 남은 부분을 상한 안에서 버려 keep-alive 연결을 다시 쓰는지 본다.
// 새 연결 수를 서버의 ConnState 로 센다(드레인이 없으면 두 번째 요청이 새
// 연결을 연다).
//
// t.Parallel 을 쓰지 않고 전용 Transport 를 쓴다: 클라이언트 기본값은 공유
// http.DefaultTransport 이고, 다른 테스트(특히 64MB 를 흘리는 드레인 상한
// 테스트)와 동시에 돌면 연결 재사용이 흔들려 가짜 실패가 났다(-race -count=20
// 에서 20/20). 단독 실행에서는 한 번도 흔들리지 않았다.
func TestOpenSearchClient_ErrorBodyDrainedForReuse(t *testing.T) {
	var newConns atomic.Int32
	body := `{"error":{"type":"query_shard_exception","reason":"` + strings.Repeat("x", 200<<10) + `"}}`
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(body))
	}))
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			newConns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	c := NewOpenSearchClient(srv.URL, "sb-chunks", 5*time.Second)
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	c.httpClient.Transport = transport

	for i := 0; i < 2; i++ {
		if _, err := c.Search(context.Background(), model.SearchQuery{Query: "q"}, 10); err == nil {
			t.Fatal("Search() returned nil error for a 400 response")
		}
	}
	if got := newConns.Load(); got != 1 {
		t.Errorf("new connections = %d, want 1 (the drained connection is reused)", got)
	}
}

// TestOpenSearchClient_ErrorBodyDrainIsBounded 는 드레인에 상한이 있는지 본다
// (무한 드레인 금지). 서버가 64MB 오류 본문을 흘려보낼 때 클라이언트가 실제로
// 받아 간 양을 서버 쪽에서 센다. 상한이 있으면 읽기 64KB + 드레인 1MB 에 소켓
// 버퍼만큼만 전달되고 쓰기가 실패한다. 연결 수로는 이것을 가를 수 없다 —
// 끝까지 읽은 연결도 재사용되지 않는 경우가 있어 결과가 흔들렸다.
func TestOpenSearchClient_ErrorBodyDrainIsBounded(t *testing.T) {
	t.Parallel()

	const total = 64 << 20
	var written atomic.Int64
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer close(done)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(total))
		w.WriteHeader(http.StatusBadRequest)
		chunk := []byte(strings.Repeat("x", 32<<10))
		for sent := 0; sent < total; sent += len(chunk) {
			n, err := w.Write(chunk)
			written.Add(int64(n))
			if err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	c := NewOpenSearchClient(srv.URL, "sb-chunks", 5*time.Second)

	if _, err := c.Search(context.Background(), model.SearchQuery{Query: "q"}, 10); err == nil {
		t.Fatal("Search() returned nil error for a 400 response")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server handler did not finish; the client neither read nor closed the body")
	}
	// 소켓 버퍼(수 MB)를 감안해도 64MB 전부가 나가면 드레인에 상한이 없는 것이다.
	if got := written.Load(); got >= total/2 {
		t.Errorf("server delivered %d of %d bytes; the drain is not bounded", got, total)
	}
}
