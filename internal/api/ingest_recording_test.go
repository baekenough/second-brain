package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/collector/smsmap"
)

// --- helpers ---

// audioFilesInDir returns the names of non-sidecar files in dir (i.e. files
// that do NOT end in ".meta.json"). Use this in tests that assert on the number
// of audio files written by the ingest-recording handler, since the handler now
// also writes a {audio}.meta.json sidecar alongside every audio file.
func audioFilesInDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("audioFilesInDir readdir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".meta.json") {
			names = append(names, e.Name())
		}
	}
	return names
}

// validM4ABytes returns a minimal byte slice that passes the m4a audio
// validation check (audiovalidate.CheckM4A). The slice contains a valid
// ISOBMFF ftyp box marker at offset 4 and is padded to n bytes total.
// n must be >= 8; any value < 8 is silently bumped to 8.
func validM4ABytes(n int) []byte {
	if n < 8 {
		n = 8
	}
	b := make([]byte, n)
	copy(b[4:8], "ftyp")
	return b
}

// newRecordingTestServer creates a Server wired for the ingest-recording handler.
// If recordingDir is empty a temp dir is created and its path returned.
//
// PII number hashing (issue #164) is explicitly turned ON here so every
// existing test in this file — written when hashing was the only behaviour —
// keeps passing unmodified. Tests exercising the new default (hashing OFF,
// raw number surfaced) build their own Server directly with
// WithPIINumberHashing(false) instead of using this helper; see
// TestIngestRecording_AnonymousCallTitleRawNumberWhenHashingDisabled and
// TestIngestRecording_FilenameUsesRawNumberWhenHashingDisabled.
func newRecordingTestServer(
	t *testing.T,
	upserter IngestRecordingUpserter,
	recordingDir string,
	maxFileBytes int64,
	cutover time.Time,
) (*Server, string) {
	t.Helper()
	if recordingDir == "" {
		recordingDir = t.TempDir()
	}
	srv := NewServer(nil, nil, nil, nil, nil, "", "test-key").
		WithPIINumberHashing(true)
	srv.WithIngestRecording(upserter, recordingDir, maxFileBytes, cutover)
	return srv, recordingDir
}

// buildRecordingForm constructs a multipart/form-data request body for the
// recording ingest endpoint.
func buildRecordingForm(
	t *testing.T,
	filename string,
	audioData []byte,
	number string,
	dateMs int64,
	extra ...string, // alternating key/value pairs
) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	// Audio file part.
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(audioData); err != nil {
		t.Fatalf("write audio bytes: %v", err)
	}

	// Required fields.
	_ = mw.WriteField("number", number)
	_ = mw.WriteField("date_ms", fmt.Sprintf("%d", dateMs))

	// Optional extra fields.
	for i := 0; i+1 < len(extra); i += 2 {
		_ = mw.WriteField(extra[i], extra[i+1])
	}

	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

// doRecordingPost sends a POST /api/v1/ingest/recording through the full chi router.
func doRecordingPost(t *testing.T, srv *Server, body interface{ Read([]byte) (int, error) }, contentType, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/recording", body)
	req.Header.Set("Content-Type", contentType)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

// --- tests ---

// TestIngestRecording_AuthRequired verifies that a missing Bearer token returns 401.
func TestIngestRecording_AuthRequired(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv, _ := newRecordingTestServer(t, upserter, "", 0, time.Time{})

	dateMs := time.Now().Add(-time.Hour).UnixMilli()
	body, ct := buildRecordingForm(t, "audio.m4a", validM4ABytes(32), "010-1234-5678", dateMs)
	rr := doRecordingPost(t, srv, body, ct, "" /* no auth */)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusUnauthorized, rr.Body.String())
	}
}

// TestIngestRecording_Success verifies that a valid recording upload saves the
// file, creates a PENDING document, and returns 201.
func TestIngestRecording_Success(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv, recordingDir := newRecordingTestServer(t, upserter, "", 0, time.Time{})

	number := "010-1234-5678"
	dateMs := time.Now().Add(-time.Hour).UnixMilli()
	audioData := validM4ABytes(32)

	body, ct := buildRecordingForm(t, "recording.m4a", audioData, number, dateMs,
		"duration_sec", "120",
		"contact_name", "Alice",
	)
	rr := doRecordingPost(t, srv, body, ct, "Bearer test-key")

	if rr.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}

	var resp IngestRecordingResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Accepted {
		t.Error("expected accepted=true")
	}
	if resp.DocumentID == "" {
		t.Error("expected non-empty document_id")
	}
	if resp.Skipped {
		t.Error("expected skipped=false")
	}

	// Document upserted.
	if len(upserter.upserted) != 1 {
		t.Fatalf("expected 1 upserted doc, got %d", len(upserter.upserted))
	}

	// Audio file written to recordingDir (sidecar .meta.json is excluded).
	audioFiles := audioFilesInDir(t, recordingDir)
	if len(audioFiles) != 1 {
		t.Fatalf("expected 1 audio file in recordingDir, got %d", len(audioFiles))
	}
	audioFile := audioFiles[0]

	// Filename must encode the recording timestamp in TPhoneCallRecords format
	// so WhisperCollector.recordingTime() can parse it. The raw number is
	// never embedded in the filename (issue #164/#165 follow-up) — only its
	// one-way hash is. Format: {numHash}_{YYYYMMDDHHMMSS}.m4a
	expectedPattern := smsmap.ShortHash(number) + "_"
	if len(audioFile) < len(expectedPattern) || audioFile[:len(expectedPattern)] != expectedPattern {
		t.Errorf("audio filename %q does not start with %q", audioFile, expectedPattern)
	}
	if strings.Contains(audioFile, sanitizePhoneNumber(number)) {
		t.Errorf("audio filename %q must not contain the raw phone number", audioFile)
	}
	if filepath.Ext(audioFile) != ".m4a" {
		t.Errorf("audio filename ext = %q, want .m4a", filepath.Ext(audioFile))
	}

	// Audio bytes must match.
	got, err := os.ReadFile(filepath.Join(recordingDir, audioFile))
	if err != nil {
		t.Fatalf("read audio file: %v", err)
	}
	if !bytes.Equal(got, audioData) {
		t.Errorf("audio file content mismatch: got %q, want %q", got, audioData)
	}
}

// TestIngestRecording_StoresPendingDocument verifies that the upserted document
// has the correct SourceType, "pending" metadata, and OccurredAt.
func TestIngestRecording_StoresPendingDocument(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv, _ := newRecordingTestServer(t, upserter, "", 0, time.Time{})

	number := "010-9999-8888"
	dateMs := int64(1705311000000) // 2024-01-15 09:30:00 UTC
	wantTime := time.Unix(1705311000, 0).UTC()

	body, ct := buildRecordingForm(t, "call.m4a", validM4ABytes(32), number, dateMs)
	rr := doRecordingPost(t, srv, body, ct, "Bearer test-key")
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rr.Code, rr.Body.String())
	}

	if len(upserter.upserted) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(upserter.upserted))
	}
	doc := upserter.upserted[0]

	// Must be call-log source type (not call-transcript — transcription pending).
	if doc.SourceType != "call-log" {
		t.Errorf("SourceType=%q, want call-log", doc.SourceType)
	}

	// OccurredAt must match dateMs.
	if doc.OccurredAt == nil {
		t.Fatal("OccurredAt is nil")
	}
	if !doc.OccurredAt.Equal(wantTime) {
		t.Errorf("OccurredAt=%v, want %v", doc.OccurredAt, wantTime)
	}

	// Transcription must be marked as pending.
	transcription, _ := doc.Metadata["transcription"].(string)
	if transcription != "pending" {
		t.Errorf("metadata[transcription]=%q, want pending", transcription)
	}
}

// TestIngestRecording_Idempotent verifies that uploading the same recording
// twice produces the same SourceID (the store's Upsert handles deduplication).
func TestIngestRecording_Idempotent(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv, _ := newRecordingTestServer(t, upserter, "", 0, time.Time{})

	number := "010-3333-4444"
	dateMs := time.Now().Add(-time.Hour).UnixMilli()
	durationSec := "60"

	// First upload.
	b1, ct1 := buildRecordingForm(t, "audio.m4a", validM4ABytes(32), number, dateMs, "duration_sec", durationSec)
	rr1 := doRecordingPost(t, srv, b1, ct1, "Bearer test-key")
	if rr1.Code != http.StatusCreated {
		t.Fatalf("first: status = %d, want 201", rr1.Code)
	}

	// Second upload (identical metadata).
	b2, ct2 := buildRecordingForm(t, "audio.m4a", validM4ABytes(32), number, dateMs, "duration_sec", durationSec)
	rr2 := doRecordingPost(t, srv, b2, ct2, "Bearer test-key")
	if rr2.Code != http.StatusCreated {
		t.Fatalf("second: status = %d, want 201", rr2.Code)
	}

	if len(upserter.upserted) != 2 {
		t.Fatalf("expected 2 upsert calls, got %d", len(upserter.upserted))
	}
	id1 := upserter.upserted[0].SourceID
	id2 := upserter.upserted[1].SourceID
	if id1 != id2 {
		t.Errorf("SourceID mismatch (not idempotent): first=%q second=%q", id1, id2)
	}
	if len(id1) < 9 || id1[:9] != "call-log:" {
		t.Errorf("SourceID %q does not have expected 'call-log:' prefix", id1)
	}
}

// TestIngestRecording_CutoverSkips verifies that a recording pre-dating the
// cutover floor returns 200 with accepted=false, skipped=true.
func TestIngestRecording_CutoverSkips(t *testing.T) {
	t.Parallel()

	cutover := time.Now().Add(-30 * time.Minute)
	upserter := &stubIngestUpserter{}
	srv, _ := newRecordingTestServer(t, upserter, "", 0, cutover)

	// Recording 2 hours ago — before the cutover floor.
	oldDateMs := time.Now().Add(-2 * time.Hour).UnixMilli()
	body, ct := buildRecordingForm(t, "old.m4a", []byte("audio"), "010-0000-0001", oldDateMs)
	rr := doRecordingPost(t, srv, body, ct, "Bearer test-key")

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	var resp IngestRecordingResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Accepted {
		t.Error("expected accepted=false for pre-cutover recording")
	}
	if !resp.Skipped {
		t.Error("expected skipped=true for pre-cutover recording")
	}
	if len(upserter.upserted) != 0 {
		t.Errorf("expected 0 upserts (skipped), got %d", len(upserter.upserted))
	}
}

// TestIngestRecording_OversizedFile verifies that a file exceeding the limit
// returns 413.
func TestIngestRecording_OversizedFile(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	// Limit to 64 bytes.
	srv, _ := newRecordingTestServer(t, upserter, "", 64, time.Time{})

	bigAudio := bytes.Repeat([]byte("x"), 128)
	dateMs := time.Now().Add(-time.Hour).UnixMilli()
	body, ct := buildRecordingForm(t, "big.m4a", bigAudio, "010-0000-0001", dateMs)
	rr := doRecordingPost(t, srv, body, ct, "Bearer test-key")

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusRequestEntityTooLarge, rr.Body.String())
	}

	var errBody map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if _, ok := errBody["error"]; !ok {
		t.Error("response missing 'error' key")
	}
}

// TestIngestRecording_MissingNumber verifies that a missing 'number' field
// returns 400.
func TestIngestRecording_MissingNumber(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv, _ := newRecordingTestServer(t, upserter, "", 0, time.Time{})

	dateMs := time.Now().Add(-time.Hour).UnixMilli()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "audio.m4a")
	_, _ = fw.Write([]byte("fake audio"))
	// number intentionally omitted
	_ = mw.WriteField("date_ms", fmt.Sprintf("%d", dateMs))
	_ = mw.Close()

	rr := doRecordingPost(t, srv, &buf, mw.FormDataContentType(), "Bearer test-key")

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusBadRequest, rr.Body.String())
	}
}

// TestIngestRecording_MissingDateMs verifies that a missing or zero 'date_ms'
// field returns 400.
func TestIngestRecording_MissingDateMs(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv, _ := newRecordingTestServer(t, upserter, "", 0, time.Time{})

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "audio.m4a")
	_, _ = fw.Write([]byte("fake audio"))
	_ = mw.WriteField("number", "010-0000-0001")
	// date_ms intentionally omitted
	_ = mw.Close()

	rr := doRecordingPost(t, srv, &buf, mw.FormDataContentType(), "Bearer test-key")

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusBadRequest, rr.Body.String())
	}
}

// TestIngestRecording_FilenameEncoding verifies that the saved audio file uses
// the TPhoneCallRecords naming pattern expected by WhisperCollector.recordingTime().
func TestIngestRecording_FilenameEncoding(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv, recordingDir := newRecordingTestServer(t, upserter, "", 0, time.Time{})

	number := "01012345678"
	// 2024-01-15 09:30:00 UTC
	dateMs := int64(1705311000000)
	// local time: what we expect in the filename (use time.Local)
	localTime := time.Unix(1705311000, 0).In(time.Local)
	expectedTimestamp := localTime.Format("20060102150405")

	body, ct := buildRecordingForm(t, "call.m4a", validM4ABytes(32), number, dateMs)
	rr := doRecordingPost(t, srv, body, ct, "Bearer test-key")
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rr.Code, rr.Body.String())
	}

	audioFiles := audioFilesInDir(t, recordingDir)
	if len(audioFiles) != 1 {
		t.Fatalf("expected 1 audio file, got %d", len(audioFiles))
	}
	audioFile := audioFiles[0]

	wantName := fmt.Sprintf("%s_%s.m4a", smsmap.ShortHash(number), expectedTimestamp)
	if audioFile != wantName {
		t.Errorf("audio filename=%q, want %q", audioFile, wantName)
	}
}

// TestIngestRecording_FilenameUsesRawNumberWhenHashingDisabled is the mirror
// of TestIngestRecording_FilenameEncoding for the default
// (PIINumberHashingEnabled=false) case: the saved audio filename embeds the
// sanitized raw number instead of its hash (issue #164 policy reversal), and
// the sidecar carries "number" (raw) rather than "number_hash".
func TestIngestRecording_FilenameUsesRawNumberWhenHashingDisabled(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	recordingDir := t.TempDir()
	srv := NewServer(nil, nil, nil, nil, nil, "", "test-key")
	srv.WithIngestRecording(upserter, recordingDir, 0, time.Time{})

	number := "01012345678"
	// 2024-01-15 09:30:00 UTC
	dateMs := int64(1705311000000)
	localTime := time.Unix(1705311000, 0).In(time.Local)
	expectedTimestamp := localTime.Format("20060102150405")

	body, ct := buildRecordingForm(t, "call.m4a", validM4ABytes(32), number, dateMs)
	rr := doRecordingPost(t, srv, body, ct, "Bearer test-key")
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rr.Code, rr.Body.String())
	}

	audioFiles := audioFilesInDir(t, recordingDir)
	if len(audioFiles) != 1 {
		t.Fatalf("expected 1 audio file, got %d", len(audioFiles))
	}
	audioFile := audioFiles[0]

	wantName := fmt.Sprintf("%s_%s.m4a", sanitizePhoneNumber(number), expectedTimestamp)
	if audioFile != wantName {
		t.Errorf("audio filename=%q, want %q", audioFile, wantName)
	}

	// Sidecar must carry "number" (raw), not "number_hash".
	sidecarPath := filepath.Join(recordingDir, audioFile+".meta.json")
	sidecarData, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	var sidecar map[string]any
	if err := json.Unmarshal(sidecarData, &sidecar); err != nil {
		t.Fatalf("unmarshal sidecar: %v", err)
	}
	if sidecar["number"] != number {
		t.Errorf("sidecar number = %v, want %q", sidecar["number"], number)
	}
	if _, ok := sidecar["number_hash"]; ok {
		t.Errorf("sidecar must not contain number_hash when hashing is disabled, got %v", sidecar["number_hash"])
	}
}

// TestIngestRecording_NotConfigured verifies that the endpoint returns 503 when
// not wired up.
func TestIngestRecording_NotConfigured(t *testing.T) {
	t.Parallel()

	// Server without WithIngestRecording.
	srv := NewServer(nil, nil, nil, nil, nil, "", "test-key")

	dateMs := time.Now().Add(-time.Hour).UnixMilli()
	body, ct := buildRecordingForm(t, "audio.m4a", []byte("audio"), "010-0000-0001", dateMs)
	rr := doRecordingPost(t, srv, body, ct, "Bearer test-key")

	// Route should not be registered, so chi returns 404.
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusNotFound, rr.Body.String())
	}
}

// --- unit tests for helpers ---

// TestIngestRecording_VoiceMemoSuccess verifies that a valid voice-memo upload
// (no phone number) is accepted with 201 and stores the correct metadata.
func TestIngestRecording_VoiceMemoSuccess(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv, recordingDir := newRecordingTestServer(t, upserter, "", 0, time.Time{})

	dateMs := time.Now().Add(-time.Hour).UnixMilli()
	audioData := validM4ABytes(32)

	body, ct := buildRecordingForm(t, "memo.m4a", audioData, "" /* no number */, dateMs,
		"kind", "voice-memo",
		"duration_sec", "45",
		"contact_name", "회의 메모",
	)
	rr := doRecordingPost(t, srv, body, ct, "Bearer test-key")

	if rr.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}

	if len(upserter.upserted) != 1 {
		t.Fatalf("expected 1 upserted doc, got %d", len(upserter.upserted))
	}
	doc := upserter.upserted[0]

	// SourceType must remain call-log for schema compatibility.
	if doc.SourceType != "call-log" {
		t.Errorf("SourceType=%q, want call-log", doc.SourceType)
	}

	// recording_type must be "voice-memo".
	recType, _ := doc.Metadata["recording_type"].(string)
	if recType != "voice-memo" {
		t.Errorf("metadata[recording_type]=%q, want voice-memo", recType)
	}

	// direction must NOT be present for voice-memo.
	if _, ok := doc.Metadata["direction"]; ok {
		t.Error("metadata[direction] should not be set for voice-memo")
	}

	// transcription must be pending.
	transcription, _ := doc.Metadata["transcription"].(string)
	if transcription != "pending" {
		t.Errorf("metadata[transcription]=%q, want pending", transcription)
	}

	// Audio file must exist in recordingDir with voice-memo prefix
	// (sidecar .meta.json is excluded from the count).
	audioFiles := audioFilesInDir(t, recordingDir)
	if len(audioFiles) != 1 {
		t.Fatalf("expected 1 audio file, got %d", len(audioFiles))
	}
	audioFile := audioFiles[0]
	// New format: voice-memo_{YYYYMMDDHHMMSS}_{sanitizedOriginalName}.ext
	const voiceMemoPrefix = "voice-memo_"
	if !strings.HasPrefix(audioFile, voiceMemoPrefix) {
		t.Errorf("audio filename %q does not start with %q", audioFile, voiceMemoPrefix)
	}

	// Audio bytes must match.
	got, err := os.ReadFile(filepath.Join(recordingDir, audioFile))
	if err != nil {
		t.Fatalf("read audio file: %v", err)
	}
	if !bytes.Equal(got, audioData) {
		t.Errorf("audio file content mismatch")
	}
}

// TestIngestRecording_VoiceMemoNoNumberAllowed verifies that kind=voice-memo
// with an empty number field returns 201 (not 400).
func TestIngestRecording_VoiceMemoNoNumberAllowed(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv, _ := newRecordingTestServer(t, upserter, "", 0, time.Time{})

	dateMs := time.Now().Add(-time.Hour).UnixMilli()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "audio.m4a")
	_, _ = fw.Write(validM4ABytes(32))
	// number intentionally omitted
	_ = mw.WriteField("kind", "voice-memo")
	_ = mw.WriteField("date_ms", fmt.Sprintf("%d", dateMs))
	_ = mw.Close()

	rr := doRecordingPost(t, srv, &buf, mw.FormDataContentType(), "Bearer test-key")

	if rr.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}
}

// TestIngestRecording_CallKindRequiresNumber verifies that kind=call with an
// empty number field still returns 400.
func TestIngestRecording_CallKindRequiresNumber(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv, _ := newRecordingTestServer(t, upserter, "", 0, time.Time{})

	dateMs := time.Now().Add(-time.Hour).UnixMilli()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "audio.m4a")
	_, _ = fw.Write([]byte("fake audio"))
	_ = mw.WriteField("kind", "call")
	// number intentionally omitted
	_ = mw.WriteField("date_ms", fmt.Sprintf("%d", dateMs))
	_ = mw.Close()

	rr := doRecordingPost(t, srv, &buf, mw.FormDataContentType(), "Bearer test-key")

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusBadRequest, rr.Body.String())
	}
}

// TestIngestRecording_InvalidKind verifies that an unknown kind value returns 400.
func TestIngestRecording_InvalidKind(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv, _ := newRecordingTestServer(t, upserter, "", 0, time.Time{})

	dateMs := time.Now().Add(-time.Hour).UnixMilli()
	body, ct := buildRecordingForm(t, "audio.m4a", validM4ABytes(32), "010-1234-5678", dateMs,
		"kind", "unknown-kind",
	)
	rr := doRecordingPost(t, srv, body, ct, "Bearer test-key")

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusBadRequest, rr.Body.String())
	}
}

// TestIngestRecording_CallRecordingType verifies that kind=call stores
// recording_type="call" in metadata.
func TestIngestRecording_CallRecordingType(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv, _ := newRecordingTestServer(t, upserter, "", 0, time.Time{})

	dateMs := time.Now().Add(-time.Hour).UnixMilli()
	body, ct := buildRecordingForm(t, "call.m4a", validM4ABytes(32), "010-1234-5678", dateMs,
		"kind", "call",
	)
	rr := doRecordingPost(t, srv, body, ct, "Bearer test-key")
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rr.Code, rr.Body.String())
	}

	if len(upserter.upserted) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(upserter.upserted))
	}
	doc := upserter.upserted[0]

	recType, _ := doc.Metadata["recording_type"].(string)
	if recType != "call" {
		t.Errorf("metadata[recording_type]=%q, want call", recType)
	}

	direction, _ := doc.Metadata["direction"].(string)
	if direction != "incoming" {
		t.Errorf("metadata[direction]=%q, want incoming", direction)
	}
}

// TestIngestRecording_CallSourceIDStableAcrossFilenameFix verifies that the
// call-log SourceID format (call-log:{dateMs}:{numHash}:{durHash}) is
// unaffected by the audio-filename hashing fix (issue #164/#165 follow-up):
// the on-disk filename and the SourceID are computed independently from the
// same numHash value, so changing the filename scheme does not change the
// SourceID for already-ingested documents.
func TestIngestRecording_CallSourceIDStableAcrossFilenameFix(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv, _ := newRecordingTestServer(t, upserter, "", 0, time.Time{})

	number := "010-7777-8888"
	dateMs := int64(1705311000000)
	durationSec := "45"

	body, ct := buildRecordingForm(t, "call.m4a", validM4ABytes(32), number, dateMs,
		"duration_sec", durationSec,
	)
	rr := doRecordingPost(t, srv, body, ct, "Bearer test-key")
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rr.Code, rr.Body.String())
	}
	if len(upserter.upserted) != 1 {
		t.Fatalf("expected 1 upserted doc, got %d", len(upserter.upserted))
	}

	numHash := smsmap.ShortHash(number)
	durHash := smsmap.BodyShortHash(durationSec)
	wantSourceID := fmt.Sprintf("call-log:%d:%s:%s", dateMs, numHash, durHash)

	got := upserter.upserted[0].SourceID
	if got != wantSourceID {
		t.Errorf("SourceID = %q, want %q (must match pre-existing format so already-ingested docs are not orphaned)",
			got, wantSourceID)
	}
}

// TestIngestRecording_AnonymousCallTitleNeverContainsRawNumber verifies that
// when contact_name is absent, the call-log document's Title, Content, and
// Metadata never fall back to the raw phone number — instead a short,
// one-way hashed label consistent with number_hash is used (issue #164/#165
// follow-up).
func TestIngestRecording_AnonymousCallTitleNeverContainsRawNumber(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv, _ := newRecordingTestServer(t, upserter, "", 0, time.Time{})

	number := "010-5555-6666"
	dateMs := time.Now().Add(-time.Hour).UnixMilli()

	// contact_name intentionally omitted.
	body, ct := buildRecordingForm(t, "anon.m4a", validM4ABytes(32), number, dateMs,
		"duration_sec", "30",
	)
	rr := doRecordingPost(t, srv, body, ct, "Bearer test-key")
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rr.Code, rr.Body.String())
	}

	if len(upserter.upserted) != 1 {
		t.Fatalf("expected 1 upserted doc, got %d", len(upserter.upserted))
	}
	doc := upserter.upserted[0]

	if strings.Contains(doc.Title, number) {
		t.Errorf("Title = %q must not contain the raw phone number %q", doc.Title, number)
	}
	if strings.Contains(doc.Content, number) {
		t.Errorf("Content = %q must not contain the raw phone number %q", doc.Content, number)
	}
	wantHashPrefix := smsmap.ShortHash(number)[:8]
	if !strings.Contains(doc.Title, wantHashPrefix) {
		t.Errorf("Title = %q, want it to contain the hashed label %q", doc.Title, wantHashPrefix)
	}
	if !strings.Contains(doc.Content, wantHashPrefix) {
		t.Errorf("Content = %q, want it to contain the hashed label %q", doc.Content, wantHashPrefix)
	}

	// Metadata's contact_name field mirrors the (empty) form input verbatim —
	// it is not itself a raw-number leak since it is empty, but assert this
	// explicitly so a future change doesn't silently reintroduce one.
	if contactName, _ := doc.Metadata["contact_name"].(string); contactName != "" {
		t.Errorf("metadata[contact_name] = %q, want empty", contactName)
	}
}

// TestIngestRecording_AnonymousCallTitleRawNumberWhenHashingDisabled is the
// mirror of TestIngestRecording_AnonymousCallTitleNeverContainsRawNumber for
// the default (PIINumberHashingEnabled=false) case: Title/Content fall back
// to the raw number instead of a hashed label, and the raw number is
// additionally surfaced in Metadata["number"] so it stays searchable (issue
// #164 policy reversal). SourceID stays hashed regardless.
func TestIngestRecording_AnonymousCallTitleRawNumberWhenHashingDisabled(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	recordingDir := t.TempDir()
	srv := NewServer(nil, nil, nil, nil, nil, "", "test-key")
	srv.WithIngestRecording(upserter, recordingDir, 0, time.Time{})

	number := "010-5555-6666"
	dateMs := time.Now().Add(-time.Hour).UnixMilli()

	// contact_name intentionally omitted.
	body, ct := buildRecordingForm(t, "anon.m4a", validM4ABytes(32), number, dateMs,
		"duration_sec", "30",
	)
	rr := doRecordingPost(t, srv, body, ct, "Bearer test-key")
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rr.Code, rr.Body.String())
	}

	if len(upserter.upserted) != 1 {
		t.Fatalf("expected 1 upserted doc, got %d", len(upserter.upserted))
	}
	doc := upserter.upserted[0]

	if !strings.Contains(doc.Title, number) {
		t.Errorf("Title = %q should contain the raw phone number %q when hashing is disabled", doc.Title, number)
	}
	if !strings.Contains(doc.Content, number) {
		t.Errorf("Content = %q should contain the raw phone number %q when hashing is disabled", doc.Content, number)
	}

	gotNumber, _ := doc.Metadata["number"].(string)
	if gotNumber != number {
		t.Errorf("metadata[number] = %q, want %q", gotNumber, number)
	}

	// SourceID must still be hashed — this flag never touches dedup identity.
	wantNumHash := smsmap.ShortHash(number)
	if !strings.Contains(doc.SourceID, wantNumHash) {
		t.Errorf("SourceID = %q should still contain the hashed number %q regardless of the flag", doc.SourceID, wantNumHash)
	}
}

// TestIngestRecording_VoiceMemoIdempotent verifies that uploading the same
// voice-memo twice produces the same SourceID.
func TestIngestRecording_VoiceMemoIdempotent(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv, _ := newRecordingTestServer(t, upserter, "", 0, time.Time{})

	dateMs := time.Now().Add(-time.Hour).UnixMilli()

	// First upload.
	b1, ct1 := buildRecordingForm(t, "memo.m4a", validM4ABytes(32), "", dateMs, "kind", "voice-memo")
	rr1 := doRecordingPost(t, srv, b1, ct1, "Bearer test-key")
	if rr1.Code != http.StatusCreated {
		t.Fatalf("first: status = %d, want 201", rr1.Code)
	}

	// Second upload (identical metadata).
	b2, ct2 := buildRecordingForm(t, "memo.m4a", validM4ABytes(32), "", dateMs, "kind", "voice-memo")
	rr2 := doRecordingPost(t, srv, b2, ct2, "Bearer test-key")
	if rr2.Code != http.StatusCreated {
		t.Fatalf("second: status = %d, want 201", rr2.Code)
	}

	if len(upserter.upserted) != 2 {
		t.Fatalf("expected 2 upsert calls, got %d", len(upserter.upserted))
	}
	id1 := upserter.upserted[0].SourceID
	id2 := upserter.upserted[1].SourceID
	if id1 != id2 {
		t.Errorf("SourceID mismatch (not idempotent): first=%q second=%q", id1, id2)
	}
	if len(id1) < 9 || id1[:9] != "call-log:" {
		t.Errorf("SourceID %q does not have expected 'call-log:' prefix", id1)
	}
}

// TestIngestRecording_VoiceMemoSameFileDifferentDateMs is the key regression test
// for the idempotency fix: uploading the same original filename with a different
// date_ms (e.g. midnight vs. actual recording time) must yield the same SourceID.
// Previously this produced two DB records; after the fix the source_id no longer
// embeds dateMs so both uploads collapse to the same identifier.
func TestIngestRecording_VoiceMemoSameFileDifferentDateMs(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv, _ := newRecordingTestServer(t, upserter, "", 0, time.Time{})

	filename := "음성 260608_애니.m4a"
	// First upload: client sends midnight timestamp (legacy behaviour).
	midnightMs := int64(1749340800000) // 2025-06-08 00:00:00 UTC
	// Second upload: client sends actual recording time on the same day.
	actualMs := int64(1749361234000) // 2025-06-08 05:40:34 UTC

	b1, ct1 := buildRecordingForm(t, filename, validM4ABytes(32), "", midnightMs, "kind", "voice-memo")
	rr1 := doRecordingPost(t, srv, b1, ct1, "Bearer test-key")
	if rr1.Code != http.StatusCreated {
		t.Fatalf("first upload: status = %d, want 201; body: %s", rr1.Code, rr1.Body.String())
	}

	b2, ct2 := buildRecordingForm(t, filename, validM4ABytes(32), "", actualMs, "kind", "voice-memo")
	rr2 := doRecordingPost(t, srv, b2, ct2, "Bearer test-key")
	if rr2.Code != http.StatusCreated {
		t.Fatalf("second upload: status = %d, want 201; body: %s", rr2.Code, rr2.Body.String())
	}

	if len(upserter.upserted) != 2 {
		t.Fatalf("expected 2 upsert calls, got %d", len(upserter.upserted))
	}
	id1 := upserter.upserted[0].SourceID
	id2 := upserter.upserted[1].SourceID

	// Core assertion: same filename, different dateMs → identical SourceID.
	if id1 != id2 {
		t.Errorf("idempotency broken by dateMs change: midnight=%q actual=%q", id1, id2)
	}

	// Sanity: OccurredAt must still differ — the timestamp is stored on the document.
	oat1 := upserter.upserted[0].OccurredAt
	oat2 := upserter.upserted[1].OccurredAt
	if oat1 == nil || oat2 == nil {
		t.Fatal("OccurredAt must not be nil")
	}
	if oat1.Equal(*oat2) {
		t.Error("OccurredAt should differ between the two uploads (midnight vs actual time)")
	}
}

// TestIngestRecording_VoiceMemoSameDateMsDifferentFilename verifies that two
// voice-memo uploads sharing the same dateMs but carrying different original
// filenames produce distinct SourceIDs and are stored as separate DB records.
func TestIngestRecording_VoiceMemoSameDateMsDifferentFilename(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv, recordingDir := newRecordingTestServer(t, upserter, "", 0, time.Time{})

	// Both recordings are "from" the same day — clients often pass midnight ms.
	dateMs := int64(1749340800000) // 2025-06-08 00:00:00 UTC (midnight, same for all same-day memos)

	// Upload first voice-memo.
	b1, ct1 := buildRecordingForm(t, "음성 260608_애니.m4a", validM4ABytes(32), "", dateMs, "kind", "voice-memo")
	rr1 := doRecordingPost(t, srv, b1, ct1, "Bearer test-key")
	if rr1.Code != http.StatusCreated {
		t.Fatalf("first upload: status = %d, want 201; body: %s", rr1.Code, rr1.Body.String())
	}

	// Upload second voice-memo with different filename but same dateMs.
	b2, ct2 := buildRecordingForm(t, "260608_JTF회의_2.m4a", validM4ABytes(32), "", dateMs, "kind", "voice-memo")
	rr2 := doRecordingPost(t, srv, b2, ct2, "Bearer test-key")
	if rr2.Code != http.StatusCreated {
		t.Fatalf("second upload: status = %d, want 201; body: %s", rr2.Code, rr2.Body.String())
	}

	if len(upserter.upserted) != 2 {
		t.Fatalf("expected 2 upserted docs, got %d", len(upserter.upserted))
	}

	id1 := upserter.upserted[0].SourceID
	id2 := upserter.upserted[1].SourceID

	// Different filenames must produce different SourceIDs.
	if id1 == id2 {
		t.Errorf("SourceID collision: both uploads produced %q", id1)
	}

	// Both must use the call-log prefix.
	for _, id := range []string{id1, id2} {
		if !strings.HasPrefix(id, "call-log:") {
			t.Errorf("SourceID %q missing call-log: prefix", id)
		}
	}

	// Both audio files must be written to disk (sidecar files excluded).
	audioFiles := audioFilesInDir(t, recordingDir)
	if len(audioFiles) != 2 {
		t.Fatalf("expected 2 audio files on disk, got %d", len(audioFiles))
	}
}

// TestIngestRecording_VoiceMemoSameFilenameIdempotent verifies that uploading
// the same voice-memo file twice produces the same SourceID (idempotent upsert)
// even when dateMs is identical between uploads.
func TestIngestRecording_VoiceMemoSameFilenameIdempotent(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv, _ := newRecordingTestServer(t, upserter, "", 0, time.Time{})

	dateMs := int64(1749340800000) // 2025-06-08 00:00:00 UTC

	upload := func() string {
		b, ct := buildRecordingForm(t, "음성 260608_애니.m4a", validM4ABytes(32), "", dateMs, "kind", "voice-memo")
		rr := doRecordingPost(t, srv, b, ct, "Bearer test-key")
		if rr.Code != http.StatusCreated {
			t.Fatalf("upload: status = %d, want 201; body: %s", rr.Code, rr.Body.String())
		}
		var resp IngestRecordingResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return upserter.upserted[len(upserter.upserted)-1].SourceID
	}

	id1 := upload()
	id2 := upload()

	if id1 != id2 {
		t.Errorf("idempotency broken: first=%q second=%q", id1, id2)
	}
}

// TestIngestRecording_CorruptM4AIsRejected verifies that a file whose content
// is obviously-corrupt (4096 bytes of zeros, no ftyp box) is rejected with
// HTTP 400 and NOT written to disk.
//
// This guards against the production scenario where the mobile app uploads a
// 4096-byte garbage .m4a. Without this guard the file would be written to disk
// and then retried every minute by WhisperCollector, spamming 500 errors from
// the whisper server (av.error.InvalidDataError).
func TestIngestRecording_CorruptM4AIsRejected(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv, recordingDir := newRecordingTestServer(t, upserter, "", 0, time.Time{})

	// 4096-byte all-zero payload — exactly the garbage uploaded by the mobile app.
	garbageAudio := make([]byte, 4096)
	dateMs := time.Now().Add(-time.Hour).UnixMilli()

	body, ct := buildRecordingForm(t, "recording.m4a", garbageAudio, "010-1234-5678", dateMs)
	rr := doRecordingPost(t, srv, body, ct, "Bearer test-key")

	// Must be rejected with 400 so the mobile client marks the recording as
	// PerFileClientError and stops retrying (contract with Android app).
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusBadRequest, rr.Body.String())
	}

	// No document must be stored.
	if len(upserter.upserted) != 0 {
		t.Errorf("expected 0 upserted docs for corrupt file, got %d", len(upserter.upserted))
	}

	// No file must be written to disk.
	entries, err := os.ReadDir(recordingDir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 files on disk for corrupt upload, got %d: %v",
			len(entries), entries[0].Name())
	}
}

// TestIngestRecording_TooSmallIsRejected verifies that files below the minimum
// viable audio size (< 8 bytes) are rejected with HTTP 400.
func TestIngestRecording_TooSmallIsRejected(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv, recordingDir := newRecordingTestServer(t, upserter, "", 0, time.Time{})

	tinyAudio := []byte{0x00, 0x01, 0x02} // 3 bytes — too small
	dateMs := time.Now().Add(-time.Hour).UnixMilli()

	body, ct := buildRecordingForm(t, "tiny.m4a", tinyAudio, "010-1234-5678", dateMs)
	rr := doRecordingPost(t, srv, body, ct, "Bearer test-key")

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusBadRequest, rr.Body.String())
	}

	// No file must be written to disk.
	entries, _ := os.ReadDir(recordingDir)
	if len(entries) != 0 {
		t.Errorf("expected 0 files on disk for tiny upload, got %d", len(entries))
	}
}

// TestIngestRecording_ValidM4AIsAccepted verifies that a file with a proper
// ftyp box at offset 4 is accepted normally (regression guard: validation must
// not reject valid files).
func TestIngestRecording_ValidM4AIsAccepted(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv, recordingDir := newRecordingTestServer(t, upserter, "", 0, time.Time{})

	// Minimal valid m4a header: 8+ bytes with "ftyp" at offset 4.
	validM4A := make([]byte, 32)
	copy(validM4A[4:8], []byte("ftyp"))

	dateMs := time.Now().Add(-time.Hour).UnixMilli()
	body, ct := buildRecordingForm(t, "valid.m4a", validM4A, "010-1234-5678", dateMs)
	rr := doRecordingPost(t, srv, body, ct, "Bearer test-key")

	if rr.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d (valid m4a must be accepted); body: %s",
			rr.Code, http.StatusCreated, rr.Body.String())
	}

	if audioFiles := audioFilesInDir(t, recordingDir); len(audioFiles) != 1 {
		t.Errorf("expected 1 audio file on disk for valid upload, got %d", len(audioFiles))
	}
}

// TestIngestRecording_NonM4ANotValidated verifies that non-m4a extensions (e.g.
// .mp3, .wav) are not subject to the ftyp check and are accepted as-is.
func TestIngestRecording_NonM4ANotValidated(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv, recordingDir := newRecordingTestServer(t, upserter, "", 0, time.Time{})

	// RIFF-style header (wav) — valid audio but no ftyp box.
	wavData := []byte{'R', 'I', 'F', 'F', 'W', 'A', 'V', 'E', 0x00, 0x00}
	dateMs := time.Now().Add(-time.Hour).UnixMilli()

	body, ct := buildRecordingForm(t, "audio.wav", wavData, "010-1234-5678", dateMs)
	rr := doRecordingPost(t, srv, body, ct, "Bearer test-key")

	if rr.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d (non-m4a must not be rejected by ftyp check); body: %s",
			rr.Code, http.StatusCreated, rr.Body.String())
	}

	if audioFiles := audioFilesInDir(t, recordingDir); len(audioFiles) != 1 {
		t.Errorf("expected 1 audio file on disk for wav upload, got %d", len(audioFiles))
	}
}

// TestSanitizePhoneNumber verifies that sanitizePhoneNumber retains digits, '+',
// and '-', and returns "unknown" for empty results.
func TestSanitizePhoneNumber(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want string
	}{
		{"010-1234-5678", "010-1234-5678"},
		{"+821012345678", "+821012345678"},
		// Parentheses and spaces are stripped; no hyphens in original → none in output.
		{"(010) 1234 5678", "01012345678"},
		{"abc", "unknown"},
		{"", "unknown"},
		// Dots are stripped; only digits remain.
		{"010.1234.5678", "01012345678"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got := sanitizePhoneNumber(tc.in)
			if got != tc.want {
				t.Errorf("sanitizePhoneNumber(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
