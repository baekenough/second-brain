package llm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/baekenough/second-brain/internal/httperr"
	"github.com/baekenough/second-brain/internal/llm"
)

// #288 3항: LLM 클라이언트의 응답 읽기에 상한이 있고, 오류 본문은 읽지 않고
// 상한 안에서 버려 연결을 다시 쓰며, 재시도 판정은 전과 같이 상태 코드로만
// 이뤄지는지 본다.

const llmSentinel = "SENTINEL-본문-ZX9Q 01000009999"

type llmLockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *llmLockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *llmLockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// TestLLMRetryDecisionIsStatusBased 는 상태별 호출 횟수가 변경 전과 같은지
// 본다: 4xx(429 포함)는 1회, 5xx 는 1 + 재시도 2회. 5xx 에서는 재시도 로그
// 이벤트가 찍혔는지 먼저 확인(양성 대조)한 뒤 센티널 부재를 본다.
//
// 기본 slog 로거를 바꾸므로 t.Parallel 을 쓰지 않는다.
func TestLLMRetryDecisionIsStatusBased(t *testing.T) {
	cases := []struct {
		status    int
		wantCalls int32
	}{
		{http.StatusBadRequest, 1},
		{http.StatusUnauthorized, 1},
		{http.StatusTooManyRequests, 1},
		{http.StatusInternalServerError, 3},
		{http.StatusServiceUnavailable, 3},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			logs := &llmLockedBuffer{}
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": llmSentinel}})
			}))
			t.Cleanup(srv.Close)

			_, err := newClient(t, srv.URL, "test-key").Complete(context.Background(), "system", "user")
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := calls.Load(); got != tc.wantCalls {
				t.Errorf("calls = %d, want %d", got, tc.wantCalls)
			}
			out := logs.String()
			if tc.wantCalls > 1 && !strings.Contains(out, "llm: request failed, will retry") {
				t.Fatalf("retry log event missing (positive control)")
			}
			for sink, v := range map[string]string{"error": err.Error(), "logs": out} {
				for _, s := range []string{"SENTINEL", "ZX9Q", "01000009999", "00009999"} {
					if strings.Contains(v, s) {
						t.Errorf("%s leaks %q", sink, s)
					}
				}
			}
		})
	}
}

// TestLLMSuccessBodyLimit 는 성공 응답 본문이 16MiB 를 넘으면
// ErrBodyTooLarge 로 끝나고 재시도하지 않는지 본다.
func TestLLMSuccessBodyLimit(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	oversized := `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"pad":"` +
		strings.Repeat("p", 16<<20) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(oversized))
	}))
	t.Cleanup(srv.Close)

	_, err := newClient(t, srv.URL, "test-key").Complete(context.Background(), "system", "user")
	if !errors.Is(err, httperr.ErrBodyTooLarge) {
		t.Fatalf("err = %v, want ErrBodyTooLarge", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 (an oversized body is not retried)", calls.Load())
	}
}

// TestLLMErrorBodyDrainedForReuse 는 4xx 오류 본문을 상한 안에서 버려
// keep-alive 연결을 다시 쓰는지 본다(비스트리밍·스트리밍 모두). 전용
// Transport 로 다른 테스트와 연결 풀을 공유하지 않는다.
func TestLLMErrorBodyDrainedForReuse(t *testing.T) {
	body := `{"error":{"message":"` + strings.Repeat("x", 200<<10) + `"}}`
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", streaming), func(t *testing.T) {
			var newConns atomic.Int32
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

			tr := &http.Transport{}
			t.Cleanup(tr.CloseIdleConnections)
			client := llm.New(llm.Config{BaseURL: srv.URL, Model: "gpt-test", APIKey: "test-key", MaxTokens: 16}, &http.Client{Transport: tr})
			for i := 0; i < 2; i++ {
				var err error
				if streaming {
					err = client.StreamWithMessages(context.Background(), "system", []llm.Message{{Role: "user", Content: "u"}}, func(string) {})
				} else {
					_, err = client.Complete(context.Background(), "system", "user")
				}
				if err == nil {
					t.Fatal("expected an error for 400")
				}
			}
			if got := newConns.Load(); got != 1 {
				t.Errorf("new connections = %d, want 1 (the drained connection is reused)", got)
			}
		})
	}
}
