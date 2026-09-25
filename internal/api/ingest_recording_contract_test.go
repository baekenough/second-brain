package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/baekenough/second-brain/internal/collector/smsmap"
	"github.com/baekenough/second-brain/internal/store"
)

// 녹음 경로 응답 계약(#292) 단위 테스트. 앱(Uploader.handleRecordingResponse)은
// 2xx(accepted 또는 skipped)와 401·403 이 아닌 4xx 에서 그 파일을 '전송 완료'로
// 표시하고 다시 보내지 않는다. 5xx 에서는 표시하지 않고 이번 실행의 녹음 루프를
// 멈춘 뒤 다음 실행에 같은 파일부터 다시 보낸다. 그래서 일시 오류는 503, 다시
// 보내도 같은 결과(중복·형식 오류)는 2xx·4xx 여야 한다.

// recordingPostRaw 는 임의의 본문 리더로 녹음 요청을 보낸다.
func recordingPostRaw(t *testing.T, srv *Server, body io.Reader, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/recording", body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

// failingReader 는 data 를 다 준 뒤 err 를 돌려준다(전송 계층 오류 흉내).
type failingReader struct {
	data *bytes.Reader
	err  error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.data.Len() > 0 {
		return f.data.Read(p)
	}
	return 0, f.err
}

func assert503(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Retry-After"); got != ingestMessagesRetryAfter {
		t.Errorf("Retry-After = %q, want %q", got, ingestMessagesRetryAfter)
	}
}

// TestIngestRecording_UpsertErrorContract: 문서 저장 오류의 분류.
func TestIngestRecording_UpsertErrorContract(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{"duplicate transcript", store.ErrDuplicateTranscript, http.StatusOK},
		{"wrapped duplicate", fmt.Errorf("upsert: %w", store.ErrDuplicateTranscript), http.StatusOK},
		{"connection failure", &pgconn.PgError{Code: "08006"}, http.StatusServiceUnavailable},
		{"unknown error", errors.New("boom"), http.StatusServiceUnavailable},
		{"context canceled", context.Canceled, http.StatusServiceUnavailable},
		{"invalid input", &pgconn.PgError{Code: "22021"}, http.StatusUnprocessableEntity},
		{"check violation", &pgconn.PgError{Code: "23514"}, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, dir := newRecordingTestServer(t, &stubIngestUpserter{err: tc.err}, "", 0, time.Time{})
			body, ct := buildRecordingForm(t, "rec.m4a", validM4ABytes(64), "010-0000-0001",
				time.Now().Add(-time.Hour).UnixMilli())
			rr := doRecordingPost(t, srv, body, ct, "Bearer test-key")
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			// 어떤 경우든 오디오·사이드카는 문서 저장 전에 디스크에 있다.
			if n := len(audioFilesInDir(t, dir)); n != 1 {
				t.Errorf("audio files = %d, want 1", n)
			}
			switch tc.wantStatus {
			case http.StatusOK:
				var resp IngestRecordingResponse
				if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
					t.Fatal(err)
				}
				// 앱은 accepted 또는 skipped 가 참이면 전송 완료로 표시한다.
				if !resp.Accepted || !resp.Skipped || resp.Reason != recordingSkipReasonDuplicate || resp.DocumentID != "" {
					t.Errorf("response = %+v, want accepted+skipped reason=%s", resp, recordingSkipReasonDuplicate)
				}
			case http.StatusServiceUnavailable:
				assert503(t, rr)
			case http.StatusUnprocessableEntity:
				if strings.Contains(rr.Body.String(), "010") {
					t.Errorf("422 body leaks input: %s", rr.Body.String())
				}
			}
		})
	}
}

// TestIngestRecording_BodyReadErrorsAre503: 업로드가 도중에 끊기거나 읽기
// 기한이 지나면 503 이다. 수정 전에는 400 → 앱이 전송 완료로 표시해 영구 유실.
func TestIngestRecording_BodyReadErrorsAre503(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"truncated upload", io.ErrUnexpectedEOF},
		{"read deadline", os.ErrDeadlineExceeded},
		{"connection reset", errors.New("read tcp: connection reset by peer")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			upserter := &stubIngestUpserter{}
			srv, dir := newRecordingTestServer(t, upserter, "", 0, time.Time{})
			full, ct := buildRecordingForm(t, "rec.m4a", validM4ABytes(4096), "010-0000-0001",
				time.Now().Add(-time.Hour).UnixMilli())
			half := full.Bytes()[:full.Len()/2]
			rr := recordingPostRaw(t, srv, &failingReader{data: bytes.NewReader(half), err: tc.err}, ct)
			assert503(t, rr)
			if n := len(audioFilesInDir(t, dir)); n != 0 || len(upserter.upserted) != 0 {
				t.Errorf("files=%d upserts=%d after failed upload, want 0/0", n, len(upserter.upserted))
			}
		})
	}
}

// TestIngestRecording_MalformedFormIs400: 클라이언트가 보낸 바이트 자체가
// 틀린 경우(다시 보내도 같다)는 400 을 유지한다 — 5xx 면 poison 이 된다.
func TestIngestRecording_MalformedFormIs400(t *testing.T) {
	t.Parallel()

	full, ct := buildRecordingForm(t, "rec.m4a", validM4ABytes(256), "010-0000-0001",
		time.Now().Add(-time.Hour).UnixMilli())
	cleanCut := full.Bytes()[:full.Len()-20] // 본문은 온전히 받았지만 multipart 가 안 끝남
	for _, tc := range []struct {
		name, contentType string
		body              []byte
	}{
		{"not multipart", "text/plain", []byte("hello")},
		{"missing boundary", "multipart/form-data", full.Bytes()},
		{"multipart ends early (clean EOF)", ct, cleanCut},
		{"empty body", ct, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, _ := newRecordingTestServer(t, &stubIngestUpserter{}, "", 0, time.Time{})
			rr := recordingPostRaw(t, srv, bytes.NewReader(tc.body), tc.contentType)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestIngestRecording_MultipartTempFileFailureIs503: 32 MiB 를 넘는 파트는
// 임시 디렉터리에 내려 쓴다. 그 쓰기가 실패하면(디스크 가득·경로 없음) 디스크
// 오류이므로 503 이다. t.Setenv 를 쓰므로 병렬로 돌리지 않는다.
func TestIngestRecording_MultipartTempFileFailureIs503(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing-tmp"))
	srv, dir := newRecordingTestServer(t, &stubIngestUpserter{}, "", 64<<20, time.Time{})
	body, ct := buildRecordingForm(t, "rec.m4a", validM4ABytes(33<<20), "010-0000-0001",
		time.Now().Add(-time.Hour).UnixMilli())
	rr := doRecordingPost(t, srv, body, ct, "Bearer test-key")
	assert503(t, rr)
	if n := len(audioFilesInDir(t, dir)); n != 0 {
		t.Errorf("audio files = %d, want 0", n)
	}
}

// recordingAudioName 은 해싱을 켠 테스트 서버가 kind=call 녹음에 붙이는 저장
// 파일 이름이다(ingestRecordingHandler 와 같은 공식).
func recordingAudioName(number string, dateMs int64) string {
	ts := time.UnixMilli(dateMs).UTC().In(time.Local).Format("20060102150405")
	return smsmap.ShortHash(number) + "_" + ts + ".m4a"
}

// TestIngestRecording_DiskErrorsAre503: 저장 디렉터리·사이드카·오디오 쓰기
// 실패는 모두 503 이다(수정 전: 디렉터리·오디오는 500, 사이드카는 Warn 뒤 201).
// 사이드카를 오디오보다 먼저 쓰므로, 사이드카가 실패하면 오디오도 없어야
// 한다 — WhisperCollector 가 사이드카 없는 오디오를 병합 없이 전사해 원장에
// 올리는 일을 막는다.
func TestIngestRecording_DiskErrorsAre503(t *testing.T) {
	t.Parallel()
	const number = "010-0000-0001"
	dateMs := time.Now().Add(-time.Hour).UnixMilli()

	t.Run("recording dir is a file", func(t *testing.T) {
		t.Parallel()
		notDir := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		upserter := &stubIngestUpserter{}
		srv, _ := newRecordingTestServer(t, upserter, filepath.Join(notDir, "rec"), 0, time.Time{})
		body, ct := buildRecordingForm(t, "rec.m4a", validM4ABytes(64), number, dateMs)
		assert503(t, doRecordingPost(t, srv, body, ct, "Bearer test-key"))
		if len(upserter.upserted) != 0 {
			t.Errorf("upserts = %d, want 0", len(upserter.upserted))
		}
	})

	t.Run("sidecar write fails", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
		upserter := &stubIngestUpserter{}
		srv, _ := newRecordingTestServer(t, upserter, dir, 0, time.Time{})
		body, ct := buildRecordingForm(t, "rec.m4a", validM4ABytes(64), number, dateMs)
		assert503(t, doRecordingPost(t, srv, body, ct, "Bearer test-key"))
		entries, _ := os.ReadDir(dir)
		if len(entries) != 0 || len(upserter.upserted) != 0 {
			t.Errorf("entries=%d upserts=%d, want 0/0 (no audio without its sidecar)", len(entries), len(upserter.upserted))
		}
	})

	t.Run("sidecar path blocked, audio would succeed", func(t *testing.T) {
		t.Parallel()
		// 사이드카만 실패하게 한다(저장 경로에 디렉터리). 오디오 쓰기는 성공할
		// 수 있는 상태라서, 사이드카 실패를 무시하고 진행하면 201 이 난다.
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, recordingAudioName(number, dateMs)+".meta.json"), 0o755); err != nil {
			t.Fatal(err)
		}
		upserter := &stubIngestUpserter{}
		srv, _ := newRecordingTestServer(t, upserter, dir, 0, time.Time{})
		body, ct := buildRecordingForm(t, "rec.m4a", validM4ABytes(64), number, dateMs)
		assert503(t, doRecordingPost(t, srv, body, ct, "Bearer test-key"))
		if len(upserter.upserted) != 0 {
			t.Errorf("upserts = %d, want 0", len(upserter.upserted))
		}
		if _, err := os.Stat(filepath.Join(dir, recordingAudioName(number, dateMs))); !os.IsNotExist(err) {
			t.Errorf("audio written although its sidecar failed (stat err=%v)", err)
		}
	})

	t.Run("re-upload replaces a read-only audio file", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root ignores file permissions")
		}
		// 임시 파일에 쓰고 이름을 바꾸므로(writeFileAtomic) 기존 파일의 권한과
		// 상관없이 같은 내용의 재업로드가 성공한다. 제자리 쓰기(os.WriteFile)는
		// 여기서 EACCES 로 실패한다 — 2026-08 uid 불일치 장애 때의 모양이다.
		dir := t.TempDir()
		audioPath := filepath.Join(dir, recordingAudioName(number, dateMs))
		if err := os.WriteFile(audioPath, []byte("old"), 0o444); err != nil {
			t.Fatal(err)
		}
		srv, _ := newRecordingTestServer(t, &stubIngestUpserter{}, dir, 0, time.Time{})
		body, ct := buildRecordingForm(t, "rec.m4a", validM4ABytes(64), number, dateMs)
		if rr := doRecordingPost(t, srv, body, ct, "Bearer test-key"); rr.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
		}
		if got, _ := os.ReadFile(audioPath); !bytes.Equal(got, validM4ABytes(64)) {
			t.Errorf("audio not replaced (len %d)", len(got))
		}
	})

	t.Run("audio write fails", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// 오디오 저장 경로에 디렉터리를 만들어 두면 이름 바꾸기가 실패한다.
		if err := os.Mkdir(filepath.Join(dir, recordingAudioName(number, dateMs)), 0o755); err != nil {
			t.Fatal(err)
		}
		upserter := &stubIngestUpserter{}
		srv, _ := newRecordingTestServer(t, upserter, dir, 0, time.Time{})
		body, ct := buildRecordingForm(t, "rec.m4a", validM4ABytes(64), number, dateMs)
		assert503(t, doRecordingPost(t, srv, body, ct, "Bearer test-key"))
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".partial") {
				t.Errorf("temporary file left behind: %s", e.Name())
			}
		}
		if len(upserter.upserted) != 0 {
			t.Errorf("upserts = %d, want 0", len(upserter.upserted))
		}
	})
}

// TestWriteFileAtomic 은 온전한 내용만 보이고 임시 파일이 남지 않는지 본다.
func TestWriteFileAtomic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "a.m4a")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("new-content")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "new-content" {
		t.Fatalf("content = %q, %v", got, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("entries = %d, want 1 (no leftover temporary file)", len(entries))
	}
}

// TestIngestRecording_LogsCarryNoPII: 실패 경로의 로그에 번호·연락처·파일
// 이름이 없어야 한다. 해싱을 끈 기본 설정에서는 저장 파일 이름이 곧 번호이고,
// 앱이 올리는 원래 파일 이름에도 번호·연락처가 있다. slog 기본 로거를 바꾸므로
// 병렬로 돌리지 않는다.
func TestIngestRecording_LogsCarryNoPII(t *testing.T) {
	logs := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const (
		number  = "010-5555-1234"
		contact = "홍길동테스트"
	)
	uploadName := contact + "_" + number + "_20260301.m4a"
	dateMs := time.Now().Add(-time.Hour).UnixMilli()
	newSrv := func(upserter IngestRecordingUpserter, dir string) *Server {
		return NewServer(nil, nil, nil, nil, nil, "", "test-key").
			WithPIINumberHashing(false).
			WithIngestRecording(upserter, dir, 0, time.Time{})
	}
	post := func(srv *Server, audio []byte) *httptest.ResponseRecorder {
		body, ct := buildRecordingForm(t, uploadName, audio, number, dateMs, "contact_name", contact)
		return doRecordingPost(t, srv, body, ct, "Bearer test-key")
	}

	// 1) 손상 오디오 400, 2) 문서 저장 일시 오류 503, 3) 오디오 쓰기 실패 503,
	// 4) 잘린 업로드 503, 5) 중복 200.
	if rr := post(newSrv(&stubIngestUpserter{}, t.TempDir()), []byte("not-audio-bytes")); rr.Code != http.StatusBadRequest {
		t.Fatalf("corrupt: status %d", rr.Code)
	}
	if rr := post(newSrv(&stubIngestUpserter{err: &pgconn.PgError{Code: "08006", Message: number}}, t.TempDir()), validM4ABytes(64)); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("transient upsert: status %d", rr.Code)
	}
	blocked := t.TempDir()
	if err := os.Mkdir(filepath.Join(blocked, sanitizePhoneNumber(number)+"_"+time.UnixMilli(dateMs).UTC().In(time.Local).Format("20060102150405")+".m4a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if rr := post(newSrv(&stubIngestUpserter{}, blocked), validM4ABytes(64)); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("audio write: status %d", rr.Code)
	}
	full, ct := buildRecordingForm(t, uploadName, validM4ABytes(4096), number, dateMs, "contact_name", contact)
	if rr := recordingPostRaw(t, newSrv(&stubIngestUpserter{}, t.TempDir()),
		&failingReader{data: bytes.NewReader(full.Bytes()[:full.Len()/2]), err: io.ErrUnexpectedEOF}, ct); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("truncated: status %d", rr.Code)
	}
	if rr := post(newSrv(&stubIngestUpserter{err: store.ErrDuplicateTranscript}, t.TempDir()), validM4ABytes(64)); rr.Code != http.StatusOK {
		t.Fatalf("duplicate: status %d", rr.Code)
	}

	out := logs.String()
	for _, want := range []string{
		"rejecting corrupt audio upload",
		"transient upsert failure",
		"write audio file failed",
		"multipart read failed",
		"duplicate call content",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected log %q not found (positive control)", want)
		}
	}
	for _, pii := range []string{number, "01055551234", "5555", contact, uploadName, blocked} {
		if strings.Contains(out, pii) {
			t.Errorf("log leaks %q:\n%s", pii, out)
		}
	}
}

// TestIngestRecording_SlowUploadNotCutByServerReadTimeout: 서버 전체의
// ReadTimeout 보다 오래 걸리는 업로드도 끝까지 받는다(recordingReadDeadline).
// 이 연장이 없으면 느린 업로드는 매번 같은 이유로 끊겨, 503 으로 바꾼 뒤에는
// 앱 녹음 루프가 그 파일에서 영원히 멈춘다.
func TestIngestRecording_SlowUploadNotCutByServerReadTimeout(t *testing.T) {
	t.Parallel()
	srv, dir := newRecordingTestServer(t, &stubIngestUpserter{}, "", 0, time.Time{})
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.Config.ReadTimeout = 300 * time.Millisecond
	ts.Start()
	t.Cleanup(ts.Close)

	full, ct := buildRecordingForm(t, "rec.m4a", validM4ABytes(8192), "010-0000-0001",
		time.Now().Add(-time.Hour).UnixMilli())
	data := full.Bytes()
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write(data[:len(data)/2])
		time.Sleep(900 * time.Millisecond) // ReadTimeout 의 세 배
		_, _ = pw.Write(data[len(data)/2:])
		_ = pw.Close()
	}()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/ingest/recording", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // 테스트
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201; body=%s", resp.StatusCode, b)
	}
	if n := len(audioFilesInDir(t, dir)); n != 1 {
		t.Errorf("audio files = %d, want 1", n)
	}
}

// TestIngestRecording_NoDeadlineExtensionWithoutAPIKey (#292 리뷰 LOW): API_KEY
// 가 비어 인증이 꺼져 있으면 읽기 기한을 늘리지 않는다 — 인증 없는 느린
// 업로드가 200초 동안 연결을 붙잡지 못하게. 서버 ReadTimeout 에 끊겨야 한다.
func TestIngestRecording_NoDeadlineExtensionWithoutAPIKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	srv := NewServer(nil, nil, nil, nil, nil, "", "").
		WithPIINumberHashing(true).
		WithIngestRecording(&stubIngestUpserter{}, dir, 0, time.Time{})
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.Config.ReadTimeout = 300 * time.Millisecond
	ts.Start()
	t.Cleanup(ts.Close)

	full, ct := buildRecordingForm(t, "rec.m4a", validM4ABytes(8192), "010-0000-0001",
		time.Now().Add(-time.Hour).UnixMilli())
	data := full.Bytes()
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write(data[:len(data)/2])
		time.Sleep(900 * time.Millisecond)
		_, _ = pw.Write(data[len(data)/2:])
		_ = pw.Close()
	}()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/ingest/recording", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Type", ct)
	resp, err := ts.Client().Do(req)
	if err == nil {
		defer resp.Body.Close() //nolint:errcheck // 테스트
		if resp.StatusCode == http.StatusCreated {
			t.Fatalf("status = 201: read deadline was extended for an unauthenticated upload")
		}
	}
	if n := len(audioFilesInDir(t, dir)); n != 0 {
		t.Errorf("audio files = %d, want 0", n)
	}
}

// TestIngestRecording_StoredNamesStayWithinLimits (#292 리뷰 LOW): 사용자
// 입력(원래 파일 이름·확장자·번호)이 길어도 저장 이름이 파일 시스템 한도
// (255바이트)를 넘지 않는다. 넘으면 임시 이름 쓰기가 매번 ENAMETOOLONG(503)
// 으로 실패해 앱 녹음 루프가 그 파일에서 멈춘다.
func TestIngestRecording_StoredNamesStayWithinLimits(t *testing.T) {
	t.Parallel()
	dateMs := time.Now().Add(-time.Hour).UnixMilli()
	long := strings.Repeat("a", 240)

	post := func(t *testing.T, srv *Server, filename, number string, extra ...string) {
		t.Helper()
		body, ct := buildRecordingForm(t, filename, validM4ABytes(64), number, dateMs, extra...)
		if rr := doRecordingPost(t, srv, body, ct, "Bearer test-key"); rr.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
		}
	}
	onlyAudio := func(t *testing.T, dir string) string {
		t.Helper()
		names := audioFilesInDir(t, dir)
		if len(names) != 1 {
			t.Fatalf("audio files = %v, want exactly 1", names)
		}
		if len(names[0]) > 150 {
			t.Errorf("stored name is %d bytes, want <= 150: %s", len(names[0]), names[0])
		}
		return names[0]
	}

	t.Run("long voice-memo name", func(t *testing.T) {
		t.Parallel()
		srv, dir := newRecordingTestServer(t, &stubIngestUpserter{}, "", 0, time.Time{})
		post(t, srv, long+".m4a", "", "kind", "voice-memo")
		if name := onlyAudio(t, dir); !strings.HasSuffix(name, ".m4a") {
			t.Errorf("name = %s, want .m4a suffix", name)
		}
	})

	t.Run("long names with a shared prefix do not collide", func(t *testing.T) {
		t.Parallel()
		srv, dir := newRecordingTestServer(t, &stubIngestUpserter{}, "", 0, time.Time{})
		post(t, srv, long+"-one.m4a", "", "kind", "voice-memo")
		post(t, srv, long+"-two.m4a", "", "kind", "voice-memo")
		if names := audioFilesInDir(t, dir); len(names) != 2 {
			t.Errorf("audio files = %v, want 2 distinct files", names)
		}
	})

	t.Run("long unknown extension", func(t *testing.T) {
		t.Parallel()
		srv, dir := newRecordingTestServer(t, &stubIngestUpserter{}, "", 0, time.Time{})
		post(t, srv, "rec."+long, "010-0000-0001")
		if name := onlyAudio(t, dir); !strings.HasSuffix(name, recordingUnknownExt) {
			t.Errorf("name = %s, want %s suffix", name, recordingUnknownExt)
		}
	})

	t.Run("known extension keeps its spelling", func(t *testing.T) {
		t.Parallel()
		srv, dir := newRecordingTestServer(t, &stubIngestUpserter{}, "", 0, time.Time{})
		post(t, srv, "rec.M4A", "010-0000-0001")
		if name := onlyAudio(t, dir); !strings.HasSuffix(name, ".M4A") {
			t.Errorf("name = %s, want .M4A suffix (re-upload must hit the same path)", name)
		}
	})

	t.Run("long raw number with hashing off", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		srv := NewServer(nil, nil, nil, nil, nil, "", "test-key").
			WithPIINumberHashing(false).
			WithIngestRecording(&stubIngestUpserter{}, dir, 0, time.Time{})
		number := strings.Repeat("1", 300)
		post(t, srv, "rec.m4a", number)
		if name := onlyAudio(t, dir); !strings.HasPrefix(name, smsmap.ShortHash(number)+"_") {
			t.Errorf("name = %s, want the number hash as prefix", name)
		}
	})
}

// TestCleanupStaleRecordingPartials: 시작 시 청소는 오래된 임시 파일만 지운다.
func TestCleanupStaleRecordingPartials(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	mk := func(name string, mtime time.Time) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	mk(".a.m4a.123.partial", old)                                             // 지움
	mk(".b.m4a.456.partial", now)                                             // 진행 중일 수 있음 — 둠
	mk("c.m4a", old)                                                          // 오디오 — 둠
	mk("d.partial", old)                                                      // 점으로 시작하지 않음 — 둠
	if err := os.Mkdir(filepath.Join(dir, ".e.partial"), 0o755); err != nil { // 디렉터리 — 둠
		t.Fatal(err)
	}

	removed, err := CleanupStaleRecordingPartials(dir, RecordingPartialMaxAge, now)
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v, want 1/nil", removed, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 4 {
		t.Errorf("entries left = %d, want 4", len(entries))
	}
	if n, err := CleanupStaleRecordingPartials(filepath.Join(dir, "missing"), RecordingPartialMaxAge, now); n != 0 || err != nil {
		t.Errorf("missing dir: n=%d err=%v, want 0/nil", n, err)
	}
}
