package collector

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/config"
)

// ---------------------------------------------------------------------------
// labelTranscript unit tests
// ---------------------------------------------------------------------------

// TestLabelTranscript_Clean2SpeakerAlternation verifies the happy path: two
// speakers alternating cleanly, each whisper segment overlapping exactly one
// diarization segment.
func TestLabelTranscript_Clean2SpeakerAlternation(t *testing.T) {
	t.Parallel()

	whisperSegs := []whisperSegment{
		{Start: 0.0, End: 2.0, Text: "안녕하세요"},
		{Start: 2.5, End: 4.5, Text: "반갑습니다"},
		{Start: 5.0, End: 7.0, Text: "네 잘 부탁드립니다"},
		{Start: 7.5, End: 9.5, Text: "저도요"},
	}
	diarSegs := []diarSegment{
		{Start: 0.0, End: 2.5, Speaker: "SPEAKER_00"},
		{Start: 2.5, End: 5.0, Speaker: "SPEAKER_01"},
		{Start: 5.0, End: 7.5, Speaker: "SPEAKER_00"},
		{Start: 7.5, End: 10.0, Speaker: "SPEAKER_01"},
	}

	content, nSpeakers := labelTranscript(whisperSegs, diarSegs)

	if nSpeakers != 2 {
		t.Errorf("speakerCount = %d, want 2", nSpeakers)
	}

	// Expect alternating blocks: 화자1, 화자2, 화자1, 화자2 (no consecutive merge)
	lines := strings.Split(content, "\n")
	if len(lines) != 4 {
		t.Errorf("line count = %d, want 4; content:\n%s", len(lines), content)
	}

	wantPrefixes := []string{"[화자1]", "[화자2]", "[화자1]", "[화자2]"}
	for i, line := range lines {
		if i >= len(wantPrefixes) {
			break
		}
		if !strings.HasPrefix(line, wantPrefixes[i]) {
			t.Errorf("line[%d] = %q, want prefix %q", i, line, wantPrefixes[i])
		}
	}
}

// TestLabelTranscript_ConsecutiveSameSpeaker verifies that consecutive whisper
// segments assigned to the same speaker are merged into one block.
func TestLabelTranscript_ConsecutiveSameSpeaker(t *testing.T) {
	t.Parallel()

	whisperSegs := []whisperSegment{
		{Start: 0.0, End: 2.0, Text: "첫 번째"},
		{Start: 2.0, End: 4.0, Text: "두 번째"},   // same speaker → merge
		{Start: 4.5, End: 6.5, Text: "세 번째"},   // different speaker
	}
	diarSegs := []diarSegment{
		{Start: 0.0, End: 4.1, Speaker: "SPEAKER_00"},
		{Start: 4.1, End: 7.0, Speaker: "SPEAKER_01"},
	}

	content, nSpeakers := labelTranscript(whisperSegs, diarSegs)

	if nSpeakers != 2 {
		t.Errorf("speakerCount = %d, want 2", nSpeakers)
	}

	lines := strings.Split(content, "\n")
	if len(lines) != 2 {
		t.Errorf("line count = %d, want 2 (two merged blocks); content:\n%s", len(lines), content)
	}

	// First block should contain both first and second text.
	if !strings.Contains(lines[0], "첫 번째") || !strings.Contains(lines[0], "두 번째") {
		t.Errorf("first block %q should contain both merged texts", lines[0])
	}
}

// TestLabelTranscript_MaxOverlapWins verifies that when a whisper segment
// overlaps two diarization segments, the one with the larger overlap wins.
func TestLabelTranscript_MaxOverlapWins(t *testing.T) {
	t.Parallel()

	// whisper segment [1.0, 4.0] overlaps:
	//   SPEAKER_00 [0.0, 2.5] → overlap = 2.5-1.0 = 1.5s
	//   SPEAKER_01 [2.5, 5.0] → overlap = 4.0-2.5 = 1.5s  (tie → first wins)
	// whisper segment [2.0, 5.0] overlaps:
	//   SPEAKER_00 [0.0, 2.5] → overlap = 2.5-2.0 = 0.5s
	//   SPEAKER_01 [2.5, 6.0] → overlap = 5.0-2.5 = 2.5s  (SPEAKER_01 wins)
	whisperSegs := []whisperSegment{
		{Start: 1.0, End: 4.0, Text: "overlap-tie"},
		{Start: 2.0, End: 5.0, Text: "speaker01-wins"},
	}
	diarSegs := []diarSegment{
		{Start: 0.0, End: 2.5, Speaker: "SPEAKER_00"},
		{Start: 2.5, End: 6.0, Speaker: "SPEAKER_01"},
	}

	content, nSpeakers := labelTranscript(whisperSegs, diarSegs)

	if nSpeakers < 1 {
		t.Errorf("speakerCount = %d, want >= 1", nSpeakers)
	}

	// The second whisper segment must be assigned to 화자2 (SPEAKER_01).
	lines := strings.Split(content, "\n")
	var found bool
	for _, line := range lines {
		if strings.HasPrefix(line, "[화자2]") && strings.Contains(line, "speaker01-wins") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected [화자2] block containing 'speaker01-wins'; content:\n%s", content)
	}
}

// TestLabelTranscript_NoOverlapFallsBackToNearest verifies that a whisper
// segment with no overlapping diarization segment is assigned to the nearest
// speaker by midpoint distance.
//
// Two whisper segments are used so that speaker-numbering is deterministic:
//   - segment 0 [0.0, 2.0] overlaps SPEAKER_00 → 화자1 (first appearance)
//   - segment 1 [20.0, 21.0] has no overlap; nearest is SPEAKER_01 → 화자2
//
// This guarantees the second speaker (SPEAKER_01) is labelled 화자2 regardless
// of the order of entries in diarSegs.
func TestLabelTranscript_NoOverlapFallsBackToNearest(t *testing.T) {
	t.Parallel()

	whisperSegs := []whisperSegment{
		{Start: 0.0, End: 2.0, Text: "첫 발화"},    // overlaps SPEAKER_00 → 화자1
		{Start: 20.0, End: 21.0, Text: "늦은 세그먼트"}, // no overlap; nearest = SPEAKER_01 → 화자2
	}
	// SPEAKER_00 [0.0, 5.0] midpoint 2.5,   dist from ws[1] midpoint 20.5 = 18.0
	// SPEAKER_01 [18.0, 19.5] midpoint 18.75, dist from ws[1] midpoint 20.5 = 1.75 → nearest
	diarSegs := []diarSegment{
		{Start: 0.0, End: 5.0, Speaker: "SPEAKER_00"},
		{Start: 18.0, End: 19.5, Speaker: "SPEAKER_01"},
	}

	content, nSpeakers := labelTranscript(whisperSegs, diarSegs)

	if nSpeakers != 2 {
		t.Errorf("speakerCount = %d, want 2", nSpeakers)
	}

	// Nearest for segment 1 is SPEAKER_01 → 화자2 (second appearance in assignment order).
	if !strings.Contains(content, "[화자2]") {
		t.Errorf("expected [화자2] (nearest SPEAKER_01); content: %q", content)
	}
	if !strings.Contains(content, "늦은 세그먼트") {
		t.Errorf("expected 'late segment' text in content; content: %q", content)
	}
}

// TestLabelTranscript_SingleSpeaker verifies that a single speaker throughout
// the recording produces one block and speakerCount=1.
func TestLabelTranscript_SingleSpeaker(t *testing.T) {
	t.Parallel()

	whisperSegs := []whisperSegment{
		{Start: 0.0, End: 2.0, Text: "좋아"},
		{Start: 2.5, End: 4.5, Text: "알겠어"},
		{Start: 5.0, End: 6.0, Text: "맞아"},
	}
	diarSegs := []diarSegment{
		{Start: 0.0, End: 6.5, Speaker: "SPEAKER_00"},
	}

	content, nSpeakers := labelTranscript(whisperSegs, diarSegs)

	if nSpeakers != 1 {
		t.Errorf("speakerCount = %d, want 1", nSpeakers)
	}

	lines := strings.Split(content, "\n")
	if len(lines) != 1 {
		t.Errorf("line count = %d, want 1 (all same speaker → one block); content:\n%s", len(lines), content)
	}
	if !strings.HasPrefix(lines[0], "[화자1]") {
		t.Errorf("line = %q, want [화자1] prefix", lines[0])
	}
	// All three texts should appear in the single block.
	for _, text := range []string{"좋아", "알겠어", "맞아"} {
		if !strings.Contains(lines[0], text) {
			t.Errorf("single block missing %q; content: %q", text, lines[0])
		}
	}
}

// TestLabelTranscript_EmptyDiarization verifies that empty diarization input
// returns ("", 0) so callers fall back to the plain transcript.
func TestLabelTranscript_EmptyDiarization(t *testing.T) {
	t.Parallel()

	whisperSegs := []whisperSegment{
		{Start: 0.0, End: 2.0, Text: "텍스트"},
	}

	content, nSpeakers := labelTranscript(whisperSegs, nil)

	if content != "" {
		t.Errorf("content = %q, want empty (fallback)", content)
	}
	if nSpeakers != 0 {
		t.Errorf("speakerCount = %d, want 0", nSpeakers)
	}
}

// TestLabelTranscript_EmptyWhisperSegments verifies that empty whisper
// segments also returns ("", 0).
func TestLabelTranscript_EmptyWhisperSegments(t *testing.T) {
	t.Parallel()

	diarSegs := []diarSegment{
		{Start: 0.0, End: 5.0, Speaker: "SPEAKER_00"},
	}

	content, nSpeakers := labelTranscript(nil, diarSegs)

	if content != "" {
		t.Errorf("content = %q, want empty (fallback)", content)
	}
	if nSpeakers != 0 {
		t.Errorf("speakerCount = %d, want 0", nSpeakers)
	}
}

// TestLabelTranscript_SpeakerNumberingByFirstAppearance verifies that speakers
// are numbered in order of their first appearance in the whisper segment
// assignments, regardless of the order of speaker IDs in the diarization
// segments list.
func TestLabelTranscript_SpeakerNumberingByFirstAppearance(t *testing.T) {
	t.Parallel()

	// whisper order: SPEAKER_01 appears first, SPEAKER_00 second.
	whisperSegs := []whisperSegment{
		{Start: 0.0, End: 2.0, Text: "첫 발화"},  // → SPEAKER_01
		{Start: 3.0, End: 5.0, Text: "두 번째 발화"}, // → SPEAKER_00
	}
	diarSegs := []diarSegment{
		{Start: 0.0, End: 2.5, Speaker: "SPEAKER_01"}, // first assigned
		{Start: 2.5, End: 6.0, Speaker: "SPEAKER_00"}, // second assigned
	}

	content, nSpeakers := labelTranscript(whisperSegs, diarSegs)

	if nSpeakers != 2 {
		t.Errorf("speakerCount = %d, want 2", nSpeakers)
	}

	lines := strings.Split(content, "\n")
	// SPEAKER_01 was first → 화자1; SPEAKER_00 was second → 화자2
	if !strings.HasPrefix(lines[0], "[화자1]") {
		t.Errorf("line[0] = %q, want [화자1] prefix (first-appearance ordering)", lines[0])
	}
	if len(lines) > 1 && !strings.HasPrefix(lines[1], "[화자2]") {
		t.Errorf("line[1] = %q, want [화자2] prefix", lines[1])
	}
}

// ---------------------------------------------------------------------------
// DiarizationEnabled=false integration guard
// ---------------------------------------------------------------------------

// TestWhisperCollector_DiarizationDisabledByDefault verifies that a collector
// configured without DiarizationEnabled never calls the diarization endpoint,
// and produces a plain transcript document even when a diarization server is
// running.
func TestWhisperCollector_DiarizationDisabledByDefault(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const wantTranscript = "테스트 전사 내용"

	// Whisper test server — returns verbose_json with segments.
	whisperSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(whisperVerboseResponse{
			Text: wantTranscript,
			Segments: []whisperSegment{
				{Start: 0.0, End: 3.0, Text: wantTranscript},
			},
		})
	}))
	defer whisperSrv.Close()

	// Diarization server — must NOT be called.
	diarCalled := false
	diarSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		diarCalled = true
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer diarSrv.Close()

	mtime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "call.m4a", mtime)

	cfg := &config.Config{
		WhisperAudioDir:    dir,
		WhisperAPIURL:      whisperSrv.URL,
		WhisperModel:       "whisper-1",
		WhisperLanguage:    "ko",
		DiarizationEnabled: false, // OFF — default
		DiarizationAPIURL:  diarSrv.URL,
	}
	c := makeWhisperCollector(cfg, whisperSrv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Collect() returned %d docs, want 1", len(docs))
	}
	if docs[0].Content != wantTranscript {
		t.Errorf("Content = %q, want %q", docs[0].Content, wantTranscript)
	}
	if diarCalled {
		t.Error("diarization server was called even though DiarizationEnabled=false")
	}
	if _, ok := docs[0].Metadata["speaker_count"]; ok {
		t.Error("speaker_count metadata set even though diarization was disabled")
	}
}

// TestWhisperCollector_DiarizationEnabled_2Speakers verifies that when
// DiarizationEnabled=true the collector calls the diarization endpoint,
// aligns speakers, and produces labelled content with speaker_count metadata.
func TestWhisperCollector_DiarizationEnabled_2Speakers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	whisperSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(whisperVerboseResponse{
			Text: "안녕 거기서 전화해",
			Segments: []whisperSegment{
				{Start: 0.0, End: 2.0, Text: "안녕"},
				{Start: 3.0, End: 5.0, Text: "거기서 전화해"},
			},
		})
	}))
	defer whisperSrv.Close()

	diarSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(diarizeResponse{
			Segments: []diarSegment{
				{Start: 0.0, End: 2.5, Speaker: "SPEAKER_00"},
				{Start: 2.5, End: 6.0, Speaker: "SPEAKER_01"},
			},
		})
	}))
	defer diarSrv.Close()

	mtime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "call.m4a", mtime)

	cfg := &config.Config{
		WhisperAudioDir:    dir,
		WhisperAPIURL:      whisperSrv.URL,
		WhisperModel:       "whisper-1",
		WhisperLanguage:    "ko",
		DiarizationEnabled: true,
		DiarizationAPIURL:  diarSrv.URL,
	}
	c := makeWhisperCollector(cfg, whisperSrv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Collect() returned %d docs, want 1", len(docs))
	}

	doc := docs[0]

	// Content must contain speaker labels.
	if !strings.Contains(doc.Content, "[화자1]") {
		t.Errorf("Content missing [화자1]; content: %q", doc.Content)
	}
	if !strings.Contains(doc.Content, "[화자2]") {
		t.Errorf("Content missing [화자2]; content: %q", doc.Content)
	}

	// speaker_count metadata must be set.
	if v, ok := doc.Metadata["speaker_count"]; !ok || v != 2 {
		t.Errorf("Metadata[speaker_count] = %v, want 2", v)
	}
}

// TestWhisperCollector_DiarizationFallsBackOnServiceError verifies that when
// the diarization service returns an error the collector falls back gracefully
// to the plain transcript (no document failure).
func TestWhisperCollector_DiarizationFallsBackOnServiceError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const wantFallbackText = "전사 텍스트 폴백"

	whisperSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(whisperVerboseResponse{
			Text: wantFallbackText,
			Segments: []whisperSegment{
				{Start: 0.0, End: 3.0, Text: wantFallbackText},
			},
		})
	}))
	defer whisperSrv.Close()

	// Diarization service is down.
	diarSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer diarSrv.Close()

	mtime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "call.m4a", mtime)

	cfg := &config.Config{
		WhisperAudioDir:    dir,
		WhisperAPIURL:      whisperSrv.URL,
		WhisperModel:       "whisper-1",
		WhisperLanguage:    "ko",
		DiarizationEnabled: true,
		DiarizationAPIURL:  diarSrv.URL,
	}
	c := makeWhisperCollector(cfg, whisperSrv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Collect() returned %d docs, want 1 (fallback)", len(docs))
	}
	if docs[0].Content != wantFallbackText {
		t.Errorf("Content = %q, want plain fallback %q", docs[0].Content, wantFallbackText)
	}
	if _, ok := docs[0].Metadata["speaker_count"]; ok {
		t.Error("speaker_count must not be set when diarization fails")
	}
}

// TestWhisperCollector_DiarizationFallsBackWhenNoSegments verifies that when
// whisper returns no segments (e.g. older server version returning only text),
// diarization is skipped and the plain transcript is used.
func TestWhisperCollector_DiarizationFallsBackWhenNoSegments(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const wantText = "세그먼트 없는 전사"

	// Whisper server returns plain text-only response (no segments).
	whisperSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Encode only text — segments absent.
		_ = json.NewEncoder(w).Encode(map[string]string{"text": wantText})
	}))
	defer whisperSrv.Close()

	// Diarization must NOT be called.
	diarCalled := false
	diarSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		diarCalled = true
		http.Error(w, "must not be called", http.StatusInternalServerError)
	}))
	defer diarSrv.Close()

	mtime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "call.m4a", mtime)

	cfg := &config.Config{
		WhisperAudioDir:    dir,
		WhisperAPIURL:      whisperSrv.URL,
		WhisperModel:       "whisper-1",
		WhisperLanguage:    "ko",
		DiarizationEnabled: true,
		DiarizationAPIURL:  diarSrv.URL,
	}
	c := makeWhisperCollector(cfg, whisperSrv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Collect() returned %d docs, want 1", len(docs))
	}
	if docs[0].Content != wantText {
		t.Errorf("Content = %q, want %q", docs[0].Content, wantText)
	}
	if diarCalled {
		t.Error("diarization was called even though whisper returned no segments")
	}
}

// ---------------------------------------------------------------------------
// isTPhoneCallPath unit tests
// ---------------------------------------------------------------------------

func TestIsTPhoneCallPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"tphone with 14-digit timestamp", "01025777190_20260327202518.m4a", true},
		{"tphone with suffix", "01012345678_20260101120000-1.m4a", true},
		{"voice recorder pattern", "메디웨일_260120_120138.m4a", false},
		{"plain name", "recording.m4a", false},
		{"empty", "", false},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isTPhoneCallPath(tc.path); got != tc.want {
				t.Errorf("isTPhoneCallPath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// num_speakers field propagation test
// ---------------------------------------------------------------------------

// TestWhisperCollector_DiarizationTPhonePassesNumSpeakers verifies that for a
// TPhone call recording, the diarization request includes num_speakers=2.
func TestWhisperCollector_DiarizationTPhonePassesNumSpeakers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	whisperSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(whisperVerboseResponse{
			Text: "통화 내용",
			Segments: []whisperSegment{
				{Start: 0.0, End: 3.0, Text: "통화 내용"},
			},
		})
	}))
	defer whisperSrv.Close()

	var capturedNumSpeakers string
	diarSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Parse multipart to extract num_speakers field.
		contentType := r.Header.Get("Content-Type")
		_, params, _ := mime.ParseMediaType(contentType)
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				break
			}
			data, _ := io.ReadAll(p)
			if p.FormName() == "num_speakers" {
				capturedNumSpeakers = string(data)
			}
			p.Close()
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(diarizeResponse{
			Segments: []diarSegment{
				{Start: 0.0, End: 3.0, Speaker: "SPEAKER_00"},
			},
		})
	}))
	defer diarSrv.Close()

	// TPhone-pattern filename: phone_number_YYYYMMDDHHMMSS.m4a
	mtime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "01012345678_20260101120000.m4a", mtime)

	cfg := &config.Config{
		WhisperAudioDir:    dir,
		WhisperAPIURL:      whisperSrv.URL,
		WhisperModel:       "whisper-1",
		WhisperLanguage:    "ko",
		DiarizationEnabled: true,
		DiarizationAPIURL:  diarSrv.URL,
	}
	c := makeWhisperCollector(cfg, whisperSrv)

	_, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}

	if capturedNumSpeakers != "2" {
		t.Errorf("num_speakers = %q, want %q for TPhone call pattern", capturedNumSpeakers, "2")
	}
}

// TestWhisperCollector_DiarizationVoiceMemoOmitsNumSpeakers verifies that for
// a voice-memo recording (non-TPhone pattern), num_speakers is NOT sent.
func TestWhisperCollector_DiarizationVoiceMemoOmitsNumSpeakers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	whisperSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(whisperVerboseResponse{
			Text: "보이스 메모",
			Segments: []whisperSegment{
				{Start: 0.0, End: 3.0, Text: "보이스 메모"},
			},
		})
	}))
	defer whisperSrv.Close()

	numSpeakersPresent := false
	diarSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType := r.Header.Get("Content-Type")
		_, params, _ := mime.ParseMediaType(contentType)
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				break
			}
			if p.FormName() == "num_speakers" {
				numSpeakersPresent = true
			}
			io.ReadAll(p) //nolint:errcheck
			p.Close()
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(diarizeResponse{
			Segments: []diarSegment{
				{Start: 0.0, End: 3.0, Speaker: "SPEAKER_00"},
			},
		})
	}))
	defer diarSrv.Close()

	// Voice Recorder pattern: label_YYMMDD_HHMMSS.m4a
	mtime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "메모_260120_120138.m4a", mtime)

	cfg := &config.Config{
		WhisperAudioDir:    dir,
		WhisperAPIURL:      whisperSrv.URL,
		WhisperModel:       "whisper-1",
		WhisperLanguage:    "ko",
		DiarizationEnabled: true,
		DiarizationAPIURL:  diarSrv.URL,
	}
	c := makeWhisperCollector(cfg, whisperSrv)

	_, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}

	if numSpeakersPresent {
		t.Error("num_speakers field was sent for a voice-memo file — should be omitted")
	}
}

// ---------------------------------------------------------------------------
// response_format gating tests
// ---------------------------------------------------------------------------

// TestWhisperCollector_ResponseFormatAbsentWhenDiarizationDisabled asserts that
// with DiarizationEnabled=false the multipart request to Whisper does NOT
// include the response_format field — preserving the original pre-#111 request
// format byte-for-byte.
func TestWhisperCollector_ResponseFormatAbsentWhenDiarizationDisabled(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const wantTranscript = "원본 요청 포맷 확인"

	// newWhisperTestServer (defined in whisper_test.go) captures all multipart
	// fields excluding "file". We use it here to inspect whether response_format
	// was sent.
	srv, captured := newWhisperTestServer(t, wantTranscript)

	mtime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "call.m4a", mtime)

	cfg := &config.Config{
		WhisperAudioDir:    dir,
		WhisperAPIURL:      srv.URL,
		WhisperModel:       "whisper-1",
		WhisperLanguage:    "ko",
		DiarizationEnabled: false, // flag OFF
	}
	c := makeWhisperCollector(cfg, srv)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Collect() returned %d docs, want 1", len(docs))
	}

	// response_format must be absent from the multipart fields.
	if v, present := captured.fields["response_format"]; present {
		t.Errorf("response_format field present in request when DiarizationEnabled=false: %q", v)
	}

	// Plain transcript must be used as content.
	if docs[0].Content != wantTranscript {
		t.Errorf("Content = %q, want %q", docs[0].Content, wantTranscript)
	}
}

// TestWhisperCollector_ResponseFormatVerboseJsonWhenDiarizationEnabled asserts
// that with DiarizationEnabled=true the multipart request includes
// response_format=verbose_json.
func TestWhisperCollector_ResponseFormatVerboseJsonWhenDiarizationEnabled(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Whisper server that captures fields and returns a verbose_json response.
	var capturedResponseFormat string
	whisperSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType := r.Header.Get("Content-Type")
		_, params, _ := mime.ParseMediaType(contentType)
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				break
			}
			data, _ := io.ReadAll(p)
			if p.FormName() == "response_format" {
				capturedResponseFormat = string(data)
			}
			p.Close()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(whisperVerboseResponse{
			Text: "verbose 응답",
			Segments: []whisperSegment{
				{Start: 0.0, End: 3.0, Text: "verbose 응답"},
			},
		})
	}))
	defer whisperSrv.Close()

	// Diarization server (minimal — just needs to not 500 so Collect proceeds).
	diarSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(diarizeResponse{
			Segments: []diarSegment{
				{Start: 0.0, End: 3.0, Speaker: "SPEAKER_00"},
			},
		})
	}))
	defer diarSrv.Close()

	mtime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "call.m4a", mtime)

	cfg := &config.Config{
		WhisperAudioDir:    dir,
		WhisperAPIURL:      whisperSrv.URL,
		WhisperModel:       "whisper-1",
		WhisperLanguage:    "ko",
		DiarizationEnabled: true, // flag ON
		DiarizationAPIURL:  diarSrv.URL,
	}
	c := makeWhisperCollector(cfg, whisperSrv)

	_, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}

	if capturedResponseFormat != "verbose_json" {
		t.Errorf("response_format = %q, want %q when DiarizationEnabled=true",
			capturedResponseFormat, "verbose_json")
	}
}
