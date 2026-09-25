package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// #290 ingest/messages 응답 계약 단위 테스트(스텁 저장소). 실DB 판은
// ingest_messages_db_test.go.

// scriptedMessagesUpserter 는 호출 순번마다 결과를 정하는 스텁이다.
type scriptedMessagesUpserter struct {
	mu    sync.Mutex
	calls int
	docs  []model.Document
	// fn 이 nil 이면 (true, nil). ctx 는 핸들러의 예산 ctx 다.
	fn func(ctx context.Context, call int, doc *model.Document) (bool, error)
}

func (u *scriptedMessagesUpserter) UpsertTrackedWithChunks(ctx context.Context, doc *model.Document, buildChunks func(*model.Document) []store.Chunk) (bool, error) {
	u.mu.Lock()
	call := u.calls
	u.calls++
	u.docs = append(u.docs, *doc)
	u.mu.Unlock()
	if u.fn == nil {
		return true, nil
	}
	changed, err := u.fn(ctx, call, doc)
	if err == nil && changed && buildChunks != nil {
		buildChunks(doc)
	}
	return changed, err
}

func (u *scriptedMessagesUpserter) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

func smsRec(addr, body string, dateMs int64) map[string]any {
	return map[string]any{"address": addr, "body": body, "date_ms": dateMs, "type": 1}
}

func recentMs(i int) int64 {
	return time.Now().Add(-time.Hour).Add(time.Duration(i) * time.Second).UnixMilli()
}

func decodeMessagesResp(t *testing.T, rr *httptest.ResponseRecorder) IngestMessagesResponse {
	t.Helper()
	var resp IngestMessagesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rr.Body.String())
	}
	return resp
}

func postRawMessages(srv *Server, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/messages", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

// TestIngestMessages_TransientAbortsBatchWith503 는 k 번째 레코드의 일시 오류가
// 배치 전체 503 + Retry-After 가 되고, 그 뒤 레코드는 처리하지 않는지 본다.
// 수정 전 코드는 201 + errors[] 였다(앱은 커서를 전진해 레코드를 잃는다).
func TestIngestMessages_TransientAbortsBatchWith503(t *testing.T) {
	t.Parallel()

	transients := map[string]error{
		"57P01_admin_shutdown": &pgconn.PgError{Code: "57P01"},
		"42P01_missing_table":  &pgconn.PgError{Code: "42P01"},
		"53300_too_many_conns": &pgconn.PgError{Code: "53300"},
		"40P01_deadlock":       &pgconn.PgError{Code: "40P01"},
		"23505_chunk_race":     &pgconn.PgError{Code: "23505", ConstraintName: "chunks_document_id_chunk_index_key"},
		"unexpected_eof":       io.ErrUnexpectedEOF,
		"unknown":              errors.New("something odd"),
	}
	for name, injected := range transients {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			u := &scriptedMessagesUpserter{fn: func(_ context.Context, call int, _ *model.Document) (bool, error) {
				if call == 1 {
					return false, injected
				}
				return true, nil
			}}
			srv := newMessagesTestServer(u, 0, time.Time{})
			payload := map[string]any{"sms": []any{
				smsRec("a1", "first", recentMs(0)),
				smsRec("a1", "second", recentMs(1)),
				smsRec("a1", "third", recentMs(2)),
				smsRec("a1", "fourth", recentMs(3)),
			}, "calls": []any{
				map[string]any{"number": "n1", "date_ms": recentMs(4), "duration_sec": 3, "type": 1},
			}}
			rr := doMessagesPost(t, srv, payload, "Bearer test-key")
			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body = %s", rr.Code, rr.Body.String())
			}
			if got := rr.Header().Get("Retry-After"); got != "30" {
				t.Errorf("Retry-After = %q, want 30", got)
			}
			if !strings.Contains(rr.Body.String(), ingestUnavailableMsg) {
				t.Errorf("body = %s, want fixed %q", rr.Body.String(), ingestUnavailableMsg)
			}
			if got := u.callCount(); got != 2 {
				t.Errorf("upsert calls = %d, want 2 (records after the transient failure must not be attempted)", got)
			}
		})
	}
}

// TestIngestMessages_PermanentSkipsRecordWith201 는 SQLSTATE 22·54·23502·23514 오류가
// 그 레코드만 errors[] 에 넣고 나머지는 계속 처리되는지 본다.
func TestIngestMessages_PermanentSkipsRecordWith201(t *testing.T) {
	t.Parallel()

	for _, code := range []string{"23514", "22021", "54000"} {
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			u := &scriptedMessagesUpserter{fn: func(_ context.Context, call int, _ *model.Document) (bool, error) {
				if call == 1 {
					return false, &pgconn.PgError{Code: code, Message: "value zz-secret-body"}
				}
				return true, nil
			}}
			srv := newMessagesTestServer(u, 0, time.Time{})
			payload := map[string]any{"sms": []any{
				smsRec("a1", "first", recentMs(0)),
				smsRec("a1", "zz-secret-body", recentMs(1)),
				smsRec("a1", "third", recentMs(2)),
			}}
			rr := doMessagesPost(t, srv, payload, "Bearer test-key")
			if rr.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201; body = %s", rr.Code, rr.Body.String())
			}
			resp := decodeMessagesResp(t, rr)
			if resp.Accepted != 2 || len(resp.Errors) != 1 {
				t.Fatalf("accepted=%d errors=%v, want 2 and 1", resp.Accepted, resp.Errors)
			}
			if want := "sms[1]: rejected by database (sqlstate " + code + ")"; resp.Errors[0] != want {
				t.Errorf("error = %q, want %q", resp.Errors[0], want)
			}
			if strings.Contains(rr.Body.String(), "zz-secret-body") {
				t.Errorf("response leaks record content: %s", rr.Body.String())
			}
		})
	}
}

// TestIngestMessages_DuplicateTranscriptIsSkipped 는 통화 중복 전사가 errors[]
// 가 아니라 skipped 로 가고(수정 전: errors[]), 503 을 만들지 않는지 본다.
func TestIngestMessages_DuplicateTranscriptIsSkipped(t *testing.T) {
	t.Parallel()

	u := &scriptedMessagesUpserter{fn: func(_ context.Context, call int, _ *model.Document) (bool, error) {
		if call == 0 {
			return false, store.ErrDuplicateTranscript
		}
		return true, nil
	}}
	srv := newMessagesTestServer(u, 0, time.Time{})
	payload := map[string]any{"calls": []any{
		map[string]any{"number": "n1", "date_ms": recentMs(0), "duration_sec": 3, "type": 1},
		map[string]any{"number": "n2", "date_ms": recentMs(1), "duration_sec": 4, "type": 2},
	}}
	rr := doMessagesPost(t, srv, payload, "Bearer test-key")
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rr.Code, rr.Body.String())
	}
	resp := decodeMessagesResp(t, rr)
	if resp.Accepted != 1 || resp.Skipped != 1 || len(resp.Errors) != 0 {
		t.Errorf("accepted=%d skipped=%d errors=%v, want 1/1/[]", resp.Accepted, resp.Skipped, resp.Errors)
	}
}

// TestIngestMessages_BudgetExpiryIs503 는 예산 ctx 가 끝나면 오류 체인에
// context 오류가 없어도(pgx 가 연결을 버리며 돌려주는 모양) 503 인지 본다.
func TestIngestMessages_BudgetExpiryIs503(t *testing.T) {
	t.Parallel()

	u := &scriptedMessagesUpserter{fn: func(ctx context.Context, _ int, _ *model.Document) (bool, error) {
		// 예산이 걸려 있지 않으면 3초 뒤에 풀려난다(테스트가 멈추지 않게).
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
		return false, errors.New("conn closed") // context 오류를 잃은 체인
	}}
	srv := newMessagesTestServer(u, 0, time.Time{})
	srv.messagesBudget = 50 * time.Millisecond
	payload := map[string]any{"sms": []any{smsRec("a1", "x", recentMs(0)), smsRec("a1", "y", recentMs(1))}}
	start := time.Now()
	rr := doMessagesPost(t, srv, payload, "Bearer test-key")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rr.Code, rr.Body.String())
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("took %v, want about the 50ms budget (budget not applied)", el)
	}
	if got := u.callCount(); got != 1 {
		t.Errorf("upsert calls = %d, want 1", got)
	}
}

// TestIngestMessages_ClientGoneIs503 는 클라이언트 이탈(요청 ctx 취소)도 일시
// 오류로 처리하는지 본다 — 떠난 클라이언트를 위해 레코드를 계속 쓰지 않는다.
func TestIngestMessages_ClientGoneIs503(t *testing.T) {
	t.Parallel()

	reqCtx, cancel := context.WithCancel(context.Background())
	u := &scriptedMessagesUpserter{fn: func(ctx context.Context, call int, _ *model.Document) (bool, error) {
		if call == 0 {
			cancel()
			<-ctx.Done()
			return false, errors.New("conn closed")
		}
		return true, nil
	}}
	srv := newMessagesTestServer(u, 0, time.Time{})
	body, _ := json.Marshal(map[string]any{"sms": []any{smsRec("a1", "x", recentMs(0)), smsRec("a1", "y", recentMs(1))}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/messages", bytes.NewReader(body)).WithContext(reqCtx)
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	if got := u.callCount(); got != 1 {
		t.Errorf("upsert calls = %d, want 1", got)
	}
}

// errAfterReader 는 data 를 준 뒤 err 를 돌려준다(업로드가 잘린 모양).
type errAfterReader struct {
	data []byte
	err  error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// TestIngestMessages_BodyDecodeStatuses 는 본문 단계의 상태 코드를 고정한다:
// 상한 초과 413, 잘린 업로드 503, 깨끗하게 끝난 잘린 JSON·문법 오류·빈 본문·
// 최상위 구조 오류 400.
func TestIngestMessages_BodyDecodeStatuses(t *testing.T) {
	t.Parallel()

	big := `{"sms":[{"address":"a","body":"` + strings.Repeat("x", 4096) + `","date_ms":1,"type":1}]}`
	cases := []struct {
		name string
		body func() io.Reader
		want int
	}{
		{"over_limit_413", func() io.Reader { return strings.NewReader(big) }, http.StatusRequestEntityTooLarge},
		{"truncated_upload_503", func() io.Reader {
			return &errAfterReader{data: []byte(`{"sms":[{"address":"a",`), err: io.ErrUnexpectedEOF}
		}, http.StatusServiceUnavailable},
		{"truncated_json_clean_eof_400", func() io.Reader { return strings.NewReader(`{"sms":[{"address":"a",`) }, http.StatusBadRequest},
		{"syntax_error_400", func() io.Reader { return strings.NewReader(`{"sms": [}`) }, http.StatusBadRequest},
		{"empty_body_400", func() io.Reader { return strings.NewReader("") }, http.StatusBadRequest},
		{"top_level_type_error_400", func() io.Reader { return strings.NewReader(`{"sms":"nope"}`) }, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u := &scriptedMessagesUpserter{}
			srv := newMessagesTestServer(u, 0, time.Time{}).WithIngestMessagesMaxBody(1024)
			rr := postRawMessages(srv, tc.body())
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", rr.Code, tc.want, rr.Body.String())
			}
			if tc.want == http.StatusServiceUnavailable && rr.Header().Get("Retry-After") == "" {
				t.Error("503 without Retry-After")
			}
			if got := u.callCount(); got != 0 {
				t.Errorf("upsert calls = %d, want 0", got)
			}
		})
	}
}

// TestIngestMessages_InvalidUTF8KeepsRecord 는 본문 원 바이트의 잘못된 UTF-8 이
// 배치 400 이 아니라 U+FFFD 로 바뀌어 저장되는지 본다(decodeBoundedJSON 을 쓰면
// 배치 전체가 400 = poison).
func TestIngestMessages_InvalidUTF8KeepsRecord(t *testing.T) {
	t.Parallel()

	u := &scriptedMessagesUpserter{}
	srv := newMessagesTestServer(u, 0, time.Time{})
	// 해석 문자열이라 \xff\xfe 는 원 바이트(잘못된 UTF-8)다.
	raw := []byte("{\"sms\":[{\"address\":\"a1\",\"body\":\"bad \xff\xfe byte\",\"date_ms\":" +
		jsonInt(recentMs(0)) + ",\"type\":1}]}")
	if utf8.Valid(raw) {
		t.Fatal("test body must contain invalid UTF-8")
	}
	rr := postRawMessages(srv, bytes.NewReader(raw))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rr.Code, rr.Body.String())
	}
	if resp := decodeMessagesResp(t, rr); resp.Accepted != 1 {
		t.Fatalf("accepted = %d, want 1", resp.Accepted)
	}
	if got := u.docs[0].Content; !strings.Contains(got, "\uFFFD") {
		t.Errorf("content = %q, want U+FFFD replacement", got)
	}
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// TestIngestMessages_RecordLevelValidation 은 필드 상한·date 범위·원소 타입
// 오류가 그 레코드만 errors[] 로 보내고(201) 나머지는 저장되는지, 오류 문구에
// 입력 값이 없는지 본다(#288 1항).
func TestIngestMessages_RecordLevelValidation(t *testing.T) {
	t.Parallel()

	longBody := "zzbody" + strings.Repeat("b", ingestSMSBodyMaxBytes)
	longAddr := "zzaddr" + strings.Repeat("a", ingestIdentityFieldMaxBytes)
	longName := "zzname" + strings.Repeat("n", ingestIdentityFieldMaxBytes)
	longNum := "zznum" + strings.Repeat("9", ingestIdentityFieldMaxBytes)
	old := time.Date(1999, 12, 31, 0, 0, 0, 0, time.UTC).UnixMilli()
	future := time.Now().Add(30 * 24 * time.Hour).UnixMilli()

	u := &scriptedMessagesUpserter{}
	srv := newMessagesTestServer(u, 0, time.Time{})
	payload := map[string]any{
		"sms": []any{
			smsRec("ok-a", "fine", recentMs(0)),   // 0 ok
			smsRec("ok-a", longBody, recentMs(1)), // 1 body
			smsRec(longAddr, "fine", recentMs(2)), // 2 address
			map[string]any{"address": "ok-a", "body": "x", "date_ms": recentMs(3), "type": 1, "contact_name": longName}, // 3 name
			smsRec("ok-a", "fine", old),    // 4 date low
			smsRec("ok-a", "fine", future), // 5 date high
			map[string]any{"address": "ok-a", "body": "x", "date_ms": "zzdate", "type": 1}, // 6 type error
			map[string]any{"body": "x", "date_ms": recentMs(7), "type": 1},                 // 7 missing address
			map[string]any{"address": "ok-a", "body": "x", "type": 1},                      // 8 missing date
			nil, // 9 null element
			smsRec("ok-a", strings.Repeat("c", ingestSMSBodyMaxBytes), recentMs(10)), // 10 exactly at limit: ok
		},
		"calls": []any{
			map[string]any{"number": longNum, "date_ms": recentMs(11), "duration_sec": 1, "type": 1}, // 0 number
			map[string]any{"number": "ok-n", "date_ms": recentMs(12), "duration_sec": 1, "type": 1},  // 1 ok
		},
	}
	rr := doMessagesPost(t, srv, payload, "Bearer test-key")
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %.300s", rr.Code, rr.Body.String())
	}
	resp := decodeMessagesResp(t, rr)
	wantErrs := []string{
		"sms[1]: body exceeds 65536 bytes",
		"sms[2]: address exceeds 1024 bytes",
		"sms[3]: contact_name exceeds 1024 bytes",
		"sms[4]: date_ms out of range",
		"sms[5]: date_ms out of range",
		"sms[6]: invalid record",
		"sms[7]: missing address",
		"sms[8]: missing date_ms",
		"sms[9]: missing address",
		"call[0]: number exceeds 1024 bytes",
	}
	if strings.Join(resp.Errors, "|") != strings.Join(wantErrs, "|") {
		t.Errorf("errors =\n%v\nwant\n%v", resp.Errors, wantErrs)
	}
	if resp.Accepted != 3 || u.callCount() != 3 {
		t.Errorf("accepted=%d upsert calls=%d, want 3/3", resp.Accepted, u.callCount())
	}
	for _, leak := range []string{"zzbody", "zzaddr", "zzname", "zznum", "zzdate"} {
		if strings.Contains(rr.Body.String(), leak) {
			t.Errorf("response leaks input value %q", leak)
		}
	}
}

// TestIngestMessages_NULIsStrippedNotSkipped 는 결정 D1: NUL 을 지우고 저장하며
// sanitized 로 센다. 같은 입력은 같은 결과(멱등)여야 한다.
func TestIngestMessages_NULIsStrippedNotSkipped(t *testing.T) {
	t.Parallel()

	u := &scriptedMessagesUpserter{}
	srv := newMessagesTestServer(u, 0, time.Time{})
	payload := map[string]any{
		"sms": []any{
			smsRec("a1", "hello\x00world", recentMs(0)),
			map[string]any{"address": "a\x001", "body": "plain", "date_ms": recentMs(1), "type": 1, "contact_name": "Al\x00ice"},
			smsRec("a1", "clean", recentMs(2)),
		},
		"calls": []any{
			map[string]any{"number": "n\x001", "date_ms": recentMs(3), "duration_sec": 1, "type": 1},
		},
	}
	rr := doMessagesPost(t, srv, payload, "Bearer test-key")
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rr.Code, rr.Body.String())
	}
	resp := decodeMessagesResp(t, rr)
	if resp.Accepted != 4 || resp.Sanitized != 3 || len(resp.Errors) != 0 {
		t.Fatalf("accepted=%d sanitized=%d errors=%v, want 4/3/[]", resp.Accepted, resp.Sanitized, resp.Errors)
	}
	for i, d := range u.docs {
		if strings.ContainsRune(d.Content, 0) || strings.ContainsRune(d.Title, 0) {
			t.Errorf("doc %d still contains NUL", i)
		}
		b, _ := json.Marshal(d.Metadata)
		if strings.Contains(string(b), `\u0000`) {
			t.Errorf("doc %d metadata still contains NUL", i)
		}
	}
	if u.docs[0].Content != "helloworld" {
		t.Errorf("content = %q, want helloworld", u.docs[0].Content)
	}
	// 같은 주소에서 NUL 만 지운 값과 같은 source_id 여야 한다(결정적 변환).
	if want := smsmapSourceID(t, "a1", recentMsFrom(u.docs[1])); u.docs[1].SourceID != want {
		t.Errorf("source_id = %q, want %q (NUL-stripped address)", u.docs[1].SourceID, want)
	}
}

func recentMsFrom(d model.Document) int64 { return d.OccurredAt.UnixMilli() }

func smsmapSourceID(t *testing.T, addr string, dateMs int64) string {
	t.Helper()
	srv := newMessagesTestServer(&scriptedMessagesUpserter{}, 0, time.Time{})
	prepared, verr := srv.prepareSMSRecord(0, mustRawJSON(t, smsRec(addr, "x", dateMs)), newIngestMessagesResult())
	if verr != nil || prepared.doc == nil {
		t.Fatalf("prepare: %v", verr)
	}
	return prepared.doc.SourceID
}

func mustRawJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestIngestMessages_GateRejectsOverlap 는 앞 요청이 처리 중일 때 겹친 요청이
// 게이트 대기 뒤 503 을 받고(같은 배치를 두 번 처리하지 않음), 앞 요청이 끝나면
// 다시 받는지 본다.
func TestIngestMessages_GateRejectsOverlap(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	unblock := make(chan struct{})
	var once sync.Once
	u := &scriptedMessagesUpserter{fn: func(_ context.Context, call int, _ *model.Document) (bool, error) {
		if call == 0 {
			once.Do(func() { close(entered) })
			<-unblock
		}
		return true, nil
	}}
	srv := newMessagesTestServer(u, 0, time.Time{})
	srv.messagesGateWait = 50 * time.Millisecond
	payload := map[string]any{"sms": []any{smsRec("a1", "x", recentMs(0))}}

	first := make(chan int, 1)
	go func() {
		rr := doMessagesPost(t, srv, payload, "Bearer test-key")
		first <- rr.Code
	}()
	<-entered

	rr := doMessagesPost(t, srv, payload, "Bearer test-key")
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") == "" {
		t.Errorf("overlapping request: status=%d Retry-After=%q, want 503 with Retry-After", rr.Code, rr.Header().Get("Retry-After"))
	}
	if !strings.Contains(rr.Body.String(), ingestBusyMsg) {
		t.Errorf("overlapping body = %s, want %q", rr.Body.String(), ingestBusyMsg)
	}
	if got := u.callCount(); got != 1 {
		t.Errorf("upsert calls = %d, want 1 (the overlapping batch must not be processed)", got)
	}

	close(unblock)
	if code := <-first; code != http.StatusCreated {
		t.Errorf("first request status = %d, want 201", code)
	}
	if rr := doMessagesPost(t, srv, payload, "Bearer test-key"); rr.Code != http.StatusCreated {
		t.Errorf("request after release: status = %d, want 201", rr.Code)
	}
}

// TestIngestMessages_DefaultsWired 는 WithIngestMessages 가 예산·게이트·본문
// 상한을 채우고, config 기본값과 api 기본값이 같은지 고정한다.
func TestIngestMessages_DefaultsWired(t *testing.T) {
	t.Parallel()

	srv := newMessagesTestServer(&scriptedMessagesUpserter{}, 0, time.Time{})
	if srv.messagesBudget != IngestMessagesBudget || srv.messagesGate == nil ||
		srv.messagesGateWait != IngestMessagesGateWait || srv.messagesMaxBody != DefaultIngestMessagesMaxBodyBytes {
		t.Errorf("defaults not wired: budget=%v gate=%v wait=%v maxBody=%d",
			srv.messagesBudget, srv.messagesGate != nil, srv.messagesGateWait, srv.messagesMaxBody)
	}
	if DefaultIngestMessagesMaxBodyBytes != config.DefaultIngestMessagesMaxBodyBytes {
		t.Errorf("api default %d != config default %d", DefaultIngestMessagesMaxBodyBytes, config.DefaultIngestMessagesMaxBodyBytes)
	}
	// 0 이하는 무시.
	srv.WithIngestMessagesMaxBody(0)
	if srv.messagesMaxBody != DefaultIngestMessagesMaxBodyBytes {
		t.Errorf("WithIngestMessagesMaxBody(0) changed the limit to %d", srv.messagesMaxBody)
	}
	// WithIngestMessages 이전에 건 값은 유지된다.
	srv2 := NewServer(nil, nil, nil, nil, nil, "", "k").WithIngestMessagesMaxBody(4096).WithIngestMessages(&scriptedMessagesUpserter{}, 0, time.Time{})
	if srv2.messagesMaxBody != 4096 {
		t.Errorf("max body = %d, want 4096 when set before WithIngestMessages", srv2.messagesMaxBody)
	}
}

// TestIngestMessages_LogsHaveNoPII 는 영구·일시 오류 로그에 본문·주소·PgError
// 문구가 남지 않는지 본다. slog 기본 로거를 바꾸므로 병렬로 돌리지 않는다
// (병렬 테스트는 직렬 테스트가 모두 끝난 뒤 돈다).
func TestIngestMessages_LogsHaveNoPII(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&lockedWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	u := &scriptedMessagesUpserter{fn: func(_ context.Context, call int, _ *model.Document) (bool, error) {
		switch call {
		case 0:
			return false, &pgconn.PgError{Code: "22P02", Message: `invalid input syntax "zz-pgmsg-secret"`}
		case 1:
			return false, &pgconn.PgError{Code: "57P01", Message: "zz-pgmsg-secret2"}
		}
		return true, nil
	}}
	srv := newMessagesTestServer(u, 0, time.Time{})
	payload := map[string]any{"sms": []any{
		smsRec("zz-addr-secret", "zz-body-secret", recentMs(0)),
		smsRec("zz-addr-secret", "zz-body-secret2", recentMs(1)),
		smsRec("zz-addr-secret", "oversize "+strings.Repeat("q", ingestSMSBodyMaxBytes), recentMs(2)),
	}}
	rr := doMessagesPost(t, srv, payload, "Bearer test-key")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	mu.Lock()
	logs := buf.String()
	mu.Unlock()
	for _, leak := range []string{"zz-addr-secret", "zz-body-secret", "zz-pgmsg-secret"} {
		if strings.Contains(logs, leak) {
			t.Errorf("logs leak %q:\n%s", leak, logs)
		}
	}
	for _, want := range []string{`"sqlstate":"22P02"`, `"sqlstate":"57P01"`, `"record_index":1`} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %s:\n%s", want, logs)
		}
	}
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// TestIngestMessages_SanitizedCountsStoredOnly 는 sanitized 가 "NUL 을 지우고
// 실제로 저장한 레코드 수" 인지 고정한다(#290 후속). NUL 을 지웠더라도 검증에서
// 거부됐거나 DB 가 영구 거부한 레코드는 errors[] 에만 잡히고 sanitized 에는
// 들어가지 않는다.
func TestIngestMessages_SanitizedCountsStoredOnly(t *testing.T) {
	t.Parallel()

	u := &scriptedMessagesUpserter{fn: func(_ context.Context, _ int, doc *model.Document) (bool, error) {
		if strings.Contains(doc.Content, "dbreject") {
			return false, &pgconn.PgError{Code: "23514"}
		}
		return true, nil
	}}
	srv := newMessagesTestServer(u, 0, time.Time{})
	payload := map[string]any{"sms": []any{
		smsRec("a1", "stored\x00one", recentMs(0)),                       // 저장 + NUL → 셈
		map[string]any{"address": "a1", "body": "no\x00date", "type": 1}, // 검증 거부 + NUL → 안 셈
		smsRec("a1", "dbreject\x00two", recentMs(2)),                     // DB 영구 거부 + NUL → 안 셈
		smsRec("a1", "clean", recentMs(3)),                               // 저장, NUL 없음 → 안 셈
	}}
	rr := doMessagesPost(t, srv, payload, "Bearer test-key")
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rr.Code, rr.Body.String())
	}
	resp := decodeMessagesResp(t, rr)
	if resp.Accepted != 2 || resp.Sanitized != 1 || len(resp.Errors) != 2 {
		t.Errorf("accepted=%d sanitized=%d errors=%v, want 2/1/2", resp.Accepted, resp.Sanitized, resp.Errors)
	}
}

// captureSlog 는 기본 slog 로거를 JSON 버퍼로 바꾸고 원래대로 돌린다. 기본
// 로거를 바꾸므로 이것을 쓰는 테스트는 병렬로 돌리지 않는다.
func captureSlog(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&lockedWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

// TestIngestMessages_SkipReasonsLoggedAndDriftAlarm 는 건너뛴 사유가 사유
// 코드별 개수로 로그에 남는지, 그리고 저장된 레코드 없이 모두 거부되면
// (앱이 필드 이름을 바꾼 계약 드리프트 모양) ERROR 로 올라가는지 본다. 사유
// 코드에는 입력 값이 없어야 한다.
func TestIngestMessages_SkipReasonsLoggedAndDriftAlarm(t *testing.T) {
	t.Run("all_rejected_is_error", func(t *testing.T) {
		logs := captureSlog(t)
		srv := newMessagesTestServer(&scriptedMessagesUpserter{}, 0, time.Time{})
		// 앱이 date_ms 를 dateMs 로 바꿔 보낸 모양: 모든 레코드가 missing_date_ms.
		payload := map[string]any{"sms": []any{
			map[string]any{"address": "zz-addr-secret", "body": "zz-body-secret", "dateMs": recentMs(0), "type": 1},
			map[string]any{"address": "zz-addr-secret", "body": "zz-body-secret", "dateMs": recentMs(1), "type": 1},
			map[string]any{"address": "zz-addr-secret", "body": "zz-body-secret", "dateMs": recentMs(2), "type": 1},
		}}
		rr := doMessagesPost(t, srv, payload, "Bearer test-key")
		if rr.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", rr.Code)
		}
		out := logs()
		for _, want := range []string{`"level":"ERROR"`, "contract drift", `"error_reasons":{"missing_date_ms":3}`} {
			if !strings.Contains(out, want) {
				t.Errorf("logs missing %s:\n%s", want, out)
			}
		}
		for _, leak := range []string{"zz-addr-secret", "zz-body-secret"} {
			if strings.Contains(out, leak) {
				t.Errorf("logs leak %q", leak)
			}
		}
	})
	t.Run("partial_is_warn_with_reasons", func(t *testing.T) {
		logs := captureSlog(t)
		u := &scriptedMessagesUpserter{fn: func(_ context.Context, call int, _ *model.Document) (bool, error) {
			if call == 1 {
				return false, &pgconn.PgError{Code: "23514"}
			}
			return true, nil
		}}
		srv := newMessagesTestServer(u, 0, time.Now().Add(-90*time.Minute))
		payload := map[string]any{
			"sms": []any{
				smsRec("a1", "ok", recentMs(0)),
				smsRec("a1", "db", recentMs(1)),
				map[string]any{"body": "x", "date_ms": recentMs(2), "type": 1},
				smsRec("a1", "old", time.Now().Add(-3*time.Hour).UnixMilli()),
			},
			"calls": []any{map[string]any{"number": "n1", "duration_sec": 1, "type": 1}},
		}
		rr := doMessagesPost(t, srv, payload, "Bearer test-key")
		if rr.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", rr.Code)
		}
		out := logs()
		if strings.Contains(out, `"level":"ERROR"`) {
			t.Errorf("partial rejection must not raise the drift ERROR:\n%s", out)
		}
		for _, want := range []string{
			`"level":"WARN"`,
			`"db_sqlstate_23514":1`, `"missing_address":1`, `"missing_date_ms":1`,
			`"skip_reasons":{"before_cutover":1}`,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("logs missing %s:\n%s", want, out)
			}
		}
	})
}
