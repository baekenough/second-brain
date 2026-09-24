package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/note"
)

// fillReader 는 무한히 'a' 를 내주는 리더다. 큰 본문을 메모리에 미리 만들지
// 않고 흘려보내려고 쓴다.
type fillReader struct{}

func (fillReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

// TestMCPHTTPHandler_BodyCap 은 main 이 쓰는 newMCPHTTPHandler 가 본문을
// mcpRequestMaxBytes 로 묶는지 본다(#282 보안 리뷰 후속). 안쪽 핸들러는
// mcp-go 처럼 io.ReadAll 로 본문 전체를 읽는다.
func TestMCPHTTPHandler_BodyCap(t *testing.T) {
	t.Parallel()

	var (
		readErr error
		readLen int
	)
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		readErr, readLen = err, len(b)
	})
	h := newMCPHTTPHandler(inner)

	// 대조군: 작은 본문은 그대로 읽힌다.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0"}`)))
	if readErr != nil {
		t.Fatalf("control: small body read failed: %v", readErr)
	}

	over := io.LimitReader(fillReader{}, mcpRequestMaxBytes+1)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/mcp", over))
	var mbe *http.MaxBytesError
	if !errors.As(readErr, &mbe) {
		t.Fatalf("read error = %v, want *http.MaxBytesError for a body over the cap", readErr)
	}
	if readLen > mcpRequestMaxBytes {
		t.Errorf("handler read %d bytes, more than the cap %d", readLen, mcpRequestMaxBytes)
	}
}

// TestMCPRequestMaxBytes_FitsLargestNote 는 상한이 add_note 의 정상 최대
// 본문(note.MaxContentBytes)을 비 ASCII 이스케이프(최악 3배)까지 담을 수
// 있는지 고정한다. 상한을 낮추다 add_note 를 깨뜨리는 회귀를 막는다.
func TestMCPRequestMaxBytes_FitsLargestNote(t *testing.T) {
	t.Parallel()

	if mcpRequestMaxBytes < 3*note.MaxContentBytes {
		t.Errorf("mcpRequestMaxBytes = %d, want >= 3*note.MaxContentBytes (%d)", mcpRequestMaxBytes, 3*note.MaxContentBytes)
	}
}
