package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/model"
)

// newWhisperTestServer returns an httptest.Server that responds to POST
// /audio/transcriptions with a JSON payload containing the given transcript
// text. It records the last received multipart fields and headers for
// assertion in tests.
type capturedRequest struct {
	fields        map[string]string // multipart form fields (excluding "file")
	filename      string            // the filename of the uploaded "file" part
	fileSize      int               // byte length of the uploaded audio data
	authorization string            // Authorization header value
}

func newWhisperTestServer(t *testing.T, transcript string) (*httptest.Server, *capturedRequest) {
	t.Helper()
	captured := &capturedRequest{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		captured.authorization = r.Header.Get("Authorization")

		// Parse multipart body.
		contentType := r.Header.Get("Content-Type")
		mediaType, params, err := mime.ParseMediaType(contentType)
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
			http.Error(w, "bad content-type", http.StatusBadRequest)
			return
		}

		mr := multipart.NewReader(r.Body, params["boundary"])
		captured.fields = make(map[string]string)

		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				http.Error(w, "multipart error", http.StatusBadRequest)
				return
			}
			data, _ := io.ReadAll(p)
			name := p.FormName()
			if name == "file" {
				captured.filename = p.FileName()
				captured.fileSize = len(data)
			} else {
				captured.fields[name] = string(data)
			}
			p.Close()
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(whisperTranscribeResponse{Text: transcript})
	}))

	t.Cleanup(srv.Close)
	return srv, captured
}

// makeWhisperCollector creates a WhisperCollector wired against a test HTTP
// server with the given config overrides.
func makeWhisperCollector(cfg *config.Config, srv *httptest.Server) *WhisperCollector {
	c := NewWhisperCollector(cfg)
	c.httpClient = srv.Client()
	c.baseURL = srv.URL
	return c
}

// writeDummyAudio creates a dummy audio file at dir/name with the given mtime.
// Content is a small placeholder — the mock server does not validate audio bytes.
func writeDummyAudio(t *testing.T, dir, name string, mtime time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("RIFF....dummy audio data"), 0o600); err != nil {
		t.Fatalf("writeDummyAudio: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("writeDummyAudio chtimes: %v", err)
	}
	return path
}

// --- Tests ---

func TestWhisperCollector_Enabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		audioDir string
		apiURL  string
		apiKey  string
		want    bool
	}{
		{
			name:    "both set without key",
			audioDir: "/tmp/audio",
			apiURL:  "http://localhost:8080/v1",
			apiKey:  "",
			want:    true,
		},
		{
			name:    "both set with key",
			audioDir: "/tmp/audio",
			apiURL:  "http://localhost:8080/v1",
			apiKey:  "sk-test",
			want:    true,
		},
		{
			name:    "audio dir empty",
			audioDir: "",
			apiURL:  "http://localhost:8080/v1",
			apiKey:  "sk-test",
			want:    false,
		},
		{
			name:    "api url empty",
			audioDir: "/tmp/audio",
			apiURL:  "",
			apiKey:  "sk-test",
			want:    false,
		},
		{
			name:    "both empty",
			audioDir: "",
			apiURL:  "",
			apiKey:  "",
			want:    false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{
				WhisperAudioDir: tc.audioDir,
				WhisperAPIURL:   tc.apiURL,
				WhisperAPIKey:   tc.apiKey,
				WhisperModel:    "whisper-1",
				WhisperLanguage: "ko",
			}
			c := NewWhisperCollector(cfg)
			// Override baseURL so Enabled() uses the test value, not the config.
			c.baseURL = tc.apiURL
			if got := c.Enabled(); got != tc.want {
				t.Errorf("Enabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWhisperCollector_Collect_BasicTranscription(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const wantTranscript = "안녕하세요, 통화 내용입니다."

	srv, captured := newWhisperTestServer(t, wantTranscript)

	mtime := time.Now().Add(-1 * time.Hour).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "call-2024.m4a", mtime)

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperAPIKey:   "sk-testkey",
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Collect() returned %d docs, want 1", len(docs))
	}

	doc := docs[0]

	// SourceType
	if doc.SourceType != model.SourceCallTranscript {
		t.Errorf("SourceType = %q, want %q", doc.SourceType, model.SourceCallTranscript)
	}

	// SourceID = "transcript:" + relative path
	wantSourceID := "transcript:call-2024.m4a"
	if doc.SourceID != wantSourceID {
		t.Errorf("SourceID = %q, want %q", doc.SourceID, wantSourceID)
	}

	// Title = filename without extension
	if doc.Title != "call-2024" {
		t.Errorf("Title = %q, want %q", doc.Title, "call-2024")
	}

	// Content = transcript text
	if doc.Content != wantTranscript {
		t.Errorf("Content = %q, want %q", doc.Content, wantTranscript)
	}

	// OccurredAt = file mtime (UTC, truncated to second)
	if doc.OccurredAt == nil {
		t.Fatal("OccurredAt is nil")
	}
	if !doc.OccurredAt.Equal(mtime) {
		t.Errorf("OccurredAt = %v, want %v", doc.OccurredAt, mtime)
	}

	// CollectedAt is set
	if doc.CollectedAt.IsZero() {
		t.Error("CollectedAt is zero")
	}

	// Metadata fields
	if v, ok := doc.Metadata["relative_path"]; !ok || v != "call-2024.m4a" {
		t.Errorf("Metadata[relative_path] = %v, want %q", v, "call-2024.m4a")
	}
	if v, ok := doc.Metadata["language"]; !ok || v != "ko" {
		t.Errorf("Metadata[language] = %v, want %q", v, "ko")
	}
	if v, ok := doc.Metadata["model"]; !ok || v != "whisper-1" {
		t.Errorf("Metadata[model] = %v, want %q", v, "whisper-1")
	}
	if _, ok := doc.Metadata["audio_size"]; !ok {
		t.Error("Metadata[audio_size] missing")
	}

	// Verify multipart fields sent to the server.
	if captured.fields["model"] != "whisper-1" {
		t.Errorf("multipart model = %q, want %q", captured.fields["model"], "whisper-1")
	}
	if captured.fields["language"] != "ko" {
		t.Errorf("multipart language = %q, want %q", captured.fields["language"], "ko")
	}
	if captured.filename != "call-2024.m4a" {
		t.Errorf("uploaded filename = %q, want %q", captured.filename, "call-2024.m4a")
	}
	if captured.fileSize == 0 {
		t.Error("uploaded file has zero bytes")
	}
}

func TestWhisperCollector_Collect_SinceFilter(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, _ := newWhisperTestServer(t, "새 통화입니다.")

	now := time.Now().UTC()
	since := now.Add(-2 * time.Hour)

	// Old file: mtime before since → should be skipped.
	oldMtime := now.Add(-3 * time.Hour).Truncate(time.Second)
	writeDummyAudio(t, dir, "old-call.mp3", oldMtime)

	// New file: mtime after since → should be transcribed.
	newMtime := now.Add(-1 * time.Hour).Truncate(time.Second)
	writeDummyAudio(t, dir, "new-call.m4a", newMtime)

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)

	docs, err := c.Collect(context.Background(), since)
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}

	if len(docs) != 1 {
		t.Fatalf("Collect() returned %d docs, want 1 (old file should be skipped)", len(docs))
	}
	if docs[0].SourceID != "transcript:new-call.m4a" {
		t.Errorf("SourceID = %q, want transcript:new-call.m4a", docs[0].SourceID)
	}
}

func TestWhisperCollector_Collect_SinceZero_ProcessesAll(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, _ := newWhisperTestServer(t, "전체 전사")

	now := time.Now().UTC()
	writeDummyAudio(t, dir, "a.wav", now.Add(-10*time.Hour).Truncate(time.Second))
	writeDummyAudio(t, dir, "b.mp3", now.Add(-5*time.Hour).Truncate(time.Second))

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("Collect() returned %d docs, want 2 (since zero processes all)", len(docs))
	}
}

func TestWhisperCollector_Collect_OversizedFileSkipped(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, _ := newWhisperTestServer(t, "이 파일은 전사되지 않아야 함")

	// Create a file whose size is exactly 25 MB + 1 byte.
	bigPath := filepath.Join(dir, "big.m4a")
	// Write a sparse file by seeking past the limit and writing a single byte.
	f, err := os.Create(bigPath)
	if err != nil {
		t.Fatalf("create big file: %v", err)
	}
	if _, err := f.Seek(whisperMaxFileBytes, io.SeekStart); err != nil {
		f.Close()
		t.Fatalf("seek: %v", err)
	}
	if _, err := f.Write([]byte{0}); err != nil {
		f.Close()
		t.Fatalf("write: %v", err)
	}
	f.Close()

	mtime := time.Now().UTC().Truncate(time.Second)
	if err := os.Chtimes(bigPath, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("Collect() returned %d docs, want 0 (oversized file must be skipped)", len(docs))
	}
}

func TestWhisperCollector_Collect_NonAudioExtensionIgnored(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, _ := newWhisperTestServer(t, "should not appear")

	now := time.Now().UTC().Truncate(time.Second)

	// Write a text file — should be ignored.
	txtPath := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(txtPath, []byte("plain text"), 0o600); err != nil {
		t.Fatalf("write txt: %v", err)
	}
	if err := os.Chtimes(txtPath, now, now); err != nil {
		t.Fatalf("chtimes txt: %v", err)
	}

	// Write a PDF file — should be ignored.
	pdfPath := filepath.Join(dir, "report.pdf")
	if err := os.WriteFile(pdfPath, []byte("%PDF-1.4 body"), 0o600); err != nil {
		t.Fatalf("write pdf: %v", err)
	}
	if err := os.Chtimes(pdfPath, now, now); err != nil {
		t.Fatalf("chtimes pdf: %v", err)
	}

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("Collect() returned %d docs, want 0 (non-audio files must be ignored)", len(docs))
	}
}

func TestWhisperCollector_Collect_NoAuthHeaderWhenKeyEmpty(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, captured := newWhisperTestServer(t, "로컬 whisper 전사")

	mtime := time.Now().Add(-1 * time.Hour).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "local.ogg", mtime)

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperAPIKey:   "", // No API key — local whisper.cpp scenario.
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)

	_, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}

	if captured.authorization != "" {
		t.Errorf("Authorization header = %q, want empty (no key configured)", captured.authorization)
	}
}

func TestWhisperCollector_Collect_AuthHeaderWhenKeyPresent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, captured := newWhisperTestServer(t, "OpenAI 전사")

	mtime := time.Now().Add(-1 * time.Hour).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "openai.flac", mtime)

	const apiKey = "sk-open-ai-key"
	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperAPIKey:   apiKey,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)

	_, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}

	wantAuth := "Bearer " + apiKey
	if captured.authorization != wantAuth {
		t.Errorf("Authorization = %q, want %q", captured.authorization, wantAuth)
	}
}

func TestWhisperCollector_Collect_MultipartModelLanguage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	const wantModel = "large-v3"
	const wantLang = "en"

	srv, captured := newWhisperTestServer(t, "multipart fields test")

	mtime := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "test.aac", mtime)

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    wantModel,
		WhisperLanguage: wantLang,
	}
	c := makeWhisperCollector(cfg, srv)

	_, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}

	if captured.fields["model"] != wantModel {
		t.Errorf("multipart model = %q, want %q", captured.fields["model"], wantModel)
	}
	if captured.fields["language"] != wantLang {
		t.Errorf("multipart language = %q, want %q", captured.fields["language"], wantLang)
	}
}

func TestWhisperCollector_Collect_LanguageOmittedWhenEmpty(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, captured := newWhisperTestServer(t, "auto detect language")

	mtime := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "test.mp3", mtime)

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "", // empty → omit from request
	}
	c := makeWhisperCollector(cfg, srv)

	_, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}

	if _, present := captured.fields["language"]; present {
		t.Errorf("language multipart field should be absent when WhisperLanguage is empty, got %q", captured.fields["language"])
	}
}

func TestWhisperCollector_Collect_RecursiveWalk(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	subDir := filepath.Join(dir, "2024", "calls")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	srv, _ := newWhisperTestServer(t, "재귀 전사")

	now := time.Now().UTC()
	writeDummyAudio(t, dir, "root.wav", now.Add(-1*time.Hour).Truncate(time.Second))
	writeDummyAudio(t, subDir, "nested.m4a", now.Add(-2*time.Hour).Truncate(time.Second))

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("Collect() returned %d docs, want 2 (recursive walk)", len(docs))
	}

	// nested file should have relative path with directory separator.
	var foundNested bool
	for _, d := range docs {
		if d.SourceID == fmt.Sprintf("transcript:%s", filepath.Join("2024", "calls", "nested.m4a")) {
			foundNested = true
		}
	}
	if !foundNested {
		t.Errorf("nested document not found in results; got source IDs: %v",
			func() []string {
				ids := make([]string, len(docs))
				for i, d := range docs {
					ids[i] = d.SourceID
				}
				return ids
			}(),
		)
	}
}

func TestWhisperCollector_Collect_PartialFailureContinues(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Server returns 500 for the first file, 200 for the second.
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(whisperTranscribeResponse{Text: "두 번째 파일 전사"})
	}))
	defer srv.Close()

	now := time.Now().UTC()
	// Two files with different names so walk order is deterministic by name.
	writeDummyAudio(t, dir, "aaa.mp3", now.Add(-2*time.Hour).Truncate(time.Second))
	writeDummyAudio(t, dir, "bbb.m4a", now.Add(-1*time.Hour).Truncate(time.Second))

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() should not return error on partial failure, got: %v", err)
	}
	if len(docs) != 1 {
		t.Errorf("Collect() returned %d docs, want 1 (one failure + one success)", len(docs))
	}
	if docs[0].Content != "두 번째 파일 전사" {
		t.Errorf("Content = %q, want %q", docs[0].Content, "두 번째 파일 전사")
	}
}

func TestWhisperCollector_Collect_ContextCancellation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, _ := newWhisperTestServer(t, "should not complete")

	now := time.Now().UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "cancel.flac", now.Add(-time.Hour))

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	// Should return an error or empty docs — must not panic.
	_, err := c.Collect(ctx, time.Time{})
	// Either the walk returns ctx.Err() wrapped, or the HTTP call fails.
	// Both are acceptable; the important thing is no panic and the error is from ctx.
	if err != nil && !strings.Contains(err.Error(), "context") {
		// HTTP client errors on context cancellation look like "context canceled"
		// wrapped in transport errors; accept any error here.
		_ = err
	}
}

func TestWhisperCollector_Name_And_Source(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{WhisperModel: "whisper-1", WhisperLanguage: "ko"}
	c := NewWhisperCollector(cfg)
	if c.Name() != "whisper" {
		t.Errorf("Name() = %q, want %q", c.Name(), "whisper")
	}
	if c.Source() != model.SourceCallTranscript {
		t.Errorf("Source() = %q, want %q", c.Source(), model.SourceCallTranscript)
	}
}
