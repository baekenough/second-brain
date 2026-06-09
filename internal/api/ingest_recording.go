package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/baekenough/second-brain/internal/collector/smsmap"
	"github.com/baekenough/second-brain/internal/model"
)

// defaultIngestRecordingMaxFileBytes is the per-upload size cap for recording
// uploads. Defaults to the same value as defaultIngestMaxFileBytes (100 MiB)
// unless overridden via WithIngestRecording.
//
// Reuses INGEST_MAX_FILE_BYTES (already parsed by callers) so operators have
// one knob for all ingest endpoints.
const defaultIngestRecordingMaxFileBytes = 100 << 20 // 100 MiB

// IngestRecordingUpserter is the document persistence interface required by the
// ingest-recording handler. *store.DocumentStore satisfies this interface.
type IngestRecordingUpserter interface {
	Upsert(ctx context.Context, doc *model.Document) error
}

// WithIngestRecording attaches the dependencies required by
// POST /api/v1/ingest/recording and enables the route.
//
// recordingDir is the directory where uploaded audio files are written; it must
// be non-empty for the route to be active. Corresponds to cfg.IngestRecordingDir.
//
// maxFileBytes is the per-upload size cap (bytes); 0 uses the package default
// (100 MiB). Pass cfg.IngestMaxFileBytes here.
//
// cutover is the optional floor time from cfg.CollectorCutover: recordings whose
// parsed timestamp is before this value are silently skipped (accepted=false,
// skipped=true). Zero time.Time{} disables the floor.
//
// Must be called before the first call to Handler().
func (s *Server) WithIngestRecording(
	upserter IngestRecordingUpserter,
	recordingDir string,
	maxFileBytes int64,
	cutover time.Time,
) *Server {
	s.recordingUpserter = upserter
	s.recordingDir = recordingDir
	if maxFileBytes <= 0 {
		s.recordingMaxFileBytes = defaultIngestRecordingMaxFileBytes
	} else {
		s.recordingMaxFileBytes = maxFileBytes
	}
	s.recordingCutover = cutover
	return s
}

// IngestRecordingResponse is the JSON body returned on a successful request.
type IngestRecordingResponse struct {
	Accepted   bool   `json:"accepted"`
	Skipped    bool   `json:"skipped"`
	DocumentID string `json:"document_id,omitempty"`
}

// ingestRecordingHandler handles POST /api/v1/ingest/recording.
//
// Accepts multipart/form-data with:
//   - file         (required) — audio file (.m4a recommended; any audio ext accepted)
//   - number       (required) — caller/callee phone number
//   - date_ms      (required) — Unix millisecond timestamp of the recording
//   - duration_sec (optional) — call duration in seconds (default 0)
//   - contact_name (optional) — display name of the contact
//
// Behaviour:
//  1. Validate form fields and size cap.
//  2. Apply cutover floor: recordings with a timestamp before recordingCutover
//     are skipped (accepted=false, skipped=true, HTTP 200).
//  3. Write the audio file to recordingDir with a filename encoding the
//     recording timestamp in TPhoneCallRecords format so WhisperCollector's
//     recordingTime() / cutover logic works:
//     "{sanitized-number}_{YYYYMMDDHHMMSS}.{ext}"
//  4. Create a PENDING call-log model.Document (SourceType=SourceCallLog) with
//     "[TRANSCRIPTION PENDING]" in the content and idempotently upsert it.
//     WhisperCollector transcribes the audio on its next scheduled run.
//  5. The upsert is idempotent: same number + date_ms + duration_sec → same SourceID
//     (mirrors smsmap.MapCall SourceID format).
//
// Returns 201 Created on success, 200 when skipped by cutover floor, appropriate
// error codes otherwise.
func (s *Server) ingestRecordingHandler(w http.ResponseWriter, r *http.Request) {
	if s.recordingUpserter == nil || s.recordingDir == "" {
		writeError(w, http.StatusServiceUnavailable, "recording ingest not configured")
		return
	}

	// Enforce the per-upload size limit.
	maxBytes := s.recordingMaxFileBytes
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		if isMaxBytesError(err) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("file exceeds maximum upload size of %d bytes", maxBytes))
			return
		}
		writeError(w, http.StatusBadRequest, "invalid multipart form")
		return
	}

	// --- Validate required form fields ---
	number := r.FormValue("number")
	if number == "" {
		writeError(w, http.StatusBadRequest, "field 'number' is required")
		return
	}
	dateMsStr := r.FormValue("date_ms")
	if dateMsStr == "" {
		writeError(w, http.StatusBadRequest, "field 'date_ms' is required")
		return
	}
	var dateMs int64
	if _, err := fmt.Sscanf(dateMsStr, "%d", &dateMs); err != nil || dateMs == 0 {
		writeError(w, http.StatusBadRequest, "field 'date_ms' must be a valid Unix millisecond timestamp")
		return
	}

	var durationSec int
	if ds := r.FormValue("duration_sec"); ds != "" {
		fmt.Sscanf(ds, "%d", &durationSec) //nolint:errcheck // 0 is an acceptable default
	}
	contactName := r.FormValue("contact_name")

	// --- Read the audio file part ---
	f, fh, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "field 'file' is required")
		return
	}
	defer f.Close() //nolint:errcheck // best-effort cleanup

	// --- Cutover floor check ---
	recordedAt := time.UnixMilli(dateMs).UTC()
	if !s.recordingCutover.IsZero() && recordedAt.Before(s.recordingCutover) {
		writeJSON(w, http.StatusOK, IngestRecordingResponse{
			Accepted: false,
			Skipped:  true,
		})
		return
	}

	// --- Determine audio file extension ---
	ext := ".m4a" // default
	if fh != nil && fh.Filename != "" {
		if e := filepath.Ext(fh.Filename); e != "" {
			ext = e
		}
	}

	// --- Build output filename (TPhoneCallRecords pattern) ---
	// Format: {sanitized-number}_{YYYYMMDDHHMMSS}{ext}
	// WhisperCollector.recordingTime() parses this pattern to extract the
	// recording timestamp, so existing cutover / watermark logic works.
	localTime := recordedAt.In(time.Local)
	audioFilename := fmt.Sprintf("%s_%s%s",
		sanitizePhoneNumber(number),
		localTime.Format("20060102150405"),
		ext,
	)

	// --- Build stable SourceID (mirrors smsmap.MapCall) ---
	numHash := smsmap.ShortHash(number)
	durHash := smsmap.BodyShortHash(fmt.Sprintf("%d", durationSec))
	sourceID := fmt.Sprintf("call-log:%d:%s:%s", dateMs, numHash, durHash)

	// --- Ensure the recording directory exists ---
	if err := os.MkdirAll(s.recordingDir, 0o755); err != nil {
		slog.Error("ingest_recording: mkdir failed", "dir", s.recordingDir, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	// --- Write audio file ---
	destPath := filepath.Join(s.recordingDir, audioFilename)
	destFile, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		slog.Error("ingest_recording: open dest file", "path", destPath, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	if _, err := io.Copy(destFile, f); err != nil {
		_ = destFile.Close()
		_ = os.Remove(destPath) // clean up partial file
		if isMaxBytesError(err) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("file exceeds maximum upload size of %d bytes", maxBytes))
			return
		}
		slog.Error("ingest_recording: write audio file", "path", destPath, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if err := destFile.Close(); err != nil {
		// Sync / close failure is unexpected but non-fatal for the response.
		slog.Warn("ingest_recording: close audio file (non-fatal)", "path", destPath, "error", err)
	}

	// --- Create PENDING call-log document ---
	// WhisperCollector transcribes the audio later. The document acts as a
	// placeholder so the call is immediately discoverable via search / metadata.
	contact := contactName
	if contact == "" {
		contact = number
	}
	title := fmt.Sprintf("incoming 통화 %s", contact)
	content := fmt.Sprintf("상대방: %s\n통화 방향: incoming\n통화 시간: %ds\n[TRANSCRIPTION PENDING]",
		contact, durationSec)

	meta := map[string]any{
		"contact_name":     contactName,
		"direction":        "incoming",
		"duration_seconds": durationSec,
		"audio_file":       audioFilename,
		"transcription":    "pending",
	}
	t := recordedAt
	doc := &model.Document{
		SourceType:  model.SourceCallLog,
		SourceID:    sourceID,
		Title:       title,
		Content:     content,
		Metadata:    meta,
		Status:      "active",
		OccurredAt:  &t,
		CollectedAt: time.Now().UTC(),
	}

	if err := s.recordingUpserter.Upsert(r.Context(), doc); err != nil {
		// Audio is already written — log the doc failure but don't lose the file.
		slog.Error("ingest_recording: upsert failed",
			"source_id", sourceID, "audio", destPath, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	writeJSON(w, http.StatusCreated, IngestRecordingResponse{
		Accepted:   true,
		Skipped:    false,
		DocumentID: doc.ID.String(),
	})
}

// sanitizePhoneNumber strips characters that are unsafe in filenames from a
// phone number, retaining only digits, '+', and '-'.
// Returns "unknown" when the result is empty.
func sanitizePhoneNumber(number string) string {
	out := make([]byte, 0, len(number))
	for i := 0; i < len(number); i++ {
		c := number[i]
		if (c >= '0' && c <= '9') || c == '+' || c == '-' {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return "unknown"
	}
	return string(out)
}
