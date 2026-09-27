package collector

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/config"
)

func syntheticCloudAudio(t *testing.T, dir, duration string) string {
	t.Helper()
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip("ffmpeg/ffprobe required for synthetic audio test")
		}
	}
	path := filepath.Join(dir, "private-contact-01012345678.wav")
	_, err := audioCommand(context.Background(), "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", duration, "-ac", "2", "-c:a", "pcm_s16le", path)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCloudLargeAudioTranscribesOriginalUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := syntheticCloudAudio(t, dir, "150")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) <= whisperCloudMaxFileBytes {
		t.Fatal("fixture is not oversized")
	}
	tempRoot := t.TempDir()
	t.Setenv("TMPDIR", tempRoot)
	originalHash := sha256.Sum256(before)
	info, _ := os.Stat(path)
	calls := 0
	failUpload := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if failUpload {
			w.WriteHeader(500)
			return
		}
		if err := r.ParseMultipartForm(whisperCloudMaxFileBytes + 1024); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		defer r.MultipartForm.RemoveAll()
		f, h, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		size, _ := io.Copy(io.Discard, f)
		if size <= 0 || size > whisperCloudMaxFileBytes {
			t.Errorf("upload size=%d", size)
		}
		if h.Filename != "audio.mp3" {
			t.Errorf("filename leaks source identity: %q", h.Filename)
		}
		_ = json.NewEncoder(w).Encode(whisperTranscribeResponse{Text: "합성 음원 전사"})
	}))
	defer srv.Close()
	c := makeCloudWhisperCollector(t, &config.Config{WhisperAudioDir: dir, WhisperModel: "gpt-4o-transcribe-diarize"}, srv)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil || len(docs) != 1 || calls != 1 {
		t.Fatalf("docs=%d calls=%d err=%v", len(docs), calls, err)
	}
	if docs[0].SourceID != "transcript:"+filepath.Base(path) || docs[0].Metadata["audio_size"] != info.Size() || !strings.Contains(docs[0].Content, "합성 음원 전사") {
		t.Fatalf("identity/content changed: %+v", docs[0])
	}
	if _, ok := docs[0].Metadata["diarization"]; ok {
		t.Fatal("flat fallback incorrectly marked diarized")
	}
	after, _ := os.ReadFile(path)
	afterInfo, _ := os.Stat(path)
	if sha256.Sum256(after) != originalHash || !afterInfo.ModTime().Equal(info.ModTime()) {
		t.Fatal("original changed")
	}
	failUpload = true
	result, err := c.transcribeLargeCloudFile(context.Background(), path, info)
	if err == nil || result.text != "" {
		t.Fatalf("failed upload returned transcript: %+v %v", result, err)
	}
	entries, err := os.ReadDir(tempRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary audio leaked: entries=%v err=%v", entries, err)
	}

}

func TestCloudAudioPartsTimeSpeakersAndCleanup(t *testing.T) {
	path := syntheticCloudAudio(t, t.TempDir(), "5")
	parts, cleanup, err := prepareCloudAudio(context.Background(), path, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(parts) != 3 {
		t.Fatalf("parts=%d", len(parts))
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(diarizedResponse{Text: "음성", Segments: []diarizedSegment{{Start: .1, End: .8, Speaker: "A", Text: "음성"}}})
	}))
	defer srv.Close()
	c := makeCloudWhisperCollector(t, &config.Config{WhisperAudioDir: filepath.Dir(path), WhisperModel: "gpt-4o-transcribe-diarize"}, srv)
	result, err := c.transcribeCloudParts(context.Background(), parts)
	if err != nil || calls != 3 || len(result.diarized) != 3 {
		t.Fatalf("result=%+v calls=%d err=%v", result, calls, err)
	}
	for i, seg := range result.diarized {
		if seg.Start != parts[i].start+.1 {
			t.Errorf("offset lost: %+v", seg)
		}
		if i > 0 && seg.Speaker == result.diarized[0].Speaker {
			t.Error("speakers conflated")
		}
	}
	if !strings.Contains(result.text, "구간 3") || !strings.Contains(result.text, "이 구간 안에서만 유효") {
		t.Fatal("missing segment scope")
	}
	cleanup()
	for _, part := range parts {
		if _, err := os.Stat(part.path); !os.IsNotExist(err) {
			t.Fatal("temporary audio retained")
		}
	}
}

func TestCloudAudioPartialFailureReturnsNoTranscript(t *testing.T) {
	path := syntheticCloudAudio(t, t.TempDir(), "3")
	parts, cleanup, err := prepareCloudAudio(context.Background(), path, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 2 {
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(w).Encode(whisperTranscribeResponse{Text: "partial"})
	}))
	defer srv.Close()
	c := makeCloudWhisperCollector(t, &config.Config{WhisperAudioDir: filepath.Dir(path), WhisperModel: "whisper-1"}, srv)
	result, err := c.transcribeCloudParts(context.Background(), parts)
	if err == nil || result.text != "" || result.parts != 0 {
		t.Fatalf("partial success: %+v err=%v", result, err)
	}
}

func TestCloudAudioFailureRetryAndCancellation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "private-01012345678.m4a")
	if err := os.WriteFile(path, []byte("invalid audio"), 0600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	c := NewWhisperCollector(&config.Config{WhisperAudioDir: dir, WhisperModel: "whisper-1"})
	_, err := c.transcribeLargeCloudFile(context.Background(), path, info)
	if err == nil || strings.Contains(err.Error(), "01012345678") {
		t.Fatalf("unsafe failure: %v", err)
	}
	_, err = c.transcribeLargeCloudFile(context.Background(), path, info)
	if !errors.Is(err, errWhisperRetryDeferred) {
		t.Fatalf("missing retry cooldown: %v", err)
	}
	raw, err := os.ReadFile(c.largeAudioRetryPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private") || !strings.Contains(string(raw), "retryable_failure") {
		t.Fatalf("unsafe retry state: %s", raw)
	}
	c.cfg.WhisperModel = "fixed-model"
	_, err = c.transcribeLargeCloudFile(context.Background(), path, info)
	if err == nil || errors.Is(err, errWhisperRetryDeferred) {
		t.Fatalf("model correction did not reset cooldown: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, cleanup, err := prepareCloudAudio(ctx, path, 2)
	cleanup()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err=%v", err)
	}
}

func TestAudioCommandCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := audioCommand(ctx, "sleep", "30")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 2*time.Second {
		t.Fatalf("command did not stop: %v", err)
	}
}

func TestNativeCloudSmallLongAudioIsSplit(t *testing.T) {
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip("ffmpeg/ffprobe required")
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "long.mp3")
	_, err := audioCommand(context.Background(), "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=16000", "-t", "1500", "-ac", "1", "-c:a", "libmp3lame", "-b:a", "32k", path)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Size() >= whisperCloudMaxFileBytes {
		t.Fatal("fixture must be below byte limit")
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if err := r.ParseMultipartForm(whisperCloudMaxFileBytes); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		defer r.MultipartForm.RemoveAll()
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		raw, _ := io.ReadAll(f)
		uploaded := filepath.Join(t.TempDir(), "uploaded.mp3")
		if err := os.WriteFile(uploaded, raw, 0600); err != nil {
			t.Error(err)
			return
		}
		duration, err := probeCloudAudioDuration(context.Background(), uploaded)
		if err != nil || duration > cloudAudioSegmentSeconds+1 {
			t.Errorf("upload duration=%v err=%v", duration, err)
		}
		_ = json.NewEncoder(w).Encode(diarizedResponse{Text: "합성 음원", Segments: []diarizedSegment{{Start: 0, End: 1, Speaker: "A", Text: "합성 음원"}}})
	}))
	defer srv.Close()
	c := makeCloudWhisperCollector(t, &config.Config{WhisperAudioDir: dir, WhisperModel: "gpt-4o-transcribe-diarize", WhisperChunkingStrategy: "auto"}, srv)
	result, err := c.transcribeFile(context.Background(), path, false)
	if err != nil || calls != 3 || result.parts != 3 {
		t.Fatalf("calls=%d parts=%d err=%v", calls, result.parts, err)
	}
}
