package api

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// #288 2항: POST /api/v1/actions/{key}/status 본문 상한(8 KiB)과 note 입력
// 검증. 모든 경우 로그에 note 센티널이 없어야 한다. 기본 slog 로거를
// 바꾸므로(captureSlog) t.Parallel 을 쓰지 않는다.

const actionsSentinelNote = "SENTINEL-NOTE-7F3A"

const actionsTestKey = "action:0123456789abcdef"

// escapedAstralNote 는 4바이트 문자(U+1F600)를 n 개 담은 note 를 JSON \u
// 이스케이프(서로게이트 쌍, 문자당 12B)로 적는다. 파이썬 json.dumps 기본값이
// 만드는 모양이다.
func escapedAstralNote(n int) string {
	return strings.Repeat(`😀`, n)
}

// padBody 는 JSON 끝에 공백을 붙여 본문을 정확히 size 바이트로 만든다(JSON
// 은 값 뒤 공백을 허용한다).
func padBody(t *testing.T, body string, size int) string {
	t.Helper()
	if len(body) > size {
		t.Fatalf("body is already %d bytes, cannot pad to %d", len(body), size)
	}
	return body + strings.Repeat(" ", size-len(body))
}

func postActionState(srv *Server, body []byte) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/actions/"+actionsTestKey+"/status", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// assertAccessLogged 는 요청이 접근 로그(requestLogger 의 "http" 이벤트)에
// 기대한 상태 코드로 찍혔는지 본다. 센티널 부재 검사의 양성 대조다 — 로그가
// 아예 안 찍혔으면 "센티널 없음"은 아무것도 증명하지 않는다.
func assertAccessLogged(t *testing.T, logs string, status int) {
	t.Helper()
	if !strings.Contains(logs, `"msg":"http"`) || !strings.Contains(logs, `"status":`+strconv.Itoa(status)) {
		t.Fatalf("access log event for status %d missing (positive control); logs=%d bytes", status, len(logs))
	}
}

func assertNoNoteSentinel(t *testing.T, sink, v string) {
	t.Helper()
	for _, s := range []string{actionsSentinelNote, "7F3A"} {
		if strings.Contains(v, s) {
			t.Errorf("%s leaks note sentinel %q", sink, s)
		}
	}
}

// TestActionsCaptureControl_SeesSentinel 은 캡처 장치 대조군이다.
func TestActionsCaptureControl_SeesSentinel(t *testing.T) {
	logs := captureSlog(t)
	slog.Info("capture control", "note", actionsSentinelNote)
	if !strings.Contains(logs(), actionsSentinelNote) {
		t.Fatal("capture did not see the sentinel; negative checks would be meaningless")
	}
}

// TestSetActionState_BodyLimitAndNoteValidation 은 본문 상한·note 검증의
// 경계를 본다.
func TestSetActionState_BodyLimitAndNoteValidation(t *testing.T) {
	// 센티널 18룬 + 이스케이프한 4바이트 문자 482룬 = 500룬. 서버가 자르지
	// 않고 받아들이는 가장 큰 note 에 가까운 모양이다(약 5.8KB). 공백을 붙여
	// 본문을 정확히 상한(8192B)으로 맞춘다.
	bigNote := actionsSentinelNote + escapedAstralNote(500-len(actionsSentinelNote))
	bigBody := `{"state":"done","note":"` + bigNote + `"}`

	cases := []struct {
		name       string
		body       func(t *testing.T) []byte
		wantStatus int
		wantStored bool
		check      func(t *testing.T, setter *stubActionSetter)
	}{
		{
			name:       "exactly 8192 bytes with escaped 500-rune note",
			body:       func(t *testing.T) []byte { return []byte(padBody(t, bigBody, actionStateRequestMaxBytes)) },
			wantStatus: http.StatusOK,
			wantStored: true,
			check: func(t *testing.T, setter *stubActionSetter) {
				if n := utf8.RuneCountInString(setter.gotNote); n != 500 {
					t.Errorf("stored note has %d runes, want 500", n)
				}
			},
		},
		{
			name:       "8193 bytes rejected with 413",
			body:       func(t *testing.T) []byte { return []byte(padBody(t, bigBody, actionStateRequestMaxBytes+1)) },
			wantStatus: http.StatusRequestEntityTooLarge,
		},
		{
			name:       "escaped NUL rejected with 400",
			body:       func(*testing.T) []byte { return []byte(`{"state":"done","note":"` + actionsSentinelNote + `\u0000x"}`) },
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "raw invalid UTF-8 byte rejected with 400",
			body: func(*testing.T) []byte {
				return append([]byte(`{"state":"done","note":"`+actionsSentinelNote), 0xff, '"', '}')
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "600-rune note accepted and truncated to 500",
			body: func(*testing.T) []byte {
				return []byte(`{"state":"done","note":"` + actionsSentinelNote + strings.Repeat("가", 600) + `"}`)
			},
			wantStatus: http.StatusOK,
			wantStored: true,
			check: func(t *testing.T, setter *stubActionSetter) {
				if n := utf8.RuneCountInString(setter.gotNote); n != 500 {
					t.Errorf("stored note has %d runes, want 500", n)
				}
				if !utf8.ValidString(setter.gotNote) {
					t.Error("stored note is not valid UTF-8")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureSlog(t)
			setter := &stubActionSetter{found: true}
			srv := newActionsTestServer(&stubActionLister{}, setter)
			body := tc.body(t)
			if !bytes.Contains(body, []byte(actionsSentinelNote)) {
				t.Fatal("positive control: request body must carry the note sentinel")
			}

			rec := postActionState(srv, body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if stored := setter.calls > 0; stored != tc.wantStored {
				t.Fatalf("store called=%v, want %v", stored, tc.wantStored)
			}
			if tc.check != nil {
				tc.check(t, setter)
			}
			assertNoNoteSentinel(t, "response", rec.Body.String())
			out := logs()
			assertAccessLogged(t, out, tc.wantStatus)
			assertNoNoteSentinel(t, "logs", out)
		})
	}
}

// TestSetActionState_StoreErrorDoesNotLogNote 는 저장 실패 경로에서도 note 가
// 로그에 남지 않는지 본다. 실패 이벤트가 찍혔는지 먼저 확인한다.
func TestSetActionState_StoreErrorDoesNotLogNote(t *testing.T) {
	logs := captureSlog(t)
	setter := &stubActionSetter{err: errors.New("dummy store failure")}
	srv := newActionsTestServer(&stubActionLister{}, setter)

	rec := postActionState(srv, []byte(`{"state":"done","note":"`+actionsSentinelNote+`"}`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if setter.gotNote != actionsSentinelNote {
		t.Fatalf("positive control: the note must have reached the store, got %q", setter.gotNote)
	}
	out := logs()
	if !strings.Contains(out, "actions: set state failed") {
		t.Fatal("store failure event missing (positive control)")
	}
	assertNoNoteSentinel(t, "logs", out)
	assertNoNoteSentinel(t, "response", rec.Body.String())
}
