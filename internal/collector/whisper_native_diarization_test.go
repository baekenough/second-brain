package collector

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/collector/smsmap"
	"github.com/baekenough/second-brain/internal/config"
)

// ---------------------------------------------------------------------------
// modelSupportsNativeDiarization unit tests
// ---------------------------------------------------------------------------

func TestModelSupportsNativeDiarization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		model string
		want  bool
	}{
		{"exact OpenAI native-diarize model", "gpt-4o-transcribe-diarize", true},
		{"case-insensitive match", "GPT-4O-TRANSCRIBE-DIARIZE", true},
		{"mixed case substring", "Gpt-4o-Transcribe-Diarize-Preview", true},
		{"legacy whisper-1 model", "whisper-1", false},
		{"local whisper.cpp large-v3", "large-v3", false},
		{"plain transcribe model without diarize", "gpt-4o-transcribe", false},
		{"empty model", "", false},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := modelSupportsNativeDiarization(tc.model); got != tc.want {
				t.Errorf("modelSupportsNativeDiarization(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// diarized_json request/response integration tests
// ---------------------------------------------------------------------------

// diarizedJSONServer returns an httptest.Server that responds to
// POST /audio/transcriptions with the given diarizedResponse, and captures
// the multipart form fields of the last request for assertion.
func diarizedJSONServer(t *testing.T, resp diarizedResponse) (*httptest.Server, *capturedRequest) {
	t.Helper()
	captured := &capturedRequest{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType := r.Header.Get("Content-Type")
		_, params, err := mime.ParseMediaType(contentType)
		if err != nil {
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
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}

// TestWhisperCollector_NativeDiarization_LabelledContent verifies that a
// native-diarizing model (gpt-4o-transcribe-diarize) produces speaker-labelled
// content via the SAME renderSpeakerBlocks formatter used by the pyannote
// path (labelTranscript calls it too, but the native path does not go through
// labelTranscript itself — see nativeDiarizedSegsToRawLabelled), without ever
// calling a separate diarization service, and tags the document with
// meta["diarization"]="native".
func TestWhisperCollector_NativeDiarization_LabelledContent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	srv, _ := diarizedJSONServer(t, diarizedResponse{
		Text: "안녕 거기서 전화해",
		Task: "transcribe",
		Segments: []diarizedSegment{
			{Type: "transcript.text.segment", Text: "안녕", Speaker: "A", Start: 0.0, End: 2.0, ID: "seg_0"},
			{Type: "transcript.text.segment", Text: "거기서 전화해", Speaker: "B", Start: 3.0, End: 5.0, ID: "seg_1"},
		},
	})

	// Diarization microservice must NOT be called for a native model.
	diarCalled := false
	diarSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		diarCalled = true
		http.Error(w, "must not be called for native diarization", http.StatusInternalServerError)
	}))
	defer diarSrv.Close()

	mtime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "call.m4a", mtime)

	cfg := &config.Config{
		WhisperAudioDir:         dir,
		WhisperAPIURL:           srv.URL,
		WhisperModel:            "gpt-4o-transcribe-diarize",
		WhisperLanguage:         "ko",
		WhisperChunkingStrategy: "auto",
		DiarizationEnabled:      true,
		DiarizationAPIURL:       diarSrv.URL,
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

	if !strings.Contains(doc.Content, "[화자1]") || !strings.Contains(doc.Content, "안녕") {
		t.Errorf("Content missing [화자1] 안녕 block; content: %q", doc.Content)
	}
	if !strings.Contains(doc.Content, "[화자2]") || !strings.Contains(doc.Content, "거기서 전화해") {
		t.Errorf("Content missing [화자2] block; content: %q", doc.Content)
	}
	if v, ok := doc.Metadata["speaker_count"]; !ok || v != 2 {
		t.Errorf("Metadata[speaker_count] = %v, want 2", v)
	}
	if v, ok := doc.Metadata["diarization"]; !ok || v != "native" {
		t.Errorf("Metadata[diarization] = %v, want %q", v, "native")
	}
	if diarCalled {
		t.Error("pyannote diarization microservice was called for a native-diarizing model")
	}
}

// TestWhisperCollector_NativeDiarization_IgnoresDiarizationEnabledFlag is the
// critical regression guard for the DiarizationEnabled gating bug: a
// native-diarizing model MUST request response_format=diarized_json (+
// chunking_strategy=auto) and produce [화자N]-labelled content with
// meta["diarization"]=="native" even when cfg.DiarizationEnabled=false (the
// default). DIARIZATION_ENABLED defaults to false, and whether audio leaves
// the machine is decided by WHISPER_API_URL + WHISPER_MODEL — not by this
// flag. Gating native diarization on it would mean paying the full
// cloud-transcription cost while silently discarding the speaker labels the
// model already computed as part of the same request: full privacy cost, zero
// benefit, no error. cfg.DiarizationEnabled retains its original meaning
// ONLY for the legacy pyannote path (non-native model + DiarizationAPIURL).
func TestWhisperCollector_NativeDiarization_IgnoresDiarizationEnabledFlag(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	srv, captured := diarizedJSONServer(t, diarizedResponse{
		Text: "안녕 거기서 전화해",
		Task: "transcribe",
		Segments: []diarizedSegment{
			{Type: "transcript.text.segment", Text: "안녕", Speaker: "A", Start: 0.0, End: 2.0, ID: "seg_0"},
			{Type: "transcript.text.segment", Text: "거기서 전화해", Speaker: "B", Start: 3.0, End: 5.0, ID: "seg_1"},
		},
	})

	mtime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "call.m4a", mtime)

	cfg := &config.Config{
		WhisperAudioDir:         dir,
		WhisperAPIURL:           srv.URL,
		WhisperModel:            "gpt-4o-transcribe-diarize",
		WhisperLanguage:         "ko",
		WhisperChunkingStrategy: "auto",
		DiarizationEnabled:      false, // default — must NOT gate native diarization
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

	// Request must still carry diarized_json + chunking_strategy=auto.
	if got := captured.fields["response_format"]; got != "diarized_json" {
		t.Errorf("response_format = %q, want %q even with DiarizationEnabled=false", got, "diarized_json")
	}
	if got := captured.fields["chunking_strategy"]; got != "auto" {
		t.Errorf("chunking_strategy = %q, want %q even with DiarizationEnabled=false", got, "auto")
	}

	// Content must still be speaker-labelled.
	if !strings.Contains(doc.Content, "[화자1]") || !strings.Contains(doc.Content, "안녕") {
		t.Errorf("Content missing [화자1] 안녕 block despite DiarizationEnabled=false; content: %q", doc.Content)
	}
	if !strings.Contains(doc.Content, "[화자2]") || !strings.Contains(doc.Content, "거기서 전화해") {
		t.Errorf("Content missing [화자2] block despite DiarizationEnabled=false; content: %q", doc.Content)
	}
	if v, ok := doc.Metadata["speaker_count"]; !ok || v != 2 {
		t.Errorf("Metadata[speaker_count] = %v, want 2 even with DiarizationEnabled=false", v)
	}
	if v, ok := doc.Metadata["diarization"]; !ok || v != "native" {
		t.Errorf("Metadata[diarization] = %v, want %q even with DiarizationEnabled=false", v, "native")
	}
}

// TestWhisperCollector_NativeDiarization_OverlappingSegmentsKeepOwnSpeaker is
// the regression guard for the re-alignment bug: native diarize segments must
// keep their OWN speaker label even when one segment's time interval fully
// contains another's. The API's diarized_json segments are authoritative per
// segment — there is no alignment step to perform, unlike the pyannote path
// where labelTranscript must align two independently-timestamped sequences
// by overlap.
//
// seg0 (speaker A, 0.0-10.0) fully contains seg1 (speaker B, 2.0-5.0) — a
// shape that is plausible at chunking_strategy=auto chunk boundaries. Routing
// these segments back through labelTranscript's overlap-based assignSpeaker
// would silently mis-attribute seg1's text to speaker A (the first segment
// with maximal — here, total — overlap, since assignSpeaker uses a strict
// `>` comparison and ties go to whichever segment was evaluated first),
// because assignSpeaker considers ALL diarSegs including seg1's own interval
// as "candidates" for seg0 and vice versa, rather than trusting seg1's own
// authoritative Speaker field. This test asserts seg1's text is correctly
// attributed to 화자2 (speaker B, its own label), not merged into 화자1.
func TestWhisperCollector_NativeDiarization_OverlappingSegmentsKeepOwnSpeaker(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	srv, _ := diarizedJSONServer(t, diarizedResponse{
		Text: "긴 발화 짧은 발화",
		Task: "transcribe",
		Segments: []diarizedSegment{
			// seg0 fully contains seg1's time interval.
			{Type: "transcript.text.segment", Text: "긴 발화", Speaker: "A", Start: 0.0, End: 10.0, ID: "seg_0"},
			{Type: "transcript.text.segment", Text: "짧은 발화", Speaker: "B", Start: 2.0, End: 5.0, ID: "seg_1"},
		},
	})

	mtime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "call.m4a", mtime)

	cfg := &config.Config{
		WhisperAudioDir:         dir,
		WhisperAPIURL:           srv.URL,
		WhisperModel:            "gpt-4o-transcribe-diarize",
		WhisperLanguage:         "ko",
		WhisperChunkingStrategy: "auto",
		DiarizationEnabled:      true,
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

	if v, ok := doc.Metadata["speaker_count"]; !ok || v != 2 {
		t.Fatalf("Metadata[speaker_count] = %v, want 2 (two distinct speakers, even though intervals overlap)", v)
	}

	// seg1's text must appear in an explicit [화자2] block of its own — it
	// must NOT be silently merged into [화자1]'s block (which would happen if
	// the overlap-based aligner mis-attributed it to speaker A).
	var found bool
	for _, line := range strings.Split(doc.Content, "\n") {
		if strings.HasPrefix(line, "[화자2]") && strings.Contains(line, "짧은 발화") {
			found = true
		}
		if strings.HasPrefix(line, "[화자1]") && strings.Contains(line, "짧은 발화") {
			t.Errorf("seg1 text mis-attributed to [화자1] (speaker A) instead of its own speaker B; line: %q", line)
		}
	}
	if !found {
		t.Errorf("expected a [화자2] block containing '짧은 발화' (seg1's own speaker); content: %q", doc.Content)
	}
}

// TestWhisperCollector_NativeDiarization_RequestFields verifies that the
// actual multipart request sent to a native-diarizing model carries
// response_format=diarized_json AND chunking_strategy=auto — both are
// required by the live OpenAI API for segments to be returned at all
// (ground truth: verified against the real API, see whisper.go doc comments).
func TestWhisperCollector_NativeDiarization_RequestFields(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	srv, captured := diarizedJSONServer(t, diarizedResponse{
		Text: "필드 검증",
		Segments: []diarizedSegment{
			{Text: "필드 검증", Speaker: "A", Start: 0.0, End: 2.0, ID: "seg_0"},
		},
	})

	mtime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "call.m4a", mtime)

	cfg := &config.Config{
		WhisperAudioDir:         dir,
		WhisperAPIURL:           srv.URL,
		WhisperModel:            "gpt-4o-transcribe-diarize",
		WhisperLanguage:         "ko",
		WhisperChunkingStrategy: "auto",
		DiarizationEnabled:      true,
		DiarizationAPIURL:       "http://unused.invalid", // never called for native models
	}
	c := makeWhisperCollector(cfg, srv)

	_, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}

	if got := captured.fields["response_format"]; got != "diarized_json" {
		t.Errorf("response_format = %q, want %q", got, "diarized_json")
	}
	if got := captured.fields["chunking_strategy"]; got != "auto" {
		t.Errorf("chunking_strategy = %q, want %q", got, "auto")
	}
	if got := captured.fields["model"]; got != "gpt-4o-transcribe-diarize" {
		t.Errorf("model = %q, want %q", got, "gpt-4o-transcribe-diarize")
	}
}

// TestWhisperCollector_NativeDiarization_ChunkingStrategyOmittedWhenEmpty
// verifies that setting WhisperChunkingStrategy to the empty string omits the
// field entirely (escape hatch documented in config.go), rather than sending
// chunking_strategy="".
func TestWhisperCollector_NativeDiarization_ChunkingStrategyOmittedWhenEmpty(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	srv, captured := diarizedJSONServer(t, diarizedResponse{
		Text: "청킹 전략 생략",
		Segments: []diarizedSegment{
			{Text: "청킹 전략 생략", Speaker: "A", Start: 0.0, End: 2.0, ID: "seg_0"},
		},
	})

	mtime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "call.m4a", mtime)

	cfg := &config.Config{
		WhisperAudioDir:         dir,
		WhisperAPIURL:           srv.URL,
		WhisperModel:            "gpt-4o-transcribe-diarize",
		WhisperLanguage:         "ko",
		WhisperChunkingStrategy: "", // explicit escape hatch — omit the field
		DiarizationEnabled:      true,
	}
	c := makeWhisperCollector(cfg, srv)

	_, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}

	if v, present := captured.fields["chunking_strategy"]; present {
		t.Errorf("chunking_strategy field present when WhisperChunkingStrategy is empty: %q", v)
	}
}

// TestWhisperCollector_NonNativeModel_LegacyRequestShape_NeverSendsChunkingStrategy
// is a regression guard: a non-native model (local whisper.cpp) with
// DiarizationEnabled=true must still send the legacy verbose_json request
// shape and must NEVER send chunking_strategy or diarized_json — those are
// native-model-only fields/formats.
func TestWhisperCollector_NonNativeModel_LegacyRequestShape_NeverSendsChunkingStrategy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	var capturedFields map[string]string
	whisperSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType := r.Header.Get("Content-Type")
		_, params, _ := mime.ParseMediaType(contentType)
		mr := multipart.NewReader(r.Body, params["boundary"])
		capturedFields = make(map[string]string)
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				break
			}
			data, _ := io.ReadAll(p)
			if p.FormName() != "file" {
				capturedFields[p.FormName()] = string(data)
			}
			p.Close()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(whisperVerboseResponse{
			Text: "로컬 whisper 전사",
			Segments: []whisperSegment{
				{Start: 0.0, End: 3.0, Text: "로컬 whisper 전사"},
			},
		})
	}))
	defer whisperSrv.Close()

	diarSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(diarizeResponse{
			Segments: []diarSegment{{Start: 0.0, End: 3.0, Speaker: "SPEAKER_00"}},
		})
	}))
	defer diarSrv.Close()

	mtime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "call.m4a", mtime)

	cfg := &config.Config{
		WhisperAudioDir:         dir,
		WhisperAPIURL:           whisperSrv.URL,
		WhisperModel:            "whisper-1", // NOT a native-diarizing model
		WhisperLanguage:         "ko",
		WhisperChunkingStrategy: "auto", // set, but must still be omitted for non-native models
		DiarizationEnabled:      true,
		DiarizationAPIURL:       diarSrv.URL,
	}
	c := makeWhisperCollector(cfg, whisperSrv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Collect() returned %d docs, want 1", len(docs))
	}

	if got := capturedFields["response_format"]; got != "verbose_json" {
		t.Errorf("response_format = %q, want %q (legacy shape)", got, "verbose_json")
	}
	if v, present := capturedFields["chunking_strategy"]; present {
		t.Errorf("chunking_strategy field present for non-native model: %q — must never be sent", v)
	}
	if v, ok := docs[0].Metadata["diarization"]; !ok || v != "pyannote" {
		t.Errorf("Metadata[diarization] = %v, want %q (pyannote path)", v, "pyannote")
	}
}

// TestWhisperCollector_NativeDiarization_EmptySegmentsFallsBackToText verifies
// that a diarized_json response with zero segments (observed in practice when
// chunking_strategy is missing/misconfigured, or for very short clips) does
// not crash the collector and instead falls back to the flat transcript text,
// with no speaker_count/diarization metadata set.
func TestWhisperCollector_NativeDiarization_EmptySegmentsFallsBackToText(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const wantText = "세그먼트 없는 다이어라이즈 응답"

	srv, _ := diarizedJSONServer(t, diarizedResponse{
		Text:     wantText,
		Task:     "transcribe",
		Segments: nil, // empty — must fall back, not crash
	})

	mtime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "call.m4a", mtime)

	cfg := &config.Config{
		WhisperAudioDir:         dir,
		WhisperAPIURL:           srv.URL,
		WhisperModel:            "gpt-4o-transcribe-diarize",
		WhisperLanguage:         "ko",
		WhisperChunkingStrategy: "auto",
		DiarizationEnabled:      true,
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

	if doc.Content != wantText {
		t.Errorf("Content = %q, want flat fallback %q", doc.Content, wantText)
	}
	if _, ok := doc.Metadata["speaker_count"]; ok {
		t.Error("speaker_count must not be set when diarized_json returned zero segments")
	}
	if _, ok := doc.Metadata["diarization"]; ok {
		t.Error("diarization metadata must not be set when diarized_json returned zero segments")
	}
}

// ---------------------------------------------------------------------------
// 25 MiB cloud file-size cap
// ---------------------------------------------------------------------------

// rewriteTransport routes all outbound requests to target while leaving the
// request's original URL (as constructed by transcribeFile from
// cfg.WhisperAPIURL) untouched for isLocalWhisperEndpoint's classification.
// This lets tests use a deliberately non-resolvable "cloud" hostname for
// cfg.WhisperAPIURL (so the collector treats the endpoint as non-local) while
// still routing the actual HTTP traffic to a local httptest.Server.
type rewriteTransport struct {
	target *url.URL
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.URL.Scheme = rt.target.Scheme
	out.URL.Host = rt.target.Host
	out.Host = rt.target.Host
	return http.DefaultTransport.RoundTrip(out)
}

// makeCloudWhisperCollector builds a WhisperCollector whose baseURL resolves
// to a non-local, non-resolvable hostname (".test" TLD — reserved by RFC 2606
// to never resolve, per isLocalWhisperEndpoint's DNS-resolution-failure ==
// non-local rule) while actually routing requests to srv.
func makeCloudWhisperCollector(t *testing.T, cfg *config.Config, srv *httptest.Server) *WhisperCollector {
	t.Helper()
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse srv.URL: %v", err)
	}
	const cloudBaseURL = "https://cloud.example.test"
	cfg.WhisperAPIURL = cloudBaseURL
	c := NewWhisperCollector(cfg)
	c.baseURL = cloudBaseURL
	c.httpClient = &http.Client{Transport: &rewriteTransport{target: target}}
	return c
}

// writeSparseM4A creates a valid-header, sparse (mostly-zero-filled-on-disk)
// .m4a file of exactly sizeBytes, so multi-megabyte test fixtures do not
// consume real disk I/O time or space. A valid ISOBMFF "ftyp" box is written
// at the start so the file would pass the audio integrity pre-check if it
// were not skipped earlier by the cloud size cap.
func writeSparseM4A(t *testing.T, dir, name string, sizeBytes int64, mtime time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	header := make([]byte, 32)
	copy(header[4:8], "ftyp")
	if _, err := f.Write(header); err != nil {
		f.Close()
		t.Fatalf("write header: %v", err)
	}
	if _, err := f.Seek(sizeBytes-1, io.SeekStart); err != nil {
		f.Close()
		t.Fatalf("seek: %v", err)
	}
	if _, err := f.Write([]byte{0}); err != nil {
		f.Close()
		t.Fatalf("write trailing byte: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	return path
}

// TestWhisperCollector_CloudEndpoint_OversizeFileSkipped verifies that,
// against a non-local (cloud) endpoint, a file over the 25 MiB hard API limit
// is skipped (no HTTP call, no Document, no quarantine) while the walk
// continues and a normal-size file in the same directory is still
// transcribed.
func TestWhisperCollector_CloudEndpoint_OversizeFileSkipped(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const wantTranscript = "정상 크기 파일 전사"

	serverCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverCalls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(whisperTranscribeResponse{Text: wantTranscript})
	}))
	defer srv.Close()

	now := time.Now().UTC().Truncate(time.Second)

	// Oversized file: 26 MiB > 25 MiB cloud cap. Sorted before the small file
	// alphabetically so the walk visits it first (regression guard: an early
	// skip must not abort the walk for subsequent files).
	writeSparseM4A(t, dir, "aaa_oversized.m4a", 26<<20, now)

	// Normal-size file: well under the cap, must still be transcribed.
	smallPath := filepath.Join(dir, "bbb_normal.m4a")
	smallData := make([]byte, 64)
	copy(smallData[4:8], "ftyp")
	if err := os.WriteFile(smallPath, smallData, 0o600); err != nil {
		t.Fatalf("write small file: %v", err)
	}
	if err := os.Chtimes(smallPath, now, now); err != nil {
		t.Fatalf("chtimes small file: %v", err)
	}

	cfg := &config.Config{
		WhisperAudioDir: dir,
		WhisperModel:    "whisper-1",
		WhisperLanguage: "ko",
	}
	c := makeCloudWhisperCollector(t, cfg, srv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Collect() returned %d docs, want 1 (oversized file skipped, normal file transcribed)", len(docs))
	}
	if docs[0].SourceID != "transcript:bbb_normal.m4a" {
		t.Errorf("SourceID = %q, want transcript:bbb_normal.m4a", docs[0].SourceID)
	}
	if docs[0].Content != wantTranscript {
		t.Errorf("Content = %q, want %q", docs[0].Content, wantTranscript)
	}
	if serverCalls != 1 {
		t.Errorf("server called %d times, want 1 (oversized file must never reach the HTTP endpoint)", serverCalls)
	}
}

// TestWhisperCollector_LocalEndpoint_OversizeFileNotSubjectToCloudCap verifies
// that the 25 MiB cloud cap does NOT apply to a local endpoint — only the
// configurable WhisperMaxFileBytes (which defaults to unlimited when unset in
// tests) governs local deployments.
func TestWhisperCollector_LocalEndpoint_OversizeFileNotSubjectToCloudCap(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srv, _ := newWhisperTestServer(t, "로컬 대용량 파일 전사")

	now := time.Now().UTC().Truncate(time.Second)
	// 26 MiB — over the cloud cap, but the endpoint here is local (srv.URL is
	// 127.0.0.1), so the cloud cap must not apply.
	writeSparseM4A(t, dir, "huge_local.m4a", 26<<20, now)

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
		t.Errorf("Collect() returned %d docs, want 1 (local endpoint is exempt from the 25 MiB cloud cap)", len(docs))
	}
}

// ---------------------------------------------------------------------------
// WhisperCloudAllowed guard log-level behaviour (smoke test — behaviour is
// observable only via log output, so this test asserts the non-blocking
// contract: Collect() must succeed identically regardless of the flag).
// ---------------------------------------------------------------------------

func TestWhisperCollector_CloudAllowedFlag_DoesNotBlockCollection(t *testing.T) {
	t.Parallel()

	for _, cloudAllowed := range []bool{true, false} {
		cloudAllowed := cloudAllowed
		t.Run(map[bool]string{true: "allowed", false: "not_allowed"}[cloudAllowed], func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			const wantTranscript = "클라우드 허용 플래그 무관 정상 동작"
			srv, _ := newWhisperTestServer(t, wantTranscript)

			mtime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
			writeDummyAudio(t, dir, "call.m4a", mtime)

			cfg := &config.Config{
				WhisperAudioDir:     dir,
				WhisperModel:        "whisper-1",
				WhisperLanguage:     "ko",
				WhisperCloudAllowed: cloudAllowed,
			}
			c := makeCloudWhisperCollector(t, cfg, srv)

			docs, err := c.Collect(context.Background(), time.Time{})
			if err != nil {
				t.Fatalf("Collect() error: %v", err)
			}
			if len(docs) != 1 || docs[0].Content != wantTranscript {
				t.Errorf("Collect() = %+v, want 1 doc with content %q regardless of WhisperCloudAllowed=%v",
					docs, wantTranscript, cloudAllowed)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// PII redaction pin for native-diarized content (highest-value test in this
// change — a regression here writes unredacted call content to the DB).
// ---------------------------------------------------------------------------

// TestWhisperCollector_NativeDiarization_RedactsPII verifies that a native
// diarized_json transcript carrying a Korean phone number in one speaker's
// segment and a plausible RRN in another's comes out fully redacted in
// Document.Content, with the [화자N] labels intact. buildDocument applies
// smsmap.RedactPII to `content` AFTER the diarization switch (see
// buildDocument), so this pins that the native branch assigns its rendered,
// speaker-labelled text to that SAME content variable rather than bypassing
// redaction via some other path.
func TestWhisperCollector_NativeDiarization_RedactsPII(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// 900101-1234567: YYMMDD=900101 (plausible date) + gender digit '1' (1-8)
	// — satisfies redactRRNIfPlausible's date/gender-digit checks.
	srv, _ := diarizedJSONServer(t, diarizedResponse{
		Text: "제 번호는 010-1234-5678 입니다 주민번호는 900101-1234567 이에요",
		Task: "transcribe",
		Segments: []diarizedSegment{
			{Type: "transcript.text.segment", Text: "제 번호는 010-1234-5678 입니다", Speaker: "A", Start: 0.0, End: 3.0, ID: "seg_0"},
			{Type: "transcript.text.segment", Text: "주민번호는 900101-1234567 이에요", Speaker: "B", Start: 3.5, End: 6.0, ID: "seg_1"},
		},
	})

	mtime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "call-native-pii.m4a", mtime)

	cfg := &config.Config{
		WhisperAudioDir:         dir,
		WhisperAPIURL:           srv.URL,
		WhisperModel:            "gpt-4o-transcribe-diarize",
		WhisperLanguage:         "ko",
		WhisperChunkingStrategy: "auto",
	}
	c := makeWhisperCollector(cfg, srv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Collect() returned %d docs, want 1", len(docs))
	}
	content := docs[0].Content

	if strings.Contains(content, "010-1234-5678") {
		t.Errorf("Content = %q, phone number was not redacted", content)
	}
	if strings.Contains(content, "900101-1234567") {
		t.Errorf("Content = %q, RRN was not redacted", content)
	}
	if !strings.Contains(content, smsmap.PIIRedactionToken) {
		t.Errorf("Content = %q, want %q marker present", content, smsmap.PIIRedactionToken)
	}

	// Speaker labels must survive redaction untouched.
	if !strings.Contains(content, "[화자1]") {
		t.Errorf("Content missing [화자1] label after redaction; content: %q", content)
	}
	if !strings.Contains(content, "[화자2]") {
		t.Errorf("Content missing [화자2] label after redaction; content: %q", content)
	}

	if v, ok := docs[0].Metadata["diarization"]; !ok || v != "native" {
		t.Errorf("Metadata[diarization] = %v, want %q", v, "native")
	}

	// Dedup identity (SourceID) must be unaffected by content redaction.
	if docs[0].SourceID != "transcript:call-native-pii.m4a" {
		t.Errorf("SourceID = %q, want %q (must not be affected by redaction)",
			docs[0].SourceID, "transcript:call-native-pii.m4a")
	}
}
