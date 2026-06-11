package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/audiovalidate"
	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/model"
)

// whisperDefaultHTTPTimeout is the fallback per-request timeout used when the
// config value is zero (misconfigured). 2 hours covers long audio recordings
// without producing an infinite (zero) timeout. Prefer cfg.WhisperHTTPTimeout
// over this constant; this constant exists only as a defence-in-depth fallback.
const whisperDefaultHTTPTimeout = 2 * time.Hour

// whisperAudioExts is the set of audio file extensions that the collector will
// submit for transcription. Extensions are lowercase and include the leading dot.
var whisperAudioExts = map[string]bool{
	".m4a":  true,
	".mp3":  true,
	".wav":  true,
	".aac":  true,
	".flac": true,
	".ogg":  true,
	".opus": true,
	".webm": true,
	".wma":  true,
	".aiff": true,
	".mp4":  true,
	".oga":  true,
}

// whisperTranscribeResponse is the JSON response body from the Whisper
// transcription endpoint (/v1/audio/transcriptions) when response_format is
// NOT set (simple text response). Kept for backward-compatibility.
type whisperTranscribeResponse struct {
	Text string `json:"text"`
}

// whisperSegment is a single time-stamped segment from the Whisper
// verbose_json response format.
type whisperSegment struct {
	Start float64 `json:"start"` // segment start time in seconds
	End   float64 `json:"end"`   // segment end time in seconds
	Text  string  `json:"text"`  // transcribed text for this segment
}

// whisperVerboseResponse is the JSON response body when response_format=verbose_json
// is requested. The Segments field may be absent/empty for older server versions,
// in which case callers fall back to the flat Text field.
type whisperVerboseResponse struct {
	Text     string           `json:"text"`
	Segments []whisperSegment `json:"segments"`
}

// diarSegment is a single speaker-labelled segment from the diarization
// microservice (POST {DIARIZATION_API_URL}/diarize).
type diarSegment struct {
	Start   float64 `json:"start"`   // segment start time in seconds
	End     float64 `json:"end"`     // segment end time in seconds
	Speaker string  `json:"speaker"` // e.g. "SPEAKER_00", "SPEAKER_01"
}

// diarizeResponse is the JSON body returned by the diarization microservice.
type diarizeResponse struct {
	Segments []diarSegment `json:"segments"`
}

// labelTranscript aligns whisper transcript segments with speaker diarization
// segments and returns speaker-labelled content and the number of distinct
// speakers.
//
// Alignment strategy:
//   - For each whisper segment, find the diarization speaker whose time
//     interval has the maximum overlap (overlap = max(0, min(ends)-max(starts))).
//   - When no diarization segment overlaps a whisper segment at all, the
//     nearest diarization speaker by midpoint distance is assigned.
//   - Diarization speaker IDs (e.g. "SPEAKER_00") are mapped to 화자1, 화자2, …
//     in order of first appearance across the whisper segments.
//
// Consecutive whisper segments assigned to the same speaker are grouped into a
// single block; blocks are rendered as "[화자N] text" and separated by newlines.
//
// If whisperSegs or diarSegs is empty the function returns ("", 0) so callers
// can fall back to the plain transcript text.
func labelTranscript(whisperSegs []whisperSegment, diarSegs []diarSegment) (content string, speakerCount int) {
	if len(whisperSegs) == 0 || len(diarSegs) == 0 {
		return "", 0
	}

	// speakerMap maps raw diarization speaker IDs → 화자N labels in order of
	// first appearance. A slice is used to keep insertion-order traversal O(n).
	speakerMap := map[string]string{}
	var speakerOrder []string // raw IDs in order of first appearance

	speakerLabel := func(rawID string) string {
		if label, ok := speakerMap[rawID]; ok {
			return label
		}
		label := fmt.Sprintf("화자%d", len(speakerOrder)+1)
		speakerMap[rawID] = label
		speakerOrder = append(speakerOrder, rawID)
		return label
	}

	// assignSpeaker returns the diarization speaker for a given whisper segment.
	assignSpeaker := func(ws whisperSegment) string {
		bestSpeaker := ""
		bestOverlap := -1.0 // negative sentinel: no overlap found yet

		for _, ds := range diarSegs {
			overlapStart := ws.Start
			if ds.Start > overlapStart {
				overlapStart = ds.Start
			}
			overlapEnd := ws.End
			if ds.End < overlapEnd {
				overlapEnd = ds.End
			}
			overlap := overlapEnd - overlapStart
			if overlap < 0 {
				overlap = 0
			}
			if overlap > bestOverlap {
				bestOverlap = overlap
				bestSpeaker = ds.Speaker
			}
		}

		// If no diarization segment overlapped (bestOverlap == 0), fall back to
		// the nearest segment by midpoint distance.
		if bestOverlap <= 0 {
			wsMid := (ws.Start + ws.End) / 2
			minDist := -1.0
			for _, ds := range diarSegs {
				dsMid := (ds.Start + ds.End) / 2
				dist := wsMid - dsMid
				if dist < 0 {
					dist = -dist
				}
				if minDist < 0 || dist < minDist {
					minDist = dist
					bestSpeaker = ds.Speaker
				}
			}
		}

		return bestSpeaker
	}

	// Assign each whisper segment a speaker label.
	type labelledSeg struct {
		speaker string
		text    string
	}
	labelled := make([]labelledSeg, len(whisperSegs))
	for i, ws := range whisperSegs {
		raw := assignSpeaker(ws)
		labelled[i] = labelledSeg{
			speaker: speakerLabel(raw),
			text:    strings.TrimSpace(ws.Text),
		}
	}

	// Group consecutive segments with the same speaker into blocks.
	var sb strings.Builder
	blockSpeaker := labelled[0].speaker
	var blockTexts []string

	flushBlock := func() {
		if len(blockTexts) == 0 {
			return
		}
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString("[")
		sb.WriteString(blockSpeaker)
		sb.WriteString("] ")
		sb.WriteString(strings.Join(blockTexts, " "))
	}

	for _, seg := range labelled {
		if seg.speaker != blockSpeaker {
			flushBlock()
			blockSpeaker = seg.speaker
			blockTexts = blockTexts[:0]
		}
		if seg.text != "" {
			blockTexts = append(blockTexts, seg.text)
		}
	}
	flushBlock()

	return sb.String(), len(speakerOrder)
}

// Filename patterns for recording timestamps.
//
// Pattern A — Voice Recorder app: <label>_YYMMDD_HHMMSS.<ext>
//
//	e.g. 메디웨일_260120_120138.m4a  →  2026-01-20 12:01:38
//	The 2-digit year is normalised with a 2000 offset (so "26" → 2026).
//
// Pattern B — TPhoneCallRecords: <number>_YYYYMMDDHHMMSS[{-N}].<ext>
//
//	e.g. 01025777190_20260327202518.m4a     →  2026-03-27 20:25:18
//	     01025777190_20260327202518-1.m4a   →  2026-03-27 20:25:18  (suffix ignored)
//
// Times are parsed in time.Local so that the cutover comparison is meaningful
// regardless of whether the cutover value was constructed in UTC or Local.
var (
	// reVoiceRecorder matches the 2-digit-year pattern at the END of the stem
	// (before the extension), allowing arbitrary label characters before it.
	reVoiceRecorder = regexp.MustCompile(`_(\d{2})(\d{2})(\d{2})_(\d{2})(\d{2})(\d{2})(?:\.\w+)?$`)

	// reTPhone matches the 14-digit timestamp followed by an optional -N suffix.
	reTPhone = regexp.MustCompile(`_(\d{4})(\d{2})(\d{2})(\d{2})(\d{2})(\d{2})(?:-\d+)?(?:\.\w+)?$`)
)

// recordingTime returns the recording timestamp for the given audio filename.
//
// It tries two filename patterns in order:
//  1. TPhoneCallRecords: <anything>_YYYYMMDDHHMMSS[-N].<ext>
//  2. Voice Recorder:   <label>_YYMMDD_HHMMSS.<ext>  (2-digit year, +2000)
//
// If neither pattern matches, or if the parsed date is clearly invalid,
// the file's mtime is returned unchanged. This ensures files with
// unparseable names continue to use the existing mtime-based cutover check.
//
// All parsed times are in time.Local.
func recordingTime(filename string, mtime time.Time) time.Time {
	// Pattern B first (14-digit timestamp is unambiguous and more specific).
	if m := reTPhone.FindStringSubmatch(filename); m != nil {
		yr := atoi(m[1])
		mo := atoi(m[2])
		dy := atoi(m[3])
		hr := atoi(m[4])
		mn := atoi(m[5])
		sc := atoi(m[6])
		t := time.Date(yr, time.Month(mo), dy, hr, mn, sc, 0, time.Local)
		if isPlausibleRecordingTime(t) {
			return t
		}
	}

	// Pattern A: 2-digit year.
	if m := reVoiceRecorder.FindStringSubmatch(filename); m != nil {
		yr := 2000 + atoi(m[1])
		mo := atoi(m[2])
		dy := atoi(m[3])
		hr := atoi(m[4])
		mn := atoi(m[5])
		sc := atoi(m[6])
		t := time.Date(yr, time.Month(mo), dy, hr, mn, sc, 0, time.Local)
		if isPlausibleRecordingTime(t) {
			return t
		}
	}

	return mtime
}

// atoi converts a decimal string to int, returning 0 on any error.
// The regex guarantees only digit characters, so strconv is not needed.
func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// isPlausibleRecordingTime returns true when t looks like a real recording
// date: year in [2000, 2100], month in [1, 12], day in [1, 31].
// time.Date normalises out-of-range values (e.g. month 13 → Jan of next year),
// so we check the input-level fields after construction to reject nonsense.
func isPlausibleRecordingTime(t time.Time) bool {
	return t.Year() >= 2000 && t.Year() <= 2100 &&
		t.Month() >= 1 && t.Month() <= 12 &&
		t.Day() >= 1 && t.Day() <= 31
}

// WhisperCollector transcribes audio files via an OpenAI-compatible Whisper
// endpoint and produces call-transcript documents.
//
// The collector scans cfg.WhisperAudioDir recursively, submitting only files
// whose modification time is after the scheduler watermark (since). This
// prevents re-transcription of already-processed files, which is important
// because transcription API calls are expensive.
//
// The implementation targets the /v1/audio/transcriptions endpoint using
// multipart/form-data (OpenAI API contract). Setting cfg.WhisperAPIURL to a
// local whisper.cpp server URL routes all requests through that server instead
// of the public OpenAI API — the protocol is identical.
//
// Enabled() returns true only when both WhisperAudioDir and WhisperAPIURL are
// set. WhisperAPIKey is optional: local whisper.cpp servers do not require
// authentication, so the Authorization header is omitted when the key is empty.
type WhisperCollector struct {
	cfg        *config.Config
	httpClient *http.Client
	baseURL    string // overridable in tests; defaults to cfg.WhisperAPIURL

	// maxFileBytes caps the per-file size accepted for transcription (0 = unlimited).
	// Sourced from cfg.WhisperMaxFileBytes; stored here so tests can override easily.
	maxFileBytes int64

	// indexedIDs is an optional set of source_ids already active in the store.
	// When non-nil, Collect emits files whose SourceID is absent from the set
	// even when their mtime predates the since watermark (IndexAwareCollector).
	indexedIDs map[string]struct{}

	// cutover is an optional floor time. When non-zero, files whose recording
	// timestamp (parsed from the filename via recordingTime) is before cutover
	// are suppressed even if they were never indexed. Files with unparseable
	// names fall back to mtime for the comparison.
	// Zero = floor disabled (no behaviour change).
	cutover time.Time
}

// NewWhisperCollector returns a WhisperCollector configured from cfg.
// When WhisperAudioDir or WhisperAPIURL is empty, Enabled() returns false
// and the scheduler will not call Collect.
//
// The HTTP client timeout is sourced from cfg.WhisperHTTPTimeout (set via
// WHISPER_HTTP_TIMEOUT env var; default 2h). If cfg.WhisperHTTPTimeout is
// zero (e.g. a zero-value Config in tests), whisperDefaultHTTPTimeout (2h)
// is used as a defence-in-depth fallback so a zero config value never
// yields an infinite (zero-timeout) HTTP client.
func NewWhisperCollector(cfg *config.Config) *WhisperCollector {
	httpTimeout := cfg.WhisperHTTPTimeout
	if httpTimeout <= 0 {
		httpTimeout = whisperDefaultHTTPTimeout
	}
	return &WhisperCollector{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: httpTimeout,
		},
		baseURL:      cfg.WhisperAPIURL,
		maxFileBytes: cfg.WhisperMaxFileBytes,
	}
}

func (c *WhisperCollector) Name() string             { return "whisper" }
func (c *WhisperCollector) Source() model.SourceType { return model.SourceCallTranscript }

// Enabled reports whether the collector is configured.
// WhisperAPIKey is intentionally NOT required: local whisper.cpp servers do
// not enforce authentication. The Authorization header is added only when the
// key is non-empty (see transcribeFile).
func (c *WhisperCollector) Enabled() bool {
	return c.cfg.WhisperAudioDir != "" && c.baseURL != ""
}

// WithIndexedIDs implements IndexAwareCollector. Supplying a non-nil set
// enables store-aware new-file detection: audio files whose SourceID is absent
// from the set are transcribed unconditionally (even when mtime <= since).
// Passing nil restores mtime-only filtering.
func (c *WhisperCollector) WithIndexedIDs(ids map[string]struct{}) {
	c.indexedIDs = ids
}

// WithCutover implements CutoverAwareCollector. When t is non-zero, files
// whose recording timestamp (parsed from the filename; falls back to mtime)
// is before t are suppressed even if they were never indexed.
// Zero t disables the floor (no behaviour change).
func (c *WhisperCollector) WithCutover(t time.Time) {
	c.cutover = t
}

// isLocalWhisperEndpoint reports whether the given URL host resolves to a
// loopback, Docker-internal, or RFC-1918 private address. Call-transcription
// data MUST stay local (issue #100). Misconfiguration producing a cloud endpoint
// is logged as a prominent warning but does not hard-fail the collector.
func isLocalWhisperEndpoint(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false // treat unparseable URL as non-local (warn)
	}
	host := u.Hostname()

	// Well-known local/docker aliases.
	switch host {
	case "localhost", "127.0.0.1", "::1", "host.docker.internal":
		return true
	}

	// Numeric IP: check for private/loopback ranges.
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	// RFC-1918 private ranges.
	privateRanges := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
	for _, cidr := range privateRanges {
		_, block, _ := net.ParseCIDR(cidr)
		if block != nil && block.Contains(ip) {
			return true
		}
	}
	return false
}

// Collect walks WhisperAudioDir recursively, transcribes audio files modified
// after since, and returns a Document per successful transcription.
//
// Incremental strategy (primary): only files with mtime > since are submitted.
// The scheduler watermark ensures that on subsequent runs only new or changed
// audio files are processed. The first run (since == zero) processes all files.
//
// IndexAware strategy (defence-in-depth): when WithIndexedIDs is called with a
// non-nil set, files whose SourceID is absent from the set are also transcribed
// regardless of mtime (fixes late-arriving files on OneDrive FUSE mounts).
//
// Cloud-endpoint guard: if the configured Whisper endpoint resolves to a
// non-local host, a prominent warning is logged on every collect call.
// Issue #100 mandates call transcription stays LOCAL.
//
// Partial success: individual transcription failures are logged as warnings and
// the walk continues. The final error is nil as long as the directory walk
// itself succeeds.
func (c *WhisperCollector) Collect(ctx context.Context, since time.Time) ([]model.Document, error) {
	// LOW: cloud-endpoint guard (issue #100).
	if !isLocalWhisperEndpoint(c.baseURL) {
		slog.Warn("whisper: endpoint does not appear to be local — call transcription data may be sent to a cloud API; set WhisperAPIURL to a localhost or private-network address",
			"endpoint", c.baseURL,
		)
	}

	var docs []model.Document
	now := time.Now().UTC()

	err := filepath.WalkDir(c.cfg.WhisperAudioDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			slog.Warn("whisper: walk error", "path", path, "error", walkErr)
			return nil // continue walk
		}

		// Respect context cancellation between files.
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if d.IsDir() {
			return nil
		}

		// Audio extension guard.
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if !whisperAudioExts[ext] {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			slog.Warn("whisper: stat failed", "path", path, "error", err)
			return nil
		}

		// Compute SourceID early so the indexed-set check can use it.
		relPath, relErr := filepath.Rel(c.cfg.WhisperAudioDir, path)
		if relErr != nil {
			relPath = path
		}
		sourceID := "transcript:" + relPath

		// Cutover floor: suppress files that pre-date the cutover even if
		// they were never indexed. Zero cutover = floor disabled.
		//
		// recordingTime extracts the actual recording date from the filename
		// (Voice Recorder / TPhoneCallRecords patterns). Staged audio files
		// have recent mtimes (copy time), so the mtime-based check wrongly
		// admits historical recordings. Filename-parsed time is used when
		// available; files with unparseable names fall back to mtime.
		if !c.cutover.IsZero() && recordingTime(d.Name(), info.ModTime()).Before(c.cutover) {
			return nil
		}

		// Incremental + IndexAware filter (HIGH#1 fix):
		// Emit when mtime > since  OR  SourceID not in indexed set.
		mtimeNew := since.IsZero() || info.ModTime().After(since)
		_, alreadyIndexed := c.indexedIDs[sourceID]
		notIndexed := c.indexedIDs != nil && !alreadyIndexed
		if !mtimeNew && !notIndexed {
			return nil
		}

		// Per-file size cap: skip files that exceed the configured limit.
		// When maxFileBytes <= 0 the cap is disabled (unlimited).
		if c.maxFileBytes > 0 && info.Size() > c.maxFileBytes {
			slog.Warn("whisper: skipping oversized file",
				"path", path,
				"size_bytes", info.Size(),
				"limit_bytes", c.maxFileBytes,
			)
			return nil
		}

		// Audio integrity pre-check (defence-in-depth layer 2):
		// Read only the first 8 bytes to validate the file header before sending
		// the full file to the whisper server. This prevents the infinite-retry
		// loop caused by 4096-byte garbage .m4a files (no ftyp box) that the
		// whisper server declines with av.error.InvalidDataError (HTTP 500).
		//
		// The ingest handler (layer 1) prevents new corrupt files from reaching
		// disk; this guard protects against files already on disk before the fix.
		//
		// Only one warning is emitted per walk — subsequent runs re-warn because
		// the file is still present. This is intentional: the warning serves as a
		// reminder to remove the corrupt file. Suppressing repeat warnings would
		// require persistent state (a blacklist file), which complicates restarts.
		if err := checkAudioFileHeader(path, ext); err != nil {
			slog.Warn("whisper: skipping unreadable or corrupt audio file (pre-check failed)",
				"path", path,
				"size_bytes", info.Size(),
				"reason", err,
			)
			return nil
		}

		txResult, err := c.transcribeFile(ctx, path)
		if err != nil {
			slog.Warn("whisper: transcription failed", "path", path, "error", err)
			return nil // partial success — continue
		}

		// Title is the filename without extension.
		title := strings.TrimSuffix(d.Name(), filepath.Ext(d.Name()))

		mtime := info.ModTime().UTC()
		meta := map[string]any{
			"relative_path": relPath,
			"language":      c.cfg.WhisperLanguage,
			"audio_size":    info.Size(),
			"model":         c.cfg.WhisperModel,
		}

		// Merge sidecar metadata when present (written by the ingest-recording
		// handler alongside the audio file). Missing sidecar = historical file or
		// OneDrive-staged file — silently skip, no error.
		if sidecarMeta, ok := readRecordingSidecar(path); ok {
			for k, v := range sidecarMeta {
				meta[k] = v
			}
		}

		// Use the plain transcript as the default content.
		content := txResult.text

		// Diarization post-processing (feature-flagged OFF by default).
		// Only attempted when:
		//   1. DiarizationEnabled=true AND DiarizationAPIURL is set
		//   2. Whisper returned at least one segment (verbose_json)
		// On any error the warning is logged and the plain transcript is used.
		if c.cfg.DiarizationEnabled && c.cfg.DiarizationAPIURL != "" && len(txResult.segments) > 0 {
			audioBytes, readErr := os.ReadFile(path)
			if readErr != nil {
				slog.Warn("whisper: diarization skipped — cannot re-read audio file",
					"path", path, "error", readErr)
			} else {
				isCall := isTPhoneCallPath(path)
				diarSegs, diarErr := c.diarizeAudio(ctx, path, audioBytes, isCall)
				if diarErr != nil {
					slog.Warn("whisper: diarization failed — using plain transcript",
						"path", path, "error", diarErr)
				} else {
					labelled, nSpeakers := labelTranscript(txResult.segments, diarSegs)
					if labelled != "" {
						content = labelled
						meta["speaker_count"] = nSpeakers
					} else {
						slog.Warn("whisper: labelTranscript produced empty output — using plain transcript",
							"path", path)
					}
				}
			}
		}

		docs = append(docs, model.Document{
			ID:          uuid.New(),
			SourceType:  model.SourceCallTranscript,
			SourceID:    sourceID,
			Title:       title,
			Content:     content,
			Metadata:    meta,
			OccurredAt:  &mtime,
			CollectedAt: now,
		})

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("whisper: walk %q: %w", c.cfg.WhisperAudioDir, err)
	}

	slog.Info("whisper: collected transcripts", "count", len(docs), "audio_dir", c.cfg.WhisperAudioDir)
	return docs, nil
}

// readRecordingSidecar attempts to read and parse the sidecar metadata file
// written by the ingest-recording handler at audioPath + ".meta.json".
//
// When the sidecar exists and is valid JSON, the function returns a map
// containing the recording metadata fields present in the file
// (contact_name, direction, recording_type, duration_seconds) and true.
// Only non-empty/non-zero values are included so callers do not overwrite
// existing metadata with zero-value defaults.
//
// When the sidecar is absent, unreadable, or unparseable, the function returns
// (nil, false) so the caller can proceed with existing metadata unchanged. This
// is the expected path for historical files and OneDrive-staged files that were
// present before the sidecar feature was introduced.
func readRecordingSidecar(audioPath string) (map[string]any, bool) {
	data, err := os.ReadFile(audioPath + ".meta.json")
	if err != nil {
		// Not present or unreadable — expected for pre-sidecar files.
		return nil, false
	}

	var raw struct {
		ContactName     string `json:"contact_name"`
		Direction       string `json:"direction"`
		RecordingType   string `json:"recording_type"`
		DurationSeconds int    `json:"duration_seconds"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		// Corrupt sidecar — skip without surfacing an error.
		return nil, false
	}

	result := make(map[string]any, 4)
	if raw.ContactName != "" {
		result["contact_name"] = raw.ContactName
	}
	if raw.Direction != "" {
		result["direction"] = raw.Direction
	}
	if raw.RecordingType != "" {
		result["recording_type"] = raw.RecordingType
	}
	if raw.DurationSeconds != 0 {
		result["duration_seconds"] = raw.DurationSeconds
	}

	if len(result) == 0 {
		return nil, false
	}
	return result, true
}

// checkAudioFileHeader reads the first 8 bytes of the file at path and runs
// the appropriate audiovalidate check for the given extension.
//
// m4a/mp4 files are checked for the ISOBMFF "ftyp" box at offset 4.
// All other audio formats only require the minimum-length (8-byte) guard.
//
// Returns nil when the header is plausible, an error when the file appears
// corrupt or truncated. The caller should log once and skip the file.
func checkAudioFileHeader(path, ext string) error {
	const headerSize = 8
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only, close error is irrelevant

	header := make([]byte, headerSize)
	n, err := io.ReadFull(f, header)
	if err != nil {
		// io.ErrUnexpectedEOF means the file is shorter than headerSize bytes.
		// That is itself a validation failure, so we fall through to the check.
		header = header[:n]
	}

	switch strings.ToLower(ext) {
	case ".m4a", ".mp4":
		return audiovalidate.CheckM4A(header)
	default:
		return audiovalidate.CheckAudioBytes(header)
	}
}

// transcribeFileResult holds the output of a transcription call.
type transcribeFileResult struct {
	text     string           // flat transcript text
	segments []whisperSegment // time-stamped segments; may be nil/empty for older servers
}

// transcribeFile posts the audio file at path to the Whisper transcription
// endpoint and returns the transcript text and, when diarization is enabled,
// time-stamped segments.
//
// The request is a multipart/form-data POST to {baseURL}/audio/transcriptions
// with the following fields:
//   - file: audio file bytes (filename preserved for MIME detection by the server)
//   - model: cfg.WhisperModel
//   - language: cfg.WhisperLanguage (omitted when empty)
//   - response_format: "verbose_json" (ONLY when cfg.DiarizationEnabled is true)
//
// When DiarizationEnabled is false the request is identical to the pre-diarization
// implementation: no response_format field, plain {"text":"..."} response parsed
// via whisperTranscribeResponse. This ensures zero behaviour change for deployments
// running with the flag off.
//
// When DiarizationEnabled is true, response_format=verbose_json is added and the
// response is parsed via whisperVerboseResponse to extract per-segment timestamps
// needed for speaker alignment.
//
// The Authorization header is set only when cfg.WhisperAPIKey is non-empty,
// enabling use with local whisper.cpp servers that do not require authentication.
func (c *WhisperCollector) transcribeFile(ctx context.Context, path string) (transcribeFileResult, error) {
	audioBytes, err := os.ReadFile(path)
	if err != nil {
		return transcribeFileResult{}, fmt.Errorf("read audio file: %w", err)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	// file field
	fw, err := mw.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return transcribeFileResult{}, fmt.Errorf("create form file: %w", err)
	}
	if _, err := fw.Write(audioBytes); err != nil {
		return transcribeFileResult{}, fmt.Errorf("write audio bytes: %w", err)
	}

	// model field
	if err := mw.WriteField("model", c.cfg.WhisperModel); err != nil {
		return transcribeFileResult{}, fmt.Errorf("write model field: %w", err)
	}

	// language field (optional — omit when empty to let the API auto-detect)
	if c.cfg.WhisperLanguage != "" {
		if err := mw.WriteField("language", c.cfg.WhisperLanguage); err != nil {
			return transcribeFileResult{}, fmt.Errorf("write language field: %w", err)
		}
	}

	// response_format=verbose_json is only added when diarization is enabled.
	// When the flag is off this field is absent, preserving the original
	// request format byte-for-byte.
	if c.cfg.DiarizationEnabled {
		if err := mw.WriteField("response_format", "verbose_json"); err != nil {
			return transcribeFileResult{}, fmt.Errorf("write response_format field: %w", err)
		}
	}

	if err := mw.Close(); err != nil {
		return transcribeFileResult{}, fmt.Errorf("close multipart writer: %w", err)
	}

	endpoint := strings.TrimRight(c.baseURL, "/") + "/audio/transcriptions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return transcribeFileResult{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	// Authorization header is optional: local whisper.cpp servers typically
	// operate without authentication.
	if c.cfg.WhisperAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.WhisperAPIKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return transcribeFileResult{}, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return transcribeFileResult{}, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return transcribeFileResult{}, fmt.Errorf("whisper API returned %d: %s", resp.StatusCode, body)
	}

	// Parse response according to the requested format.
	// Flag OFF: plain {"text":"..."} — identical to the original pre-#111 path.
	// Flag ON:  verbose_json {"text":"...","segments":[...]} for diarization alignment.
	if !c.cfg.DiarizationEnabled {
		var result whisperTranscribeResponse
		if err := json.Unmarshal(body, &result); err != nil {
			return transcribeFileResult{}, fmt.Errorf("decode response JSON: %w", err)
		}
		return transcribeFileResult{text: result.Text}, nil
	}

	var result whisperVerboseResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return transcribeFileResult{}, fmt.Errorf("decode response JSON: %w", err)
	}
	return transcribeFileResult{
		text:     result.Text,
		segments: result.Segments,
	}, nil
}

// isTPhoneCallPath reports whether path looks like a TPhoneCallRecords audio
// file based on the reTPhone filename pattern. When true, the diarization
// request is sent with num_speakers=2 (phone calls are always two-party).
//
// Heuristic: the TPhone pattern requires a phone-number prefix followed by
// a 14-digit timestamp (see reTPhone). Voice memos and other recordings do
// not match, so num_speakers is omitted for them.
func isTPhoneCallPath(path string) bool {
	return reTPhone.MatchString(filepath.Base(path))
}

// diarizeAudio posts the audio bytes to the diarization microservice and
// returns the speaker segments. On any error (service unavailable, bad
// response, empty segments) the error is returned and the caller should
// fall back to the plain transcript.
//
// When isTPhoneCall is true the request includes num_speakers=2; otherwise
// the field is omitted and the service auto-detects the number of speakers.
func (c *WhisperCollector) diarizeAudio(ctx context.Context, path string, audioBytes []byte, isTPhoneCall bool) ([]diarSegment, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	fw, err := mw.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return nil, fmt.Errorf("create form file: %w", err)
	}
	if _, err := fw.Write(audioBytes); err != nil {
		return nil, fmt.Errorf("write audio bytes: %w", err)
	}

	if isTPhoneCall {
		if err := mw.WriteField("num_speakers", "2"); err != nil {
			return nil, fmt.Errorf("write num_speakers field: %w", err)
		}
	}

	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("close multipart writer: %w", err)
	}

	endpoint := strings.TrimRight(c.cfg.DiarizationAPIURL, "/") + "/diarize"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return nil, fmt.Errorf("build diarization request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("diarization http request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read diarization response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("diarization API returned %d: %s", resp.StatusCode, body)
	}

	var result diarizeResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode diarization response JSON: %w", err)
	}

	if len(result.Segments) == 0 {
		return nil, fmt.Errorf("diarization returned empty segments")
	}

	return result.Segments, nil
}
