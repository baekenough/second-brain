package collector

// whisper_logsafe_test.go — #297: whisper 수집기의 로그·오류·트레이스·외부
// 요청에 전화번호(= 기본 설정의 녹음 파일 이름)가 나가지 않는지 본다.
//
// 모든 음성 검사("센티널 없음")에는 양성 대조를 붙인다(계획 §5.2):
//   1. 캡처 장치 대조: TestWhisperLogCapture_SeesSentinel
//   2. 이벤트 도달 대조: 각 테스트는 그 경로의 로그 이벤트가 실제로 찍혔고
//      file_ref 가 있다는 것을 먼저 확인한다(requireEvent). 이벤트가 없으면
//      "센티널 없음"은 아무것도 증명하지 못하므로 FAIL 이다.
//   3. file_ref 대조: 같은 파일은 같은 ref, source_id 의 ref 와도 같다.
//
// 테스트는 병렬로 돈다. 전역 slog 를 바꾸지 않고 수집기에 로거를 주입한다
// (WithLogger). 전역 OTel 제공자를 바꾸는 테스트만 병렬이 아니다.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/audiovalidate"
	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/logsafe"
)

const (
	logSentinelNum  = "01000009999"
	logSentinelFile = logSentinelNum + "_20260101120000.m4a"
	// logSentinelBody 는 외부 API 가 되돌려 주는 본문 조각이다.
	logSentinelBody = "SENTINEL-BODY-ZX9Q"
)

// logSentinelForms 는 찾을 모양이다. 숫자만 남긴 꼬리(00009999)와 한글 본문
// 센티널도 본다.
var logSentinelForms = []string{
	logSentinelNum, "010-0000-9999", "+821000009999", "00009999",
	logSentinelBody, "SENTINEL-본문-ZX9Q",
}

func sentinelFound(s string) string {
	for _, f := range logSentinelForms {
		if strings.Contains(s, f) {
			return f
		}
	}
	return ""
}

func assertNoLogSentinel(t *testing.T, where, s string) {
	t.Helper()
	if f := sentinelFound(s); f != "" {
		t.Errorf("%s leaks sentinel %q:\n%s", where, f, s)
	}
}

// syncBuffer 는 워커 고루틴이 동시에 쓰는 로그를 받는다.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// newCaptureLogger 는 Debug 까지 JSON 으로 남기는 주입용 로거다.
func newCaptureLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// events 는 캡처한 JSON 로그 줄을 파싱한다.
func (s *syncBuffer) events(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(s.String()), "\n") {
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("unparseable log line %q: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

// requireEvent 는 msg 로 시작하는 이벤트가 있고 file_ref 를 가졌는지 확인한다
// (이벤트 도달 대조). 없으면 FAIL.
func requireEvent(t *testing.T, buf *syncBuffer, msgPrefix string) map[string]any {
	t.Helper()
	for _, ev := range buf.events(t) {
		if m, _ := ev["msg"].(string); strings.HasPrefix(m, msgPrefix) {
			if ref, _ := ev["file_ref"].(string); ref == "" {
				t.Fatalf("event %q has no file_ref: %v", msgPrefix, ev)
			}
			return ev
		}
	}
	t.Fatalf("log event %q was not emitted — negative sentinel check would be vacuous.\nlogs:\n%s", msgPrefix, buf.String())
	return nil
}

func TestWhisperLogCapture_SeesSentinel(t *testing.T) {
	t.Parallel()
	// 양성 대조: 주입한 로거로 센티널을 직접 찍으면 버퍼에서 찾아져야 한다.
	logger, buf := newCaptureLogger()
	logger.Warn("whisper: control", "file_ref", "x", "p", "/data/call/"+logSentinelFile, "b", logSentinelBody)
	if sentinelFound(buf.String()) == "" {
		t.Fatalf("capture device does not see the sentinel: %q", buf.String())
	}
	requireEvent(t, buf, "whisper: control")
}

// whisperFailServer 는 status 와 본문(센티널 포함)을 돌려주는 전사 서버다.
// 받은 multipart 파일 이름을 기록한다.
type uploadNames struct {
	mu    sync.Mutex
	names []string
}

func (u *uploadNames) add(n string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.names = append(u.names, n)
}

func (u *uploadNames) all() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.names...)
}

func recordUploadName(r *http.Request, u *uploadNames) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		return
	}
	if fhs := r.MultipartForm.File["file"]; len(fhs) > 0 {
		u.add(fhs[0].Filename)
	}
}

func newStatusServer(t *testing.T, status int, body string, names *uploadNames) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recordUploadName(r, names)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// sentinelErrorBody 는 OpenAI 오류 모양이다. message 에 파일 이름·본문
// 센티널을 되돌려 준다(외부 API 가 요청 데이터를 반사하는 경우).
var sentinelErrorBody = `{"error":{"message":"cannot decode ` + logSentinelFile + ` ` + logSentinelBody +
	` SENTINEL-본문-ZX9Q","type":"server_error","code":"audio_decode_failed"}}`

func newLogTestCollector(t *testing.T, cfg *config.Config, srv *httptest.Server) (*WhisperCollector, *syncBuffer) {
	t.Helper()
	logger, buf := newCaptureLogger()
	c := NewWhisperCollector(cfg).WithLogger(logger)
	if srv != nil {
		c.httpClient = srv.Client()
		c.baseURL = srv.URL
	}
	return c, buf
}

// TestWhisperLog_TranscriptionFailed_NoPathNoBody — W7·W13·W15: 전사 API 가
// 500 과 함께 파일 이름·본문을 되돌려 줘도 로그·반환 오류·span 에 없다.
// 전역 OTel 제공자를 바꾸므로 병렬이 아니다(whisper_otel_test.go 와 같은 이유).
func TestWhisperLog_TranscriptionFailed_NoPathNoBody(t *testing.T) {
	exp := withInMemoryTracer(t)

	dir := t.TempDir()
	path := writeDummyAudio(t, dir, logSentinelFile, time.Now().Add(-time.Hour))
	names := &uploadNames{}
	srv := newStatusServer(t, http.StatusInternalServerError, sentinelErrorBody, names)

	cfg := &config.Config{WhisperAudioDir: dir, WhisperAPIURL: srv.URL, WhisperModel: "whisper-1"}
	c, buf := newLogTestCollector(t, cfg, srv)
	c.WithIndexedIDs(map[string]struct{}{})

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("got %d docs, want 0 (transcription failed)", len(docs))
	}

	ev := requireEvent(t, buf, "whisper: transcription failed")
	if ev["status"] != float64(500) {
		t.Errorf("status attr = %v, want 500", ev["status"])
	}
	if ev["upstream_error_type"] != "server_error" || ev["upstream_error_code"] != "audio_decode_failed" {
		t.Errorf("upstream error attrs = %v / %v", ev["upstream_error_type"], ev["upstream_error_code"])
	}
	if ev["file_ref"] != logsafe.FileRef(logSentinelFile) {
		t.Errorf("file_ref = %v, want FileRef(relPath)", ev["file_ref"])
	}
	if ev["kind"] != "call" || ev["ext"] != ".m4a" {
		t.Errorf("kind/ext = %v/%v, want call/.m4a", ev["kind"], ev["ext"])
	}
	assertNoLogSentinel(t, "logs", buf.String())

	// 반환 오류: 기존 접두사 유지 + 본문 없음.
	_, txErr := c.transcribeFile(context.Background(), path, true)
	if txErr == nil {
		t.Fatal("transcribeFile: expected error")
	}
	want := "whisper API returned 500 (error.type=server_error, code=audio_decode_failed)"
	if txErr.Error() != want {
		t.Errorf("error = %q, want %q", txErr.Error(), want)
	}
	var se *whisperStatusError
	if !errors.As(txErr, &se) || se.statusCode != 500 {
		t.Errorf("errors.As(*whisperStatusError) failed: %v", txErr)
	}
	assertNoLogSentinel(t, "returned error", txErr.Error())

	// span: 이벤트(exception.message)와 상태 문구.
	spans := exp.GetSpans()
	if len(spans) == 0 {
		t.Fatal("no transcription span recorded")
	}
	for _, s := range spans {
		if s.Name != "transcription" {
			continue
		}
		if s.Status.Description != "transcription failed" {
			t.Errorf("span status = %q, want fixed phrase", s.Status.Description)
		}
		if len(s.Events) == 0 {
			t.Error("span has no error event — RecordError was expected (positive control)")
		}
		for _, e := range s.Events {
			for _, a := range e.Attributes {
				assertNoLogSentinel(t, "span event "+string(a.Key), a.Value.Emit())
			}
		}
	}

	// W20: 업로드 파일 이름은 audio.m4a.
	got := names.all()
	if len(got) == 0 {
		t.Fatal("server saw no upload (positive control)")
	}
	for _, n := range got {
		if n != "audio.m4a" {
			t.Errorf("multipart filename = %q, want audio.m4a", n)
		}
	}
}

// TestWhisperLog_TranscriptionReadFailed — W7(PathError): 파일을 읽지 못한
// 오류에서 경로가 빠지고 op·errno 만 남는다.
func TestWhisperLog_TranscriptionReadFailed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := &config.Config{WhisperAudioDir: dir, WhisperAPIURL: "http://127.0.0.1:1", WhisperModel: "whisper-1"}
	c, buf := newLogTestCollector(t, cfg, nil)

	missing := filepath.Join(dir, logSentinelFile)
	info := fakeFileInfo{name: logSentinelFile}
	_, ok := c.buildDocument(context.Background(), pendingTranscription{
		path: missing, info: info, relPath: logSentinelFile, sourceID: "transcript:" + logSentinelFile,
	}, time.Now(), true)
	if ok {
		t.Fatal("buildDocument succeeded on a missing file")
	}
	ev := requireEvent(t, buf, "whisper: transcription failed")
	if ev["op"] != "open" || ev["errno"] == nil || ev["step"] != "read audio file" {
		t.Errorf("want op=open, errno, step=read audio file; got %v", ev)
	}
	assertNoLogSentinel(t, "logs", buf.String())

	_, err := c.transcribeFile(context.Background(), missing, true)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Errorf("errors.Is(err, os.ErrNotExist) must survive StripPath: %v", err)
	}
	assertNoLogSentinel(t, "returned error", fmt.Sprint(err))
}

type fakeFileInfo struct {
	os.FileInfo
	name string
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 32 }
func (f fakeFileInfo) ModTime() time.Time { return time.Now().Add(-time.Hour) }

// TestWhisperLog_SizeCaps — W5·W6: 크기 상한으로 건너뛸 때.
func TestWhisperLog_SizeCaps(t *testing.T) {
	t.Parallel()

	t.Run("configured cap", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeDummyAudio(t, dir, logSentinelFile, time.Now().Add(-time.Hour))
		cfg := &config.Config{WhisperAudioDir: dir, WhisperAPIURL: "http://127.0.0.1:1", WhisperModel: "whisper-1"}
		c, buf := newLogTestCollector(t, cfg, nil)
		c.maxFileBytes = 8
		c.WithIndexedIDs(map[string]struct{}{})
		if _, err := c.Collect(context.Background(), time.Time{}); err != nil {
			t.Fatalf("Collect: %v", err)
		}
		ev := requireEvent(t, buf, "whisper: skipping oversized file")
		if ev["size_bytes"] == nil || ev["limit_bytes"] != float64(8) {
			t.Errorf("size attrs missing: %v", ev)
		}
		assertNoLogSentinel(t, "logs", buf.String())
	})

	t.Run("cloud 25MiB cap", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		p := filepath.Join(dir, logSentinelFile)
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		// 희소 파일: 디스크를 쓰지 않고 크기만 25MiB+1.
		if err := f.Truncate(whisperCloudMaxFileBytes + 1); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		cfg := &config.Config{WhisperAudioDir: dir, WhisperAPIURL: "https://cloud.example.test", WhisperModel: "whisper-1"}
		c, buf := newLogTestCollector(t, cfg, nil)
		c.WithIndexedIDs(map[string]struct{}{})
		if _, err := c.Collect(context.Background(), time.Time{}); err != nil {
			t.Fatalf("Collect: %v", err)
		}
		requireEvent(t, buf, "whisper: skipping file over cloud API 25 MiB limit")
		assertNoLogSentinel(t, "logs", buf.String())
	})
}

// TestWhisperLog_WalkError — W3: 읽을 수 없는 디렉터리(이름이 번호)를 만나도
// 경로가 로그에 없다. root 로 돌면 권한 검사가 없어 건너뛴다.
func TestWhisperLog_WalkError(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission 000 is not enforced")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, logSentinelNum)
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	cfg := &config.Config{WhisperAudioDir: dir, WhisperAPIURL: "http://127.0.0.1:1", WhisperModel: "whisper-1"}
	c, buf := newLogTestCollector(t, cfg, nil)
	if _, err := c.Collect(context.Background(), time.Time{}); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	ev := requireEvent(t, buf, "whisper: walk error")
	if ev["op"] == nil || ev["errno"] == nil {
		t.Errorf("walk error attrs missing op/errno: %v", ev)
	}
	assertNoLogSentinel(t, "logs", buf.String())
}

// TestWhisperLog_OccurredAtFallbacks — W1·W2: 통화 시각을 파일 이름에서 못 쓸 때.
func TestWhisperLog_OccurredAtFallbacks(t *testing.T) {
	t.Parallel()
	logger, buf := newCaptureLogger()
	dir := "/data/call"
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	mtime := now.Add(-time.Hour)

	future := logSentinelNum + "_20991231235959.m4a"
	got := callRecordingOccurredAt(logger, whisperFileAttrs(dir, filepath.Join(dir, future)), future, mtime, now)
	if !got.Equal(mtime) {
		t.Fatalf("future timestamp must fall back to mtime, got %v", got)
	}
	ev := requireEvent(t, buf, "whisper: filename timestamp is in the future")
	if ev["parsed"] == nil {
		t.Errorf("parsed call time should stay in the log: %v", ev)
	}

	noTS := logSentinelNum + ".m4a"
	callRecordingOccurredAt(logger, whisperFileAttrs(dir, filepath.Join(dir, noTS)), noTS, mtime, now)
	requireEvent(t, buf, "whisper: filename has no usable embedded call timestamp")
	assertNoLogSentinel(t, "logs", buf.String())
}

// verboseServer 는 verbose_json(세그먼트 포함) 전사 응답을 돌려준다.
// onRequest 가 있으면 응답 전에 부른다.
func verboseServer(t *testing.T, segs []whisperSegment, names *uploadNames, onRequest func()) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recordUploadName(r, names)
		if onRequest != nil {
			onRequest()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(whisperVerboseResponse{Text: "전사", Segments: segs})
	}))
	t.Cleanup(srv.Close)
	return srv
}

var twoSegs = []whisperSegment{{Start: 0, End: 1, Text: "안녕"}, {Start: 1, End: 2, Text: "네"}}

// TestWhisperLog_DiarizationFailed — W10·W14·W21: 화자 분리 서버가 500 과
// 센티널 본문을 돌려줘도 로그에 없다. 두 요청의 파일 이름은 audio.m4a.
func TestWhisperLog_DiarizationFailed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeDummyAudio(t, dir, logSentinelFile, time.Now().Add(-time.Hour))

	txNames, diarNames := &uploadNames{}, &uploadNames{}
	txSrv := verboseServer(t, twoSegs, txNames, nil)
	diarSrv := newStatusServer(t, http.StatusInternalServerError, sentinelErrorBody, diarNames)

	cfg := &config.Config{
		WhisperAudioDir: dir, WhisperAPIURL: txSrv.URL, WhisperModel: "whisper-1",
		DiarizationEnabled: true, DiarizationAPIURL: diarSrv.URL,
	}
	c, buf := newLogTestCollector(t, cfg, txSrv)
	c.WithIndexedIDs(map[string]struct{}{})
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil || len(docs) != 1 {
		t.Fatalf("Collect = %d docs, %v; want 1 (plain transcript fallback)", len(docs), err)
	}
	ev := requireEvent(t, buf, "whisper: diarization failed")
	if ev["status"] != float64(500) {
		t.Errorf("status attr = %v, want 500", ev["status"])
	}
	assertNoLogSentinel(t, "logs", buf.String())

	for _, u := range []*uploadNames{txNames, diarNames} {
		got := u.all()
		if len(got) != 1 || got[0] != "audio.m4a" {
			t.Errorf("multipart filenames = %v, want [audio.m4a]", got)
		}
	}
}

// TestWhisperLog_DiarizationReread — W9: 전사 뒤 파일이 사라져 다시 읽지
// 못할 때.
func TestWhisperLog_DiarizationReread(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeDummyAudio(t, dir, logSentinelFile, time.Now().Add(-time.Hour))
	txSrv := verboseServer(t, twoSegs, &uploadNames{}, func() { _ = os.Remove(path) })

	cfg := &config.Config{
		WhisperAudioDir: dir, WhisperAPIURL: txSrv.URL, WhisperModel: "whisper-1",
		DiarizationEnabled: true, DiarizationAPIURL: "http://127.0.0.1:1",
	}
	c, buf := newLogTestCollector(t, cfg, txSrv)
	c.WithIndexedIDs(map[string]struct{}{})
	if _, err := c.Collect(context.Background(), time.Time{}); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	ev := requireEvent(t, buf, "whisper: diarization skipped — cannot re-read audio file")
	if ev["op"] != "open" {
		t.Errorf("op = %v, want open", ev["op"])
	}
	assertNoLogSentinel(t, "logs", buf.String())
}

// TestWhisperLog_EmptyRenders — W8·W11·W12: 화자 분리 결과가 비어 평문으로
// 돌아가는 세 경로.
func TestWhisperLog_EmptyRenders(t *testing.T) {
	t.Parallel()

	t.Run("labelTranscript empty (W11)", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeDummyAudio(t, dir, logSentinelFile, time.Now().Add(-time.Hour))
		blank := []whisperSegment{{Start: 0, End: 1, Text: "  "}}
		txSrv := verboseServer(t, blank, &uploadNames{}, nil)
		diarSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(diarizeResponse{Segments: []diarSegment{{Start: 0, End: 1, Speaker: "SPEAKER_00"}}})
		}))
		t.Cleanup(diarSrv.Close)
		cfg := &config.Config{
			WhisperAudioDir: dir, WhisperAPIURL: txSrv.URL, WhisperModel: "whisper-1",
			DiarizationEnabled: true, DiarizationAPIURL: diarSrv.URL,
		}
		c, buf := newLogTestCollector(t, cfg, txSrv)
		c.WithIndexedIDs(map[string]struct{}{})
		if _, err := c.Collect(context.Background(), time.Time{}); err != nil {
			t.Fatalf("Collect: %v", err)
		}
		requireEvent(t, buf, "whisper: labelTranscript produced empty output")
		assertNoLogSentinel(t, "logs", buf.String())
	})

	nativeServer := func(t *testing.T, segs []diarizedSegment) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(diarizedResponse{Text: "전사", Segments: segs})
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	t.Run("native render empty (W8)", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeDummyAudio(t, dir, logSentinelFile, time.Now().Add(-time.Hour))
		srv := nativeServer(t, []diarizedSegment{{Speaker: "A", Text: " "}})
		cfg := &config.Config{WhisperAudioDir: dir, WhisperAPIURL: srv.URL, WhisperModel: "gpt-4o-transcribe-diarize"}
		c, buf := newLogTestCollector(t, cfg, srv)
		c.WithIndexedIDs(map[string]struct{}{})
		if _, err := c.Collect(context.Background(), time.Time{}); err != nil {
			t.Fatalf("Collect: %v", err)
		}
		requireEvent(t, buf, "whisper: renderSpeakerBlocks produced empty output")
		assertNoLogSentinel(t, "logs", buf.String())
	})

	t.Run("native zero segments (W12)", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeDummyAudio(t, dir, logSentinelFile, time.Now().Add(-time.Hour))
		srv := nativeServer(t, nil)
		cfg := &config.Config{WhisperAudioDir: dir, WhisperAPIURL: srv.URL, WhisperModel: "gpt-4o-transcribe-diarize"}
		c, buf := newLogTestCollector(t, cfg, srv)
		c.WithIndexedIDs(map[string]struct{}{})
		if _, err := c.Collect(context.Background(), time.Time{}); err != nil {
			t.Fatalf("Collect: %v", err)
		}
		requireEvent(t, buf, "whisper: diarized_json response had zero segments")
		assertNoLogSentinel(t, "logs", buf.String())
	})
}

// TestWhisperLog_Quarantine — W16·W17·W18·W19: 손상 파일 격리의 네 로그.
func TestWhisperLog_Quarantine(t *testing.T) {
	t.Parallel()

	collectCorrupt := func(t *testing.T, prepare func(dir string)) (*syncBuffer, string) {
		t.Helper()
		dir := t.TempDir()
		writeCorruptM4A(t, dir, logSentinelFile, time.Now().Add(-time.Hour))
		if prepare != nil {
			prepare(dir)
		}
		cfg := &config.Config{WhisperAudioDir: dir, WhisperAPIURL: "http://127.0.0.1:1", WhisperModel: "whisper-1"}
		c, buf := newLogTestCollector(t, cfg, nil)
		c.WithIndexedIDs(map[string]struct{}{})
		if _, err := c.Collect(context.Background(), time.Time{}); err != nil {
			t.Fatalf("Collect: %v", err)
		}
		return buf, dir
	}

	t.Run("quarantined (W18)", func(t *testing.T) {
		t.Parallel()
		buf, _ := collectCorrupt(t, nil)
		ev := requireEvent(t, buf, "whisper: corrupt audio file quarantined")
		if ev["reason"] != "not_m4a" {
			t.Errorf("reason = %v, want not_m4a", ev["reason"])
		}
		assertNoLogSentinel(t, "logs", buf.String())
	})

	t.Run("mkdir failed (W16)", func(t *testing.T) {
		t.Parallel()
		buf, _ := collectCorrupt(t, func(dir string) {
			// .quarantine 을 파일로 미리 만들어 MkdirAll 을 실패시킨다.
			if err := os.WriteFile(filepath.Join(dir, whisperQuarantineDir), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		})
		ev := requireEvent(t, buf, "whisper: cannot create quarantine dir")
		if ev["mkdir_errno"] == nil || ev["quarantine_dir"] == nil {
			t.Errorf("mkdir attrs missing: %v", ev)
		}
		assertNoLogSentinel(t, "logs", buf.String())
	})

	t.Run("move failed (W17)", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("running as root: read-only directory is not enforced")
		}
		buf, dir := collectCorrupt(t, func(dir string) {
			q := filepath.Join(dir, whisperQuarantineDir)
			if err := os.Mkdir(q, 0o555); err != nil {
				t.Fatal(err)
			}
		})
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, whisperQuarantineDir), 0o755) })
		ev := requireEvent(t, buf, "whisper: quarantine move failed")
		if ev["move_op"] != "rename" || ev["move_errno"] == nil {
			t.Errorf("move attrs missing: %v", ev)
		}
		assertNoLogSentinel(t, "logs", buf.String())
	})

	t.Run("sidecar move failed (W19)", func(t *testing.T) {
		t.Parallel()
		buf, _ := collectCorrupt(t, func(dir string) {
			if err := os.WriteFile(filepath.Join(dir, logSentinelFile+".meta.json"), []byte(`{"number":"`+logSentinelNum+`"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			// 사이드카 목적지를 비어 있지 않은 디렉터리로 막아 rename 을 실패시킨다.
			blocker := filepath.Join(dir, whisperQuarantineDir, logSentinelFile+".meta.json")
			if err := os.MkdirAll(filepath.Join(blocker, "x"), 0o755); err != nil {
				t.Fatal(err)
			}
		})
		requireEvent(t, buf, "whisper: corrupt audio file quarantined")
		ev := requireEvent(t, buf, "whisper: could not move sidecar to quarantine")
		if ev["op"] != "rename" || ev["errno"] == nil {
			t.Errorf("sidecar error attrs missing: %v", ev)
		}
		assertNoLogSentinel(t, "logs", buf.String())
	})
}

func TestQuarantineReasonAttrs(t *testing.T) {
	t.Parallel()
	_, openErr := os.Open(filepath.Join(t.TempDir(), logSentinelFile))
	cases := []struct {
		err  error
		want string
	}{
		{audiovalidate.ErrTooShort, "too_short"},
		{audiovalidate.ErrNotM4A, "not_m4a"},
		{whisperStep("open audio header", logsafe.StripPath(openErr)), "unreadable"},
		{nil, "unknown"},
	}
	for _, tc := range cases {
		attrs := quarantineReasonAttrs(tc.err)
		if attrs[1] != tc.want {
			t.Errorf("reason(%v) = %v, want %s", tc.err, attrs[1], tc.want)
		}
		assertNoLogSentinel(t, "reason attrs", fmt.Sprint(attrs...))
	}
}

// TestWhisperFileRef_MatchesSourceID — file_ref 대조: 같은 파일은 같은 ref,
// 다른 파일은 다른 ref, 그리고 transcript source_id 의 ref 와 같은 값이다.
func TestWhisperFileRef_MatchesSourceID(t *testing.T) {
	t.Parallel()
	root := "/data/call"
	rel := filepath.Join("TPhoneCallRecords", logSentinelFile)
	a := whisperFileAttrs(root, filepath.Join(root, rel))
	b := whisperFileAttrs(root, filepath.Join(root, rel))
	other := whisperFileAttrs(root, filepath.Join(root, "TPhoneCallRecords", "01000009998_20260101120000.m4a"))
	if a[1] != b[1] {
		t.Errorf("same file, different refs: %v vs %v", a[1], b[1])
	}
	if a[1] == other[1] {
		t.Errorf("different files, same ref %v", a[1])
	}
	if want := "transcript:ref=" + a[1].(string); logsafe.SafeSourceID("transcript:"+rel) != want {
		t.Errorf("source_id ref %q != whisper file_ref %q", logsafe.SafeSourceID("transcript:"+rel), want)
	}
	// 반환 슬라이스는 len==cap 이어야 호출자 append 가 서로를 덮어쓰지 않는다.
	if len(a) != cap(a) {
		t.Errorf("whisperFileAttrs len %d != cap %d", len(a), cap(a))
	}
}

func TestWhisperFileKind(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		logSentinelFile:                         "call",
		"abc123_20260101120000-1.m4a":           "call",
		"voice-memo_20260101120000.m4a":         "voice-memo",
		"voice-memo_20260101120000_meeting.m4a": "voice-memo",
		"Voice 001_260101_120000.m4a":           "voice-memo",
		"random.m4a":                            "other",
	}
	for name, want := range cases {
		if got := whisperFileKind("/d/" + name); got != want {
			t.Errorf("whisperFileKind(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestReadWhisperStatusError_Shapes — 본문 모양별: 검증을 통과한 type/code 만
// 남고, 자유 문구·숫자 code·거대 본문은 버린다.
func TestReadWhisperStatusError_Shapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, body, want string
	}{
		{"openai shape", sentinelErrorBody, "whisper API returned 400 (error.type=server_error, code=audio_decode_failed)"},
		{"free text type dropped", `{"error":{"type":"Bad ` + logSentinelNum + `","code":123}}`, "whisper API returned 400"},
		{"non-json", logSentinelBody + " " + logSentinelFile, "whisper API returned 400"},
		// 숫자로 시작하는 토큰(번호 모양)은 버린다. 글자로 시작하면 숫자가 섞여도 남긴다.
		{"digit-leading tokens dropped", `{"error":{"type":"` + logSentinelNum + `","code":"123"}}`, "whisper API returned 400"},
		{"letter-leading token kept", `{"error":{"type":"e1","code":"x.y-z_9"}}`, "whisper API returned 400 (error.type=e1, code=x.y-z_9)"},
		// type 이 읽기 상한(64KiB) 뒤에 있으면 JSON 이 잘려 버린다 — 상한이 실제로 걸린다.
		{"huge body", `{"error":{"message":"` + strings.Repeat("a", whisperMaxErrorBodyBytes+10) + `","type":"x"}}`, "whisper API returned 400"},
	}
	for _, tc := range cases {
		resp := &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(tc.body))}
		got := readWhisperStatusError("whisper", resp).Error()
		if got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
		assertNoLogSentinel(t, tc.name, got)
	}
}

func TestReadBoundedBody(t *testing.T) {
	t.Parallel()
	b, err := readBoundedBody(strings.NewReader("12345"), 5)
	if err != nil || string(b) != "12345" {
		t.Errorf("at cap: %q, %v", b, err)
	}
	if _, err := readBoundedBody(strings.NewReader("123456"), 5); !errors.Is(err, errWhisperBodyTooLarge) {
		t.Errorf("over cap: err = %v, want errWhisperBodyTooLarge", err)
	}
}

// TestWhisperStatusError_DrainsForReuse — 비-200 본문을 상한까지 읽어 버려서
// keep-alive 연결을 다시 쓴다. 본문은 머리 상한(64KiB)보다 크고 드레인
// 상한(1MiB)보다 작다. 드레인이 없으면 요청마다 새 연결이 생긴다.
func TestWhisperStatusError_DrainsForReuse(t *testing.T) {
	t.Parallel()
	var (
		mu       sync.Mutex
		newConns int
	)
	big := `{"error":{"type":"server_error"}}` + strings.Repeat(" ", whisperMaxErrorBodyBytes*2)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, big)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			mu.Lock()
			newConns++
			mu.Unlock()
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	path := writeDummyAudio(t, dir, logSentinelFile, time.Now().Add(-time.Hour))
	c, _ := newLogTestCollector(t, &config.Config{WhisperAudioDir: dir, WhisperAPIURL: srv.URL, WhisperModel: "whisper-1"}, srv)
	for i := 0; i < 3; i++ {
		_, err := c.transcribeFile(context.Background(), path, true)
		var se *whisperStatusError
		if !errors.As(err, &se) || se.statusCode != http.StatusServiceUnavailable {
			t.Fatalf("call %d: err = %v, want whisperStatusError 503", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if newConns != 1 {
		t.Errorf("new connections = %d, want 1 (non-200 body must be drained for keep-alive reuse)", newConns)
	}
}
