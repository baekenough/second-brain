package httperr

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// sentinelBody 는 외부 API 가 되돌려 주는 본문 조각(질의·문서)을 흉내 낸다.
// 대문자·한글이 섞여 있어 errorFieldRe 를 통과할 수 없다.
const sentinelBody = "SENTINEL-본문-ZX9Q"

// sentinelNum 은 할당되지 않은 010-0000 대역의 가짜 번호다.
const sentinelNum = "01000009999"

func newResp(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
}

func TestStatusError_Format(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  StatusError
		want string
	}{
		{"no fields", StatusError{Prefix: "rerank API status", StatusCode: 502}, "rerank API status 502"},
		{"type and code", StatusError{Prefix: "embed API status", StatusCode: 429, Type: "requests", Code: "rate_limit_exceeded"},
			"embed API status 429 (error.type=requests, code=rate_limit_exceeded)"},
		{"type only", StatusError{Prefix: "embed batch API status", StatusCode: 400, Type: "invalid_request_error"},
			"embed batch API status 400 (error.type=invalid_request_error)"},
		{"code only", StatusError{Prefix: "embed API status", StatusCode: 401, Code: "invalid_api_key"},
			"embed API status 401 (code=invalid_api_key)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Fatalf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestReadStatusError_ExtractsOnlySafeFields 는 본문에서 type/code 만 뽑고
// message·reason(요청 조각)과 모양이 안전하지 않은 값은 버리는지 본다.
// 각 경우 양성 대조로 "센티널이 본문에는 있었다"를 먼저 확인한다 — 본문에
// 없던 센티널이 오류에 없는 것은 아무것도 증명하지 않는다.
func TestReadStatusError_ExtractsOnlySafeFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		status   int
		body     string
		wantType string
		wantCode string
	}{
		{"openai shape", 400,
			`{"error":{"message":"bad input ` + sentinelBody + ` ` + sentinelNum + `","type":"invalid_request_error","code":"context_length_exceeded"}}`,
			"invalid_request_error", "context_length_exceeded"},
		{"opensearch shape", 400,
			`{"error":{"type":"query_shard_exception","reason":"` + sentinelBody + `"}}`,
			"query_shard_exception", ""},
		{"numeric code dropped", 429,
			`{"error":{"message":"` + sentinelBody + `","type":"requests","code":429}}`,
			"requests", ""},
		{"null code dropped", 500,
			`{"error":{"message":"` + sentinelBody + `","type":"server_error","code":null}}`,
			"server_error", ""},
		{"sentinel in type rejected", 400,
			`{"error":{"type":"` + sentinelBody + `","code":"x"}}`,
			"", "x"},
		{"digits in type rejected", 400,
			`{"error":{"type":"` + sentinelNum + `","code":"n` + sentinelNum + `"}}`,
			"", ""},
		{"string error (ollama shape)", 500,
			`{"error":"model failed on ` + sentinelBody + `"}`,
			"", ""},
		{"not json", 502, "<html>" + sentinelBody + "</html>", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.body, sentinelBody) && !strings.Contains(tc.body, sentinelNum) {
				t.Fatal("test body must carry a sentinel (positive control)")
			}
			se := ReadStatusError("embed API status", newResp(tc.status, tc.body))
			if se.StatusCode != tc.status || se.Type != tc.wantType || se.Code != tc.wantCode {
				t.Fatalf("got status=%d type=%q code=%q, want %d %q %q",
					se.StatusCode, se.Type, se.Code, tc.status, tc.wantType, tc.wantCode)
			}
			msg := se.Error()
			if !strings.HasPrefix(msg, "embed API status ") {
				t.Fatalf("Error() = %q, want the caller's prefix kept", msg)
			}
			for _, s := range []string{sentinelBody, sentinelNum, "00009999", "ZX9Q"} {
				if strings.Contains(msg, s) {
					t.Errorf("Error() = %q leaks %q", msg, s)
				}
			}
		})
	}
}

// countingReader 는 읽어 간 바이트 수를 센다(드레인 상한 확인용).
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// infiniteReader 는 끝나지 않는 본문을 흉내 낸다.
type infiniteReader struct{}

func (infiniteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

// TestReadStatusError_BoundedRead 는 끝나지 않는 오류 본문에서도 읽기 64KB +
// 드레인 1MB 에서 멈추는지 본다(무한 드레인 금지).
func TestReadStatusError_BoundedRead(t *testing.T) {
	t.Parallel()
	cr := &countingReader{r: infiniteReader{}}
	resp := &http.Response{StatusCode: 500, Body: io.NopCloser(cr)}
	se := ReadStatusError("embed API status", resp)
	if se.StatusCode != 500 {
		t.Fatalf("status = %d", se.StatusCode)
	}
	want := int64(MaxErrorBodyBytes + MaxDrainBytes)
	if cr.n != want {
		t.Fatalf("read %d bytes, want exactly %d (read limit + drain limit)", cr.n, want)
	}
}

// TestDrain_ReadsSmallBodyToEOF 는 작은 본문은 끝까지 읽어 연결 재사용이
// 가능한 상태로 두는지 본다.
func TestDrain_ReadsSmallBodyToEOF(t *testing.T) {
	t.Parallel()
	r := strings.NewReader(strings.Repeat("y", 4096))
	Drain(r)
	if r.Len() != 0 {
		t.Fatalf("%d bytes left unread", r.Len())
	}
}

func TestReadBody_Limit(t *testing.T) {
	t.Parallel()
	const max = 1024
	t.Run("exactly max", func(t *testing.T) {
		b, err := ReadBody(strings.NewReader(strings.Repeat("a", max)), max)
		if err != nil || len(b) != max {
			t.Fatalf("len=%d err=%v, want %d nil", len(b), err, max)
		}
	})
	t.Run("max plus one", func(t *testing.T) {
		body := sentinelBody + strings.Repeat("a", max)
		b, err := ReadBody(strings.NewReader(body), max)
		if !errors.Is(err, ErrBodyTooLarge) || b != nil {
			t.Fatalf("len=%d err=%v, want nil + ErrBodyTooLarge", len(b), err)
		}
		if strings.Contains(err.Error(), sentinelBody) {
			t.Fatalf("error %q leaks the body", err)
		}
	})
	t.Run("stops reading at max plus one", func(t *testing.T) {
		cr := &countingReader{r: infiniteReader{}}
		if _, err := ReadBody(cr, max); !errors.Is(err, ErrBodyTooLarge) {
			t.Fatalf("err = %v", err)
		}
		if cr.n != max+1 {
			t.Fatalf("read %d bytes from an endless body, want exactly max+1=%d", cr.n, max+1)
		}
	})
	t.Run("non-positive max rejects", func(t *testing.T) {
		if _, err := ReadBody(strings.NewReader("a"), 0); !errors.Is(err, ErrBodyTooLarge) {
			t.Fatalf("err = %v, want ErrBodyTooLarge for max=0", err)
		}
	})
}

// TestStatusError_ErrorsAs 는 호출자가 문자열 대신 타입으로 상태 코드를
// 판정할 수 있는지 본다(감싼 오류 포함).
func TestStatusError_ErrorsAs(t *testing.T) {
	t.Parallel()
	var err error = ReadStatusError("embed API status", newResp(503, "{}"))
	wrapped := errors.Join(errors.New("embed: all retries exhausted"), err)
	var se *StatusError
	if !errors.As(wrapped, &se) || se.StatusCode != 503 {
		t.Fatalf("errors.As failed: %v", wrapped)
	}
}
