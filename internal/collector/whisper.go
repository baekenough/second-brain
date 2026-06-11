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
// transcription endpoint (/v1/audio/transcriptions).
type whisperTranscribeResponse struct {
	Text string `json:"text"`
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

		text, err := c.transcribeFile(ctx, path)
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

		docs = append(docs, model.Document{
			ID:          uuid.New(),
			SourceType:  model.SourceCallTranscript,
			SourceID:    sourceID,
			Title:       title,
			Content:     text,
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

// transcribeFile posts the audio file at path to the Whisper transcription
// endpoint and returns the transcript text.
//
// The request is a multipart/form-data POST to {baseURL}/audio/transcriptions
// with the following fields:
//   - file: audio file bytes (filename preserved for MIME detection by the server)
//   - model: cfg.WhisperModel
//   - language: cfg.WhisperLanguage (omitted when empty)
//
// The Authorization header is set only when cfg.WhisperAPIKey is non-empty,
// enabling use with local whisper.cpp servers that do not require authentication.
func (c *WhisperCollector) transcribeFile(ctx context.Context, path string) (string, error) {
	audioBytes, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read audio file: %w", err)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	// file field
	fw, err := mw.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	if _, err := fw.Write(audioBytes); err != nil {
		return "", fmt.Errorf("write audio bytes: %w", err)
	}

	// model field
	if err := mw.WriteField("model", c.cfg.WhisperModel); err != nil {
		return "", fmt.Errorf("write model field: %w", err)
	}

	// language field (optional — omit when empty to let the API auto-detect)
	if c.cfg.WhisperLanguage != "" {
		if err := mw.WriteField("language", c.cfg.WhisperLanguage); err != nil {
			return "", fmt.Errorf("write language field: %w", err)
		}
	}

	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("close multipart writer: %w", err)
	}

	endpoint := strings.TrimRight(c.baseURL, "/") + "/audio/transcriptions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	// Authorization header is optional: local whisper.cpp servers typically
	// operate without authentication.
	if c.cfg.WhisperAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.WhisperAPIKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("whisper API returned %d: %s", resp.StatusCode, body)
	}

	var result whisperTranscribeResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decode response JSON: %w", err)
	}

	return result.Text, nil
}
