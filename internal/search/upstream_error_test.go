package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/baekenough/second-brain/internal/httperr"
)

// #288 3항: 외부 API(임베딩·리랭크)의 오류 본문이 반환 오류·로그·span 으로
// 새지 않는지, 재시도 판정이 상태 코드로만 이뤄지는지(변경 전과 같은 횟수),
// 성공 본문에 상한이 있는지, 오류 본문을 상한 안에서 버려 연결을 다시
// 쓰는지 본다.
//
// 기본 slog 로거와 전역 TracerProvider 를 바꾸는 테스트는 t.Parallel 을 쓰지
// 않는다. Go 테스트 러너는 병렬 테스트를 비병렬 테스트가 모두 끝난 뒤에
// 재개하므로 전역 상태가 겹치지 않는다(embed_otel_test.go 와 같은 방식).

const (
	upstreamSentinelBody = "SENTINEL-본문-ZX9Q"
	upstreamSentinelNum  = "01000009999"
)

// upstreamSentinels 는 오류·로그·span 어디에도 나오면 안 되는 문자열이다.
// 숫자만 남긴 형태(00009999)도 함께 본다.
var upstreamSentinels = []string{upstreamSentinelBody, "ZX9Q", upstreamSentinelNum, "010-0000-9999", "00009999"}

// sentinelErrorBody 는 OpenAI 모양 오류 본문이다. message 에 센티널(질의
// 조각·번호)이 들어 있고 type/code 는 안전한 모양이다.
func sentinelErrorBody() string {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": "input echoed back: " + upstreamSentinelBody + " / " + upstreamSentinelNum,
		"type":    "invalid_request_error",
		"code":    "bad_input_code",
	}})
	return string(b)
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// captureDefaultSlog 는 기본 slog 로거를 버퍼로 바꾸고 테스트가 끝나면
// 되돌린다.
func captureDefaultSlog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func assertNoUpstreamSentinel(t *testing.T, sink, value string) {
	t.Helper()
	for _, s := range upstreamSentinels {
		if strings.Contains(value, s) {
			t.Errorf("%s leaks sentinel %q", sink, s)
		}
	}
}

// TestUpstreamLogCapture_SeesSentinel 은 캡처 장치 대조군이다. 캡처가 고장
// 나면 아래 "센티널 없음" 검사가 모두 거짓으로 통과하므로, 같은 장치로
// 센티널이 실제로 보이는지 먼저 확인한다.
func TestUpstreamLogCapture_SeesSentinel(t *testing.T) {
	logs := captureDefaultSlog(t)
	slog.Warn("capture control", "value", upstreamSentinelBody+" "+upstreamSentinelNum)
	for _, s := range []string{upstreamSentinelBody, upstreamSentinelNum} {
		if !strings.Contains(logs.String(), s) {
			t.Fatalf("capture did not see %q; negative checks in this file would be meaningless", s)
		}
	}
}

// TestEmbedUpstreamError_NoBodyAndStatusBasedRetry 는 단건·배치 임베딩이
// 비-200 을 받았을 때를 본다.
//
//   - 호출 횟수: 429·5xx 는 1 + embedMaxRetries 회, 그 밖의 4xx 는 1회
//     (변경 전과 같음 — 판정은 상태 코드로만 한다)
//   - 오류: 기존 접두사 + 정제한 error.type/code 는 있고 본문 센티널은 없음
//   - 로그: 재시도 이벤트가 찍혔고(양성 대조) 그 안에 센티널이 없음
//   - span: 오류 상태가 기록됐고(양성 대조) 센티널이 없음
func TestEmbedUpstreamError_NoBodyAndStatusBasedRetry(t *testing.T) {
	cases := []struct {
		status    int
		wantCalls int32
	}{
		{http.StatusBadRequest, 1},
		{http.StatusUnauthorized, 1},
		{http.StatusUnprocessableEntity, 1},
		{http.StatusTooManyRequests, 1 + embedMaxRetries},
		{http.StatusInternalServerError, 1 + embedMaxRetries},
		{http.StatusServiceUnavailable, 1 + embedMaxRetries},
	}
	for _, batch := range []bool{false, true} {
		for _, tc := range cases {
			name := fmt.Sprintf("batch=%v/status=%d", batch, tc.status)
			t.Run(name, func(t *testing.T) {
				logs := captureDefaultSlog(t)
				exp := withInMemoryTracer(t)

				var calls atomic.Int32
				body := sentinelErrorBody()
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					if tc.status == http.StatusTooManyRequests {
						w.Header().Set("Retry-After", "0")
					}
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(body))
				}))
				t.Cleanup(srv.Close)

				c := NewEmbedClient(srv.URL, "test-key", "", "text-embedding-3-small", 4)
				var err error
				prefix := "embed API status"
				retryMsg := "embed: retrying after transient error"
				if batch {
					_, err = c.EmbedBatch(context.Background(), []string{"q1", "q2"})
					prefix = "embed batch API status"
					retryMsg = "embed batch: retrying after transient error"
				} else {
					_, err = c.Embed(context.Background(), "q")
				}
				if err == nil {
					t.Fatal("expected an error")
				}
				if got := calls.Load(); got != tc.wantCalls {
					t.Errorf("calls = %d, want %d (retry decision must stay status-based)", got, tc.wantCalls)
				}

				wantMsg := fmt.Sprintf("%s %d (error.type=invalid_request_error, code=bad_input_code)", prefix, tc.status)
				if !strings.Contains(err.Error(), wantMsg) {
					t.Errorf("err = %q, want it to contain %q", err, wantMsg)
				}
				var se *httperr.StatusError
				if !errors.As(err, &se) || se.StatusCode != tc.status {
					t.Errorf("errors.As(StatusError) failed or wrong status: %v", err)
				}
				assertNoUpstreamSentinel(t, "returned error", err.Error())

				// 로그 양성 대조: 재시도하는 상태에서는 재시도 이벤트가 오류
				// 속성과 함께 찍혀야 한다.
				out := logs.String()
				if tc.wantCalls > 1 {
					if !strings.Contains(out, retryMsg) || !strings.Contains(out, "error.type=invalid_request_error") {
						t.Fatalf("retry log event missing (positive control); logs=%d bytes", len(out))
					}
				}
				assertNoUpstreamSentinel(t, "logs", out)

				// span 양성 대조: 오류 상태 설명에 접두사가 있어야 한다.
				spans := exp.GetSpans()
				sawStatus := false
				for _, s := range spans {
					if strings.Contains(s.Status.Description, prefix) {
						sawStatus = true
					}
				}
				if !sawStatus {
					t.Fatalf("no span recorded the error status (positive control); spans=%v", spanNames(spans))
				}
				raw, _ := json.Marshal(spans)
				assertNoUpstreamSentinel(t, "spans", string(raw))
			})
		}
	}
}

// TestEmbedSuccessBodyLimit 는 성공 응답이 상한을 넘으면 ErrBodyTooLarge 로
// 끝나고 재시도하지 않는지 본다. dim=1 이면 상한은 n×32B + 1MiB 다.
func TestEmbedSuccessBodyLimit(t *testing.T) {
	t.Parallel()
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprintf("batch=%v", batch), func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			oversized := `{"data":[{"index":0,"embedding":[0.1]}],"pad":"` + strings.Repeat("p", 2<<20) + `"}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(oversized))
			}))
			t.Cleanup(srv.Close)

			c := NewEmbedClient(srv.URL, "test-key", "", "text-embedding-3-small", 1)
			var err error
			if batch {
				_, err = c.EmbedBatch(context.Background(), []string{"a", "b"})
			} else {
				_, err = c.Embed(context.Background(), "a")
			}
			if !errors.Is(err, httperr.ErrBodyTooLarge) {
				t.Fatalf("err = %v, want ErrBodyTooLarge", err)
			}
			if calls.Load() != 1 {
				t.Errorf("calls = %d, want 1 (an oversized body is not retried)", calls.Load())
			}
		})
	}
}

// TestEmbedResponseLimit_ScalesWithInputs 는 배치 상한이 입력 개수와 차원을
// 따라 커지는지 본다. OpenAI 최대 입력 2048개 × 1536차원 정상 응답(약
// 63MB)을 고정 상한으로 거부하지 않아야 한다.
func TestEmbedResponseLimit_ScalesWithInputs(t *testing.T) {
	t.Parallel()
	c := NewEmbedClient("http://unused", "k", "", "text-embedding-3-small", 1536)
	if got, want := c.responseLimit(2048), int64(2048*1536*48+1<<20); got != want {
		t.Fatalf("responseLimit(2048) = %d, want %d", got, want)
	}
	unknown := NewEmbedClient("http://unused", "k", "", "text-embedding-3-small", 0)
	if got, want := unknown.responseLimit(1), int64(3072*48+1<<20); got != want {
		t.Fatalf("unknown dims: responseLimit(1) = %d, want %d", got, want)
	}
}

// newReuseServer 는 status 와 큰 오류 본문을 돌려주고 새 연결 수를 센다.
func newReuseServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var newConns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			newConns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, &newConns
}

// bigErrorBody 는 읽기 상한(64KB)을 넘는 오류 본문이다. 드레인이 없으면
// 남은 부분 때문에 연결을 다시 쓰지 못한다.
func bigErrorBody() string {
	return `{"error":{"type":"invalid_request_error","message":"` + strings.Repeat("x", 200<<10) + `"}}`
}

// TestUpstreamErrorBodyDrainedForReuse 는 오류 본문을 상한 안에서 버려
// keep-alive 연결을 다시 쓰는지 본다(opensearch_error_test.go 와 같은 틀).
// 전용 Transport 를 써서 다른 테스트와 연결 풀을 공유하지 않는다.
func TestUpstreamErrorBodyDrainedForReuse(t *testing.T) {
	t.Run("embed", func(t *testing.T) {
		srv, newConns := newReuseServer(t, http.StatusBadRequest, bigErrorBody())
		c := NewEmbedClient(srv.URL, "test-key", "", "text-embedding-3-small", 4)
		tr := &http.Transport{}
		t.Cleanup(tr.CloseIdleConnections)
		c.client.Transport = tr
		for i := 0; i < 2; i++ {
			if _, err := c.Embed(context.Background(), "q"); err == nil {
				t.Fatal("expected error for 400")
			}
		}
		if got := newConns.Load(); got != 1 {
			t.Errorf("new connections = %d, want 1 (the drained connection is reused)", got)
		}
	})
	t.Run("rerank", func(t *testing.T) {
		srv, newConns := newReuseServer(t, http.StatusBadGateway, bigErrorBody())
		r := NewHTTPReranker(srv.URL, "", "m", 0)
		tr := &http.Transport{}
		t.Cleanup(tr.CloseIdleConnections)
		r.client.Transport = tr
		for i := 0; i < 2; i++ {
			if _, err := r.Rerank(context.Background(), "q", []string{"a"}); err == nil {
				t.Fatal("expected error for 502")
			}
		}
		if got := newConns.Load(); got != 1 {
			t.Errorf("new connections = %d, want 1 (the drained connection is reused)", got)
		}
	})
}

// TestRerankUpstreamError_KeepsFixedMessage 는 리랭커가 OpenAI 모양 오류
// 본문을 받아도 오류 문구가 "rerank API status %d" 그대로이고 본문 센티널이
// 없는지 본다.
func TestRerankUpstreamError_KeepsFixedMessage(t *testing.T) {
	t.Parallel()
	body := sentinelErrorBody()
	if !strings.Contains(body, upstreamSentinelBody) {
		t.Fatal("positive control: body must carry the sentinel")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	_, err := NewHTTPReranker(srv.URL, "", "m", 0).Rerank(context.Background(), "q", []string{"a", "b"})
	if err == nil || err.Error() != "rerank API status 500" {
		t.Fatalf("err = %v, want exactly %q", err, "rerank API status 500")
	}
}

// TestRerankSuccessBodyLimit 는 리랭크 성공 응답에 상한(1MiB + 문서당 256B)이
// 있는지 본다.
func TestRerankSuccessBodyLimit(t *testing.T) {
	t.Parallel()
	oversized := `{"results":[],"pad":"` + strings.Repeat("p", 2<<20) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(oversized))
	}))
	t.Cleanup(srv.Close)
	_, err := NewHTTPReranker(srv.URL, "", "m", 0).Rerank(context.Background(), "q", []string{"a"})
	if !errors.Is(err, httperr.ErrBodyTooLarge) {
		t.Fatalf("err = %v, want ErrBodyTooLarge", err)
	}
}

// TestLocalEmbedderUpstreamError_NoBody 는 Ollama 모양 오류({"error":"…"})
// 본문이 오류에 실리지 않는지, 성공 본문 상한이 있는지 본다.
func TestLocalEmbedderUpstreamError_NoBody(t *testing.T) {
	t.Parallel()
	t.Run("error body", func(t *testing.T) {
		t.Parallel()
		body := `{"error":"model failed on ` + upstreamSentinelBody + ` ` + upstreamSentinelNum + `"}`
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		_, err := NewLocalEmbedder(srv.URL, "m", 4).Embed(context.Background(), "q")
		if err == nil || err.Error() != "local embed API status 500" {
			t.Fatalf("err = %v, want exactly %q", err, "local embed API status 500")
		}
	})
	t.Run("oversized success", func(t *testing.T) {
		t.Parallel()
		oversized := `{"embedding":[0.1],"pad":"` + strings.Repeat("p", 2<<20) + `"}`
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(oversized))
		}))
		t.Cleanup(srv.Close)
		_, err := NewLocalEmbedder(srv.URL, "m", 4).Embed(context.Background(), "q")
		if !errors.Is(err, httperr.ErrBodyTooLarge) {
			t.Fatalf("err = %v, want ErrBodyTooLarge", err)
		}
	})
}

// prettyEmbeddingBody 는 OpenAI 가 실제로 보내는 모양을 흉내 낸 임베딩 응답이다:
// step 칸 들여쓰기, float 한 줄에 하나, float32 값을 float64 최단 표기로
// 적는다(약 20자). crlf 이면 개행을 CRLF 로 쓴다.
func prettyEmbeddingBody(n, dims, step int, crlf bool) []byte {
	nl := "\n"
	if crlf {
		nl = "\r\n"
	}
	ind := func(d int) string { return strings.Repeat(" ", d*step) }
	r := rand.New(rand.NewSource(1))
	var b strings.Builder
	b.WriteString("{" + nl + ind(1) + `"object": "list",` + nl + ind(1) + `"data": [` + nl)
	v := make([]float64, dims)
	for i := 0; i < n; i++ {
		var norm float64
		for j := range v {
			v[j] = r.NormFloat64()
			norm += v[j] * v[j]
		}
		norm = math.Sqrt(norm)
		b.WriteString(ind(2) + "{" + nl + ind(3) + `"object": "embedding",` + nl +
			ind(3) + `"index": ` + strconv.Itoa(i) + "," + nl + ind(3) + `"embedding": [` + nl)
		for j := range v {
			b.WriteString(ind(4) + strconv.FormatFloat(float64(float32(v[j]/norm)), 'g', -1, 64))
			if j < dims-1 {
				b.WriteString(",")
			}
			b.WriteString(nl)
		}
		b.WriteString(ind(3) + "]" + nl + ind(2) + "}")
		if i < n-1 {
			b.WriteString(",")
		}
		b.WriteString(nl)
	}
	b.WriteString(ind(1) + "]," + nl + ind(1) + `"model": "text-embedding-3-large",` + nl +
		ind(1) + `"usage": {"prompt_tokens": 1, "total_tokens": 1}` + nl + "}" + nl)
	return []byte(b.String())
}

// TestEmbedBatchLimitMargin_PrettyPrinted 는 들여쓰기한 정상 배치 응답이
// 상한에 걸리지 않는지 본다(보안 리뷰 후속). 상한 초과는 재시도하지 않으므로
// 정상 응답을 자르면 백필이 매 틱 실패한다. 4칸 들여쓰기 + CRLF 까지 통과해야
// 한다. n=128 은 1MiB 여유분이 float 당 여유 부족을 가리지 못할 만큼 큰
// 값이다(float 당 32B 였다면 4칸 들여쓰기 약 38.4B/float 로 n>53 부터 넘는다).
func TestEmbedBatchLimitMargin_PrettyPrinted(t *testing.T) {
	t.Parallel()
	const n, dims = 128, 3072
	for _, tc := range []struct {
		step int
		crlf bool
	}{{2, false}, {2, true}, {4, false}, {4, true}} {
		t.Run(fmt.Sprintf("indent=%d/crlf=%v", tc.step, tc.crlf), func(t *testing.T) {
			t.Parallel()
			body := prettyEmbeddingBody(n, dims, tc.step, tc.crlf)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body)
			}))
			t.Cleanup(srv.Close)
			c := NewEmbedClient(srv.URL, "test-key", "", "text-embedding-3-large", dims)
			limit := c.responseLimit(n)
			t.Logf("body=%d limit=%d ratio=%.3f bytes/float=%.2f",
				len(body), limit, float64(len(body))/float64(limit), float64(len(body))/float64(n*dims))

			texts := make([]string, n)
			for i := range texts {
				texts[i] = "t"
			}
			vecs, err := c.EmbedBatch(context.Background(), texts)
			if err != nil {
				t.Fatalf("a well-formed %d-byte response was rejected (limit %d): %v", len(body), limit, err)
			}
			if len(vecs) != n || len(vecs[n-1]) != dims {
				t.Fatalf("got %d vectors (last dim %d), want %d×%d", len(vecs), len(vecs[n-1]), n, dims)
			}
		})
	}
}

// TestEmbedBodyTooLarge_LogsLimitInputsOnly 는 상한 초과 때 limit·n·dims 만
// 로그에 남고 본문은 남지 않는지 본다. 이벤트가 찍혔는지 먼저 확인한다.
func TestEmbedBodyTooLarge_LogsLimitInputsOnly(t *testing.T) {
	logs := captureDefaultSlog(t)
	oversized := `{"data":[],"pad":"` + upstreamSentinelBody + strings.Repeat("p", 2<<20) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(oversized))
	}))
	t.Cleanup(srv.Close)
	c := NewEmbedClient(srv.URL, "test-key", "", "text-embedding-3-small", 1)
	if _, err := c.EmbedBatch(context.Background(), []string{"a", "b"}); !errors.Is(err, httperr.ErrBodyTooLarge) {
		t.Fatalf("err = %v, want ErrBodyTooLarge", err)
	}
	out := logs.String()
	want := fmt.Sprintf(`"limit":%d,"n":2,"dims":1`, c.responseLimit(2))
	if !strings.Contains(out, "embed batch: response body exceeds limit") || !strings.Contains(out, want) {
		t.Fatalf("limit log event missing or incomplete (positive control); want %s", want)
	}
	assertNoUpstreamSentinel(t, "logs", out)
}

// TestRerankRequestDisablesReturnDocuments 는 요청에 return_documents:false
// 가 명시되는지 본다. 응답 상한은 본문이 없다는 전제로 잡았다.
func TestRerankRequestDisablesReturnDocuments(t *testing.T) {
	t.Parallel()
	var got map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"results":[{"index":0,"relevance_score":0.5}]}`))
	}))
	t.Cleanup(srv.Close)
	if _, err := NewHTTPReranker(srv.URL, "", "m", 0).Rerank(context.Background(), "q", []string{"a"}); err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if v, ok := got["return_documents"]; !ok || string(v) != "false" {
		t.Fatalf("return_documents = %q (present=%v), want explicit false", v, ok)
	}
}
