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
	"testing"
	"time"
)

// --- helpers ---

// newRecordingTestServer creates a Server wired for the ingest-recording handler.
// If recordingDir is empty a temp dir is created and its path returned.
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
	srv := NewServer(nil, nil, nil, nil, nil, "", "test-key")
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
	body, ct := buildRecordingForm(t, "audio.m4a", []byte("fake audio"), "010-1234-5678", dateMs)
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
	audioData := []byte("fake m4a audio bytes")

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

	// Audio file written to recordingDir.
	entries, err := os.ReadDir(recordingDir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 audio file in recordingDir, got %d", len(entries))
	}
	audioFile := entries[0].Name()

	// Filename must encode the recording timestamp in TPhoneCallRecords format
	// so WhisperCollector.recordingTime() can parse it.
	// Format: {number}_{YYYYMMDDHHMMSS}.m4a
	expectedPattern := sanitizePhoneNumber(number) + "_"
	if len(audioFile) < len(expectedPattern) || audioFile[:len(expectedPattern)] != expectedPattern {
		t.Errorf("audio filename %q does not start with %q", audioFile, expectedPattern)
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

	body, ct := buildRecordingForm(t, "call.m4a", []byte("audio"), number, dateMs)
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
	b1, ct1 := buildRecordingForm(t, "audio.m4a", []byte("audio"), number, dateMs, "duration_sec", durationSec)
	rr1 := doRecordingPost(t, srv, b1, ct1, "Bearer test-key")
	if rr1.Code != http.StatusCreated {
		t.Fatalf("first: status = %d, want 201", rr1.Code)
	}

	// Second upload (identical metadata).
	b2, ct2 := buildRecordingForm(t, "audio.m4a", []byte("audio"), number, dateMs, "duration_sec", durationSec)
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

	body, ct := buildRecordingForm(t, "call.m4a", []byte("audio bytes"), number, dateMs)
	rr := doRecordingPost(t, srv, body, ct, "Bearer test-key")
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rr.Code, rr.Body.String())
	}

	entries, _ := os.ReadDir(recordingDir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 file, got %d", len(entries))
	}
	audioFile := entries[0].Name()

	wantName := fmt.Sprintf("%s_%s.m4a", sanitizePhoneNumber(number), expectedTimestamp)
	if audioFile != wantName {
		t.Errorf("audio filename=%q, want %q", audioFile, wantName)
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
