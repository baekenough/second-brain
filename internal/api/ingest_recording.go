package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/baekenough/second-brain/internal/audiovalidate"
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
//   - kind         (optional) — "call" (default) or "voice-memo"
//   - number       (required for kind=call) — caller/callee phone number
//   - date_ms      (required) — Unix millisecond timestamp of the recording
//   - duration_sec (optional) — call duration in seconds (default 0)
//   - contact_name (optional) — display name of the contact
//
// Behaviour:
//  1. Validate form fields and size cap.
//  2. Apply cutover floor: recordings with a timestamp before recordingCutover
//     are skipped (accepted=false, skipped=true, HTTP 200).
//  3. Write the audio file to recordingDir with a filename encoding the
//     recording timestamp so WhisperCollector's recordingTime() / cutover logic
//     works:
//     - call:       "{sanitized-number}_{YYYYMMDDHHMMSS}.{ext}"
//     - voice-memo: "voice-memo_{YYYYMMDDHHMMSS}.{ext}"
//  4. Create a PENDING call-log model.Document (SourceType=SourceCallLog) with
//     "[TRANSCRIPTION PENDING]" in the content and idempotently upsert it.
//     WhisperCollector transcribes the audio on its next scheduled run.
//  5. The upsert is idempotent: same inputs → same SourceID.
//     - call:       call-log:{date_ms}:{numHash}:{durHash}  (mirrors smsmap.MapCall)
//     - voice-memo: call-log:voice-memo:{hash(originalFilename)}
//       Hash is over the original upload filename only — dateMs is excluded so
//       that the same file re-uploaded with a different timestamp produces the
//       same SourceID (fully idempotent). Different filenames still produce
//       distinct IDs.
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

	// --- Validate kind field (default: "call" for backward compatibility) ---
	kind := r.FormValue("kind")
	if kind == "" {
		kind = "call"
	}
	if kind != "call" && kind != "voice-memo" {
		writeError(w, http.StatusBadRequest, "field 'kind' must be 'call' or 'voice-memo'")
		return
	}

	// --- Validate required form fields ---
	number := r.FormValue("number")
	if kind == "call" && number == "" {
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

	// --- Cutover floor check (before reading file bytes) ---
	// Skip recordings that pre-date the floor without touching the file data.
	recordedAt := time.UnixMilli(dateMs).UTC()
	if !s.recordingCutover.IsZero() && recordedAt.Before(s.recordingCutover) {
		writeJSON(w, http.StatusOK, IngestRecordingResponse{
			Accepted: false,
			Skipped:  true,
		})
		return
	}

	// --- Read the audio file part ---
	f, fh, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "field 'file' is required")
		return
	}
	defer f.Close() //nolint:errcheck // best-effort cleanup

	// Read the entire upload into memory so we can:
	//  (a) validate the audio header before touching the disk, and
	//  (b) write atomically from memory (avoiding partial files on disk when
	//      the request is rejected after the file was already opened).
	//
	// maxBytes is already enforced by MaxBytesReader above, so this read is
	// bounded. For typical call recordings (< 100 MiB default) this is fine.
	audioBytes, err := io.ReadAll(f)
	if err != nil {
		if isMaxBytesError(err) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("file exceeds maximum upload size of %d bytes", maxBytes))
			return
		}
		slog.Error("ingest_recording: read audio bytes", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	// --- Validate audio integrity (defence-in-depth layer 1) ---
	//
	// Reject obviously-corrupt files before writing to disk. Without this guard
	// a 4096-byte all-zero garbage .m4a uploaded by the mobile app would be
	// written to disk and then retried every minute by WhisperCollector, flooding
	// the whisper server with decode failures (av.error.InvalidDataError).
	//
	// HTTP 400 is intentional: the Android app interprets 4xx as PerFileClientError
	// and calls markRecordingSent(), permanently stopping retries for that file.
	//
	// m4a/mp4 containers must carry an "ftyp" box at bytes[4:8]; all other audio
	// formats only require the minimum-length check.
	uploadedFilename := ""
	if fh != nil {
		uploadedFilename = fh.Filename
	}
	fileExt := strings.ToLower(filepath.Ext(uploadedFilename))
	var validationErr error
	switch fileExt {
	case ".m4a", ".mp4":
		validationErr = audiovalidate.CheckM4A(audioBytes)
	default:
		validationErr = audiovalidate.CheckAudioBytes(audioBytes)
	}
	if validationErr != nil {
		slog.Warn("ingest_recording: rejecting corrupt audio upload",
			"filename", uploadedFilename,
			"size_bytes", len(audioBytes),
			"reason", validationErr,
		)
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("audio file appears corrupt or invalid: %v", validationErr))
		return
	}

	// --- Determine audio file extension ---
	ext := ".m4a" // default
	if fh != nil && fh.Filename != "" {
		if e := filepath.Ext(fh.Filename); e != "" {
			ext = e
		}
	}

	// --- Build output filename ---
	// WhisperCollector.recordingTime() parses the timestamp suffix to extract the
	// recording timestamp, so existing cutover / watermark logic works for both kinds.
	localTime := recordedAt.In(time.Local)
	timestampStr := localTime.Format("20060102150405")

	var audioFilename string
	var sourceID string

	switch kind {
	case "voice-memo":
		// SourceID is derived from the original upload filename so that two different
		// recordings with the same dateMs (e.g. midnight of the same day) produce
		// distinct source IDs while re-uploading the same file remains idempotent.
		//
		// Fallback when the client sends no filename: use dateMs + file size so the
		// ID is still stable for a given upload and reasonably unique across uploads.
		originalFilename := ""
		if fh != nil {
			originalFilename = fh.Filename
		}
		var idBase string
		if originalFilename != "" {
			idBase = originalFilename
		} else {
			idBase = fmt.Sprintf("%d", dateMs)
		}
		filenameHash := smsmap.BodyShortHash(idBase)
		sourceID = fmt.Sprintf("call-log:voice-memo:%s", filenameHash)

		// Audio filename on disk: sanitize the original filename to stay ASCII-safe
		// while keeping the timestamp prefix for WhisperCollector.recordingTime().
		// Format: voice-memo_{YYYYMMDDHHMMSS}_{sanitizedOriginal}{ext}
		// Same original filename → same disk path (idempotent overwrite OK).
		if originalFilename != "" {
			baseName := strings.TrimSuffix(originalFilename, filepath.Ext(originalFilename))
			sanitized := sanitizeFilename(baseName)
			if sanitized == "" {
				sanitized = filenameHash
			}
			audioFilename = fmt.Sprintf("voice-memo_%s_%s%s", timestampStr, sanitized, ext)
		} else {
			// No original filename: fall back to timestamp-only form (legacy behaviour).
			audioFilename = fmt.Sprintf("voice-memo_%s%s", timestampStr, ext)
		}
	default: // "call"
		// Format: {sanitized-number}_{YYYYMMDDHHMMSS}{ext}
		audioFilename = fmt.Sprintf("%s_%s%s",
			sanitizePhoneNumber(number),
			timestampStr,
			ext,
		)
		// Mirrors smsmap.MapCall SourceID format.
		numHash := smsmap.ShortHash(number)
		durHash := smsmap.BodyShortHash(fmt.Sprintf("%d", durationSec))
		sourceID = fmt.Sprintf("call-log:%d:%s:%s", dateMs, numHash, durHash)
	}

	// --- Ensure the recording directory exists ---
	if err := os.MkdirAll(s.recordingDir, 0o755); err != nil {
		slog.Error("ingest_recording: mkdir failed", "dir", s.recordingDir, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	// --- Write audio file ---
	// audioBytes was validated above; write from memory to disk.
	destPath := filepath.Join(s.recordingDir, audioFilename)
	if err := os.WriteFile(destPath, audioBytes, 0o644); err != nil {
		slog.Error("ingest_recording: write audio file", "path", destPath, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	// --- Build document title, content, and metadata by kind ---
	var title, content string
	var meta map[string]any

	switch kind {
	case "voice-memo":
		// Derive a human-readable label: contact name if available, otherwise
		// the original filename without extension.
		label := contactName
		if label == "" && fh != nil && fh.Filename != "" {
			label = strings.TrimSuffix(fh.Filename, filepath.Ext(fh.Filename))
		}
		if label == "" {
			label = timestampStr
		}
		title = fmt.Sprintf("음성메모 %s", label)
		content = fmt.Sprintf("레이블: %s\n녹음 시간: %ds\n[TRANSCRIPTION PENDING]",
			label, durationSec)
		meta = map[string]any{
			"contact_name":     contactName,
			"recording_type":   "voice-memo",
			"duration_seconds": durationSec,
			"audio_file":       audioFilename,
			"transcription":    "pending",
		}
	default: // "call"
		contact := contactName
		if contact == "" {
			contact = number
		}
		title = fmt.Sprintf("incoming 통화 %s", contact)
		content = fmt.Sprintf("상대방: %s\n통화 방향: incoming\n통화 시간: %ds\n[TRANSCRIPTION PENDING]",
			contact, durationSec)
		meta = map[string]any{
			"contact_name":     contactName,
			"direction":        "incoming",
			"recording_type":   "call",
			"duration_seconds": durationSec,
			"audio_file":       audioFilename,
			"transcription":    "pending",
		}
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

// sanitizeFilename converts an arbitrary string (e.g. a user-supplied audio
// filename stem) into a filesystem-safe ASCII slug. It replaces whitespace and
// Unicode letters/digits that round-trip through ASCII with underscores so the
// result is portable across file systems and safe to embed in SourceID strings.
//
// Rules:
//   - ASCII letters and digits are kept as-is.
//   - Spaces are replaced with underscores.
//   - Any other character (non-ASCII, punctuation other than '-' and '_') is
//     replaced with an underscore.
//   - Runs of underscores are collapsed to a single underscore.
//   - Leading/trailing underscores are trimmed.
//
// Returns "" when the result is empty (caller must supply a fallback).
func sanitizeFilename(s string) string {
	out := make([]byte, 0, len(s))
	prev := byte(0)
	for _, r := range s {
		var b byte
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b = byte(r)
		case r == '-':
			b = '-'
		case unicode.IsSpace(r) || r == '_' || r > 127:
			b = '_'
		default:
			b = '_'
		}
		// Collapse consecutive underscores.
		if b == '_' && prev == '_' {
			continue
		}
		out = append(out, b)
		prev = b
	}
	// Trim leading/trailing underscores.
	result := strings.Trim(string(out), "_")
	return result
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
