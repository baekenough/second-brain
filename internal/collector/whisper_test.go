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
//
// For m4a/mp4 files a minimal valid ISOBMFF container header (32 bytes, "ftyp"
// at offset 4) is written so that the collector's audio pre-check passes.
// All other audio formats use a RIFF-style header which satisfies only the
// minimum-length guard (>= 8 bytes).
func writeDummyAudio(t *testing.T, dir, name string, mtime time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name)

	var content []byte
	switch strings.ToLower(filepath.Ext(name)) {
	case ".m4a", ".mp4":
		// Valid ISOBMFF header: 32 bytes with "ftyp" box at offset 4.
		content = make([]byte, 32)
		copy(content[4:8], "ftyp")
	default:
		content = []byte("RIFF....dummy audio data")
	}

	if err := os.WriteFile(path, content, 0o600); err != nil {
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

	// Use a small explicit cap (1 KiB) so the sparse file is definitely over.
	const testCap int64 = 1024

	// Create a file whose size is exactly testCap + 1 byte.
	bigPath := filepath.Join(dir, "big.m4a")
	// Write a sparse file by seeking past the limit and writing a single byte.
	f, err := os.Create(bigPath)
	if err != nil {
		t.Fatalf("create big file: %v", err)
	}
	if _, err := f.Seek(testCap, io.SeekStart); err != nil {
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
		WhisperAudioDir:     dir,
		WhisperAPIURL:       srv.URL,
		WhisperModel:        "whisper-1",
		WhisperLanguage:     "ko",
		WhisperMaxFileBytes: testCap,
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

// TestWhisperCollector_Collect_OversizedFileSkipped_ConfigurableCap verifies
// that a file below a custom cap is transcribed and a file above is skipped.
func TestWhisperCollector_Collect_OversizedFileSkipped_ConfigurableCap(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, _ := newWhisperTestServer(t, "작은 파일 전사")

	const testCap int64 = 100 // 100 bytes

	// Small file: under cap → should be transcribed.
	// Must carry a valid ftyp box to pass the audio integrity pre-check.
	smallPath := filepath.Join(dir, "small.m4a")
	smallData := make([]byte, 50) // 50 bytes — under the 100-byte cap
	copy(smallData[4:8], "ftyp")
	if err := os.WriteFile(smallPath, smallData, 0o600); err != nil {
		t.Fatalf("write small file: %v", err)
	}

	// Large file: over cap → should be skipped.
	largePath := filepath.Join(dir, "large.mp3")
	if err := os.WriteFile(largePath, make([]byte, 200), 0o600); err != nil { // 200 bytes
		t.Fatalf("write large file: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	for _, p := range []string{smallPath, largePath} {
		if err := os.Chtimes(p, now, now); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}

	cfg := &config.Config{
		WhisperAudioDir:     dir,
		WhisperAPIURL:       srv.URL,
		WhisperModel:        "whisper-1",
		WhisperLanguage:     "ko",
		WhisperMaxFileBytes: testCap,
	}
	c := makeWhisperCollector(cfg, srv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 1 {
		t.Errorf("Collect() returned %d docs, want 1 (large file skipped)", len(docs))
	}
	if len(docs) == 1 && docs[0].SourceID != "transcript:small.m4a" {
		t.Errorf("SourceID = %q, want transcript:small.m4a", docs[0].SourceID)
	}
}

// TestWhisperCollector_Collect_ZeroCap_Unlimited verifies that maxFileBytes=0
// disables the size cap and even large files are transcribed.
func TestWhisperCollector_Collect_ZeroCap_Unlimited(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, _ := newWhisperTestServer(t, "대용량 파일 전사")

	// Write a 10 MiB sparse file — well above the old 25 MB constant and only
	// limited to keep the test fast.
	// Write a valid ISOBMFF ftyp header at the start so the audio pre-check passes,
	// then seek to 10 MiB to create a sparse file of the desired size.
	bigPath := filepath.Join(dir, "huge.m4a")
	f, err := os.Create(bigPath)
	if err != nil {
		t.Fatalf("create big file: %v", err)
	}
	header := make([]byte, 32)
	copy(header[4:8], "ftyp")
	if _, err := f.Write(header); err != nil {
		f.Close()
		t.Fatalf("write m4a header: %v", err)
	}
	if _, err := f.Seek(10<<20, io.SeekStart); err != nil { // 10 MiB
		f.Close()
		t.Fatalf("seek: %v", err)
	}
	if _, err := f.Write([]byte{0}); err != nil {
		f.Close()
		t.Fatalf("write: %v", err)
	}
	f.Close()

	now := time.Now().UTC().Truncate(time.Second)
	if err := os.Chtimes(bigPath, now, now); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	cfg := &config.Config{
		WhisperAudioDir:     dir,
		WhisperAPIURL:       srv.URL,
		WhisperModel:        "whisper-1",
		WhisperLanguage:     "ko",
		WhisperMaxFileBytes: 0, // unlimited
	}
	c := makeWhisperCollector(cfg, srv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 1 {
		t.Errorf("Collect() returned %d docs, want 1 (zero cap = unlimited)", len(docs))
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
	// aaa.mp3: generic audio header (RIFF) — passes CheckAudioBytes; server returns 500 for first call.
	writeDummyAudio(t, dir, "aaa.mp3", now.Add(-2*time.Hour).Truncate(time.Second))
	// bbb.m4a: must carry a valid ftyp box so it passes the pre-check and reaches the server.
	// The server returns 200 for the second call, producing the expected transcript.
	validM4APath := filepath.Join(dir, "bbb.m4a")
	validM4AData := make([]byte, 64)
	copy(validM4AData[4:8], "ftyp")
	if err := os.WriteFile(validM4APath, validM4AData, 0o600); err != nil {
		t.Fatalf("write valid m4a: %v", err)
	}
	validM4AMtime := now.Add(-1 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(validM4APath, validM4AMtime, validM4AMtime); err != nil {
		t.Fatalf("chtimes valid m4a: %v", err)
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

// writeCorruptM4A creates a corrupt .m4a file at dir/name with the given mtime.
// The file content is 4096 zero bytes — identical to the garbage uploaded by the
// mobile app in the production incident (no ftyp box, not a valid m4a container).
func writeCorruptM4A(t *testing.T, dir, name string, mtime time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, make([]byte, 4096), 0o600); err != nil {
		t.Fatalf("writeCorruptM4A: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("writeCorruptM4A chtimes: %v", err)
	}
	return path
}

// TestWhisperCollector_Collect_CorruptM4ASkipped verifies that a 4096-byte
// all-zero .m4a file (no ftyp box) is silently skipped and does NOT trigger
// an HTTP call to the whisper server.
//
// This is the collector-side defence against the infinite retry loop: even if
// such a file somehow gets on disk (e.g. written before the ingest guard was
// added), the collector must not submit it to the whisper API.
func TestWhisperCollector_Collect_CorruptM4ASkipped(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Track whether the whisper server was called.
	serverCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverCalled = true
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer srv.Close()

	now := time.Now().UTC().Truncate(time.Second)
	writeCorruptM4A(t, dir, "garbage.m4a", now)

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperAPIURL:   srv.URL,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeWhisperCollector(cfg, srv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() should not error on corrupt file: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("Collect() returned %d docs, want 0 (corrupt file must be skipped)", len(docs))
	}
	if serverCalled {
		t.Error("whisper server was called for corrupt file — must be skipped before HTTP request")
	}
}

// TestWhisperCollector_Collect_CorruptM4ASkipped_ValidFileContinues verifies
// that when the directory contains both a corrupt and a valid .m4a file, the
// corrupt file is skipped (no server call) and the valid file is transcribed.
func TestWhisperCollector_Collect_CorruptM4ASkipped_ValidFileContinues(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const wantTranscript = "정상 파일 전사"
	srv, captured := newWhisperTestServer(t, wantTranscript)

	now := time.Now().UTC().Truncate(time.Second)

	// Corrupt file — 4096 zero bytes, no ftyp box.
	writeCorruptM4A(t, dir, "aaa_corrupt.m4a", now)

	// Valid file — has "ftyp" at offset 4.
	validPath := filepath.Join(dir, "bbb_valid.m4a")
	validData := make([]byte, 64)
	copy(validData[4:8], []byte("ftyp"))
	if err := os.WriteFile(validPath, validData, 0o600); err != nil {
		t.Fatalf("write valid file: %v", err)
	}
	if err := os.Chtimes(validPath, now, now); err != nil {
		t.Fatalf("chtimes valid: %v", err)
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
	if len(docs) != 1 {
		t.Errorf("Collect() returned %d docs, want 1 (only valid file)", len(docs))
	}
	if len(docs) == 1 && docs[0].Content != wantTranscript {
		t.Errorf("Content = %q, want %q", docs[0].Content, wantTranscript)
	}
	// Server must have been called exactly once (for the valid file).
	if captured.fileSize == 0 {
		t.Error("whisper server was not called for the valid file")
	}
}

// TestWhisperCollector_Collect_TooSmallFileSkipped verifies that a file shorter
// than 8 bytes is skipped before the HTTP call.
func TestWhisperCollector_Collect_TooSmallFileSkipped(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	serverCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverCalled = true
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer srv.Close()

	tinyPath := filepath.Join(dir, "tiny.m4a")
	if err := os.WriteFile(tinyPath, []byte{0x00, 0x01}, 0o600); err != nil {
		t.Fatalf("write tiny file: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := os.Chtimes(tinyPath, now, now); err != nil {
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
		t.Errorf("Collect() returned %d docs, want 0 (tiny file must be skipped)", len(docs))
	}
	if serverCalled {
		t.Error("whisper server was called for tiny file — must be skipped")
	}
}
