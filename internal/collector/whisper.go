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
	"sync"
	"time"

	"github.com/baekenough/second-brain/internal/audiovalidate"
	"github.com/baekenough/second-brain/internal/collector/smsmap"
	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// whisperDefaultHTTPTimeout is the fallback per-request timeout used when the
// config value is zero (misconfigured). 2 hours covers long audio recordings
// without producing an infinite (zero) timeout. Prefer cfg.WhisperHTTPTimeout
// over this constant; this constant exists only as a defence-in-depth fallback.
const whisperDefaultHTTPTimeout = 2 * time.Hour

// whisperCloudMaxFileBytes is the hard per-file upload limit enforced by
// OpenAI's /v1/audio/transcriptions endpoint (verified against the live API).
// This is NOT configurable — it reflects a fixed server-side constraint, not
// a local operational preference (contrast with cfg.WhisperMaxFileBytes,
// which IS configurable and exists to bound local disk/network usage). It
// applies only when the resolved endpoint is non-local (isLocalWhisperEndpoint
// == false); self-hosted whisper.cpp servers do not enforce this limit.
const whisperCloudMaxFileBytes = 25 << 20 // 25 MiB

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

// modelSupportsNativeDiarization reports whether model performs speaker
// diarization as part of the transcription response itself (OpenAI's
// gpt-4o-transcribe-diarize family), as opposed to a plain transcription
// model whose output must be aligned against a separate diarization pass
// (see diarizeAudio / diarSegment above).
//
// Detection is a case-insensitive substring match on "diarize" rather than an
// exact allow-list, so future model variants that follow the same naming
// convention are recognised without a code change.
func modelSupportsNativeDiarization(model string) bool {
	return strings.Contains(strings.ToLower(model), "diarize")
}

// diarizedSegment is a single speaker-labelled segment from the OpenAI
// gpt-4o-transcribe-diarize response (response_format=diarized_json).
//
// Verified response shape (ground truth — checked against the live OpenAI
// API):
//
//	{"type":"transcript.text.segment","text":" ...","speaker":"A",
//	 "start":0.0,"end":5.55,"id":"seg_0"}
//
// Speaker values are single uppercase letters ("A", "B", …) — NOT the
// "SPEAKER_00" style produced by the pyannote microservice (diarSegment
// above). The two speaker-ID vocabularies are never mixed: each audio file
// is processed by exactly one diarization path (see
// modelSupportsNativeDiarization).
type diarizedSegment struct {
	Type    string  `json:"type"`
	Text    string  `json:"text"`
	Speaker string  `json:"speaker"`
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	ID      string  `json:"id"`
}

// diarizedResponse is the JSON body returned by /v1/audio/transcriptions
// when response_format=diarized_json is requested. Segments may be absent or
// empty even on a 200 response — observed when chunking_strategy is
// omitted/misconfigured (see transcribeFile) or for very short clips the
// model chooses not to segment. Callers must fall back to the flat Text
// field in that case; see transcribeFile and buildDocument.
type diarizedResponse struct {
	Text     string            `json:"text"`
	Task     string            `json:"task"`
	Duration float64           `json:"duration"`
	Segments []diarizedSegment `json:"segments"`
}

// rawLabelledSeg pairs a raw (pre-화자N) speaker identifier with a segment's
// text. It is the common input shape consumed by renderSpeakerBlocks: the two
// diarization paths (pyannote alignment in labelTranscript, and native
// diarization in nativeDiarizedSegsToRawLabelled) differ only in HOW they
// arrive at a (speaker, text) pair per segment — one aligns by time-interval
// overlap, the other reads an already-authoritative field — but both converge
// on this same shape so a single renderer guarantees identical stored output
// format ("[화자N] text" grouped blocks) regardless of path.
type rawLabelledSeg struct {
	speaker string // raw ID, e.g. "SPEAKER_00" (pyannote) or "A" (native diarize) — not yet a 화자N label
	text    string
}

// nativeDiarizedSegsToRawLabelled converts diarized_json segments into the
// rawLabelledSeg sequence consumed by renderSpeakerBlocks.
//
// This function deliberately does NOT reuse labelTranscript's overlap-based
// alignment. labelTranscript exists to solve a different problem: aligning
// two INDEPENDENTLY timestamped sequences (whisper transcript segments vs. a
// separate diarization service's segments) that were never guaranteed to
// share boundaries. Native diarize segments have no such problem — each
// diarizedSegment carries its OWN authoritative speaker for its OWN text,
// assigned by the model as part of producing that exact segment. There is
// nothing to align.
//
// Routing native segments through labelTranscript's overlap-based
// assignSpeaker instead would be actively wrong: assignSpeaker scores ALL
// diarSegs against a given whisper segment and picks the STRICTLY GREATEST
// overlap (ties go to whichever candidate it evaluates first), so if the API
// ever returns a segment whose interval fully contains another's — plausible
// at chunking_strategy=auto chunk boundaries — the contained segment's own
// interval loses the overlap contest to the containing segment, silently
// misattributing its text to the wrong speaker. That failure mode is
// invisible (no error, wrong output) and depends entirely on a third-party
// API's segmentation behaviour, which this code does not control. Native
// diarization output must therefore be trusted per-segment, never re-aligned.
func nativeDiarizedSegsToRawLabelled(segs []diarizedSegment) []rawLabelledSeg {
	out := make([]rawLabelledSeg, len(segs))
	for i, s := range segs {
		out[i] = rawLabelledSeg{speaker: s.Speaker, text: s.Text}
	}
	return out
}

// renderSpeakerBlocks converts a per-segment sequence of (raw speaker, text)
// pairs — in original segment order — into content formatted as consecutive
// "[화자N] text" blocks, plus the number of distinct speakers.
//
// Raw speaker IDs are mapped to 화자1, 화자2, … in order of FIRST APPEARANCE in
// segs (not alphabetical/numeric order of the raw ID), so the mapping is
// stable and readable regardless of whether the raw vocabulary is pyannote's
// "SPEAKER_00" style or the native diarize API's "A"/"B" style.
//
// Consecutive segments assigned to the same 화자N label are merged into a
// single block (space-joined); blocks themselves are newline-separated.
// Segments with empty (post-trim) text contribute no text but do not break an
// in-progress block.
//
// If segs is empty the function returns ("", 0) so callers can fall back to
// the plain transcript text.
func renderSpeakerBlocks(segs []rawLabelledSeg) (content string, speakerCount int) {
	if len(segs) == 0 {
		return "", 0
	}

	// speakerMap maps raw speaker IDs → 화자N labels in order of first
	// appearance. A slice is used to keep insertion-order traversal O(n).
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

	type labelledSeg struct {
		speaker string
		text    string
	}
	labelled := make([]labelledSeg, len(segs))
	for i, s := range segs {
		labelled[i] = labelledSeg{
			speaker: speakerLabel(s.speaker),
			text:    strings.TrimSpace(s.text),
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

// labelTranscript aligns whisper transcript segments with speaker diarization
// segments (from the separate pyannote microservice — see diarizeAudio) and
// returns speaker-labelled content and the number of distinct speakers.
//
// This alignment step exists ONLY because whisper transcript segments and
// pyannote diarization segments are two INDEPENDENTLY timestamped sequences
// with no guaranteed shared boundaries — they must be reconciled by
// time-interval overlap. Native diarization (gpt-4o-transcribe-diarize) has
// no such problem and does NOT go through this function; see
// nativeDiarizedSegsToRawLabelled for why re-alignment would be actively
// wrong for that path.
//
// Alignment strategy:
//   - For each whisper segment, find the diarization speaker whose time
//     interval has the maximum overlap (overlap = max(0, min(ends)-max(starts))).
//   - When no diarization segment overlaps a whisper segment at all, the
//     nearest diarization speaker by midpoint distance is assigned.
//
// The resulting (speaker, text) pairs are rendered via renderSpeakerBlocks,
// which performs the 화자N first-appearance numbering and consecutive-block
// grouping shared with the native diarization path. This preserves the
// original labelTranscript output byte-for-byte — only the block-rendering
// half was extracted into a shared helper.
//
// If whisperSegs or diarSegs is empty the function returns ("", 0) so callers
// can fall back to the plain transcript text.
func labelTranscript(whisperSegs []whisperSegment, diarSegs []diarSegment) (content string, speakerCount int) {
	if len(whisperSegs) == 0 || len(diarSegs) == 0 {
		return "", 0
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

	// Assign each whisper segment a (raw) speaker via overlap alignment, then
	// hand off to the shared renderer for 화자N numbering and block grouping.
	segs := make([]rawLabelledSeg, len(whisperSegs))
	for i, ws := range whisperSegs {
		segs[i] = rawLabelledSeg{speaker: assignSpeaker(ws), text: ws.Text}
	}

	return renderSpeakerBlocks(segs)
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

	// diarizeHTTPClient is a SEPARATE http.Client used only by diarizeAudio
	// (#169). Unlike httpClient (shared by transcribeFile, where a cloud
	// endpoint is a supported — if discouraged — operator choice, #100),
	// diarization audio must never leave the machine (#165), so this client's
	// Transport.DialContext performs dial-time locality verification: it
	// resolves the host, confirms EVERY resolved address is
	// loopback/RFC-1918/link-local, and dials ONLY that verified address
	// (pinned — never re-resolved). This closes the gap in
	// isLocalWhisperEndpoint, whose single-label-hostname branch trusts the
	// host with NO resolution at all, and whose multi-label branch resolves
	// once during the pre-flight check without pinning the transport's
	// subsequent dial to the same address (a DNS-rebinding TOCTOU).
	// See verifiedDialContext.
	diarizeHTTPClient *http.Client

	// diarizeResolveHost is the DNS resolution function used by
	// verifiedDialContext. Overridable in tests to simulate DNS-rebinding
	// scenarios (a resolver answer that differs from whatever the pre-flight
	// isLocalWhisperEndpoint check observed). Defaults to
	// defaultDiarizeResolveHost (net.DefaultResolver.LookupHost) when nil.
	diarizeResolveHost func(ctx context.Context, host string) ([]string, error)

	// maxFileBytes caps the per-file size accepted for transcription (0 = unlimited).
	// Sourced from cfg.WhisperMaxFileBytes; stored here so tests can override easily.
	maxFileBytes int64

	// workerCount is the number of files transcribed in parallel after the walk.
	// Sourced from cfg.WhisperConcurrency (clamped to >= 1 by concurrency()).
	// Stored here so tests can override without rebuilding the Config. A value of
	// 1 (the default) reproduces the original sequential behaviour exactly.
	workerCount int

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

	// failedQuarantine is an in-memory skip-set of absolute file paths whose
	// quarantine move failed (e.g. because the audio directory is on a
	// read-only mount — EROFS). Paths recorded here are silently bypassed on
	// subsequent WalkDir passes within the same process lifetime, preventing
	// the infinite-retry loop that occurs when physical isolation is impossible
	// (#158).
	//
	// Physical quarantine to a separate rw volume (A-plan) would persist across
	// restarts but is excluded from this change: the marginal benefit (one
	// extra warning on restart) is outweighed by the operational complexity of
	// adding a second mount. The skip-set fully eliminates the loop for the
	// current process lifetime.
	//
	// Access pattern: Collect() calls WalkDir which is single-goroutine, so
	// no mutex is required as long as the scheduler does not call Collect()
	// concurrently on the same collector instance (confirmed by scheduler
	// design). Initialised in NewWhisperCollector.
	failedQuarantine map[string]struct{}
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

	// Security: refuse to follow HTTP redirects on every request either
	// client makes. Without this, a compromised/misconfigured "local"
	// whisper or diarization endpoint could 30x-redirect the request to an
	// external host, exfiltrating raw call-audio bytes even though
	// isLocalWhisperEndpoint approved the original (local) URL — the
	// locality check only inspects the request URL, not where a redirect
	// might subsequently point. Returning a plain error (rather than
	// http.ErrUseLastResponse) makes both callers fail loudly with an
	// explicit security reason instead of silently consuming whatever body
	// the redirect target returned.
	refuseRedirect := func(req *http.Request, via []*http.Request) error {
		return fmt.Errorf("whisper: refusing to follow HTTP redirect to %q (audio exfiltration guard)", req.URL)
	}

	c := &WhisperCollector{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout:       httpTimeout,
			CheckRedirect: refuseRedirect,
		},
		baseURL:          cfg.WhisperAPIURL,
		maxFileBytes:     cfg.WhisperMaxFileBytes,
		workerCount:      cfg.WhisperConcurrency,
		failedQuarantine: make(map[string]struct{}),
	}

	// Diarization gets its OWN http.Client (#169). Its Transport's
	// DialContext is c.verifiedDialContext, which independently verifies
	// locality AT DIAL TIME and pins the dial to the exact address it
	// verified — see the diarizeHTTPClient field doc and verifiedDialContext
	// for the full rationale. transcribeFile is completely unaffected: it
	// keeps using c.httpClient, unchanged.
	c.diarizeHTTPClient = &http.Client{
		Timeout:       httpTimeout,
		CheckRedirect: refuseRedirect,
		Transport: &http.Transport{
			DialContext: c.verifiedDialContext,
		},
	}

	return c
}

// concurrency returns the effective worker-pool size for transcription, clamped
// to at least 1. A zero or negative workerCount (e.g. a zero-value Config in
// tests) yields 1, preserving the original sequential behaviour.
func (c *WhisperCollector) concurrency() int {
	if c.workerCount < 1 {
		return 1
	}
	return c.workerCount
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

// whisperPrivateCIDRs is the set of RFC-1918 and loopback CIDR blocks that are
// considered local for the purposes of the cloud-endpoint guard (#153).
var whisperPrivateCIDRs = func() []*net.IPNet {
	cidrs := []string{
		"127.0.0.0/8",    // IPv4 loopback
		"10.0.0.0/8",     // RFC-1918 class A
		"172.16.0.0/12",  // RFC-1918 class B
		"192.168.0.0/16", // RFC-1918 class C
		// Tailscale CGNAT shared address space (RFC 6598). Tailscale assigns mesh
		// nodes 100.64.0.0/10 addresses (e.g. 100.64.0.1), so pointing the
		// whisper endpoint at a Tailscale IP must count as local — otherwise a
		// false "endpoint not local" WARN fires every cycle. Keeps issue #100
		// (call data stays local) satisfied for the Tailscale mesh.
		"100.64.0.0/10",
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, block, err := net.ParseCIDR(cidr)
		if err == nil {
			nets = append(nets, block)
		}
	}
	return nets
}()

// isPrivateIP reports whether ip falls within a loopback or RFC-1918 range.
func isPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() {
		return true
	}
	for _, block := range whisperPrivateCIDRs {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

// isPrivateOrLinkLocalIP reports whether ip is loopback, RFC-1918, or
// link-local (RFC 3927 IPv4 169.254.0.0/16, or IPv6 fe80::/10 and its
// multicast counterpart). Used ONLY by verifiedDialContext, the diarization
// dial-time verifier (#169) — it is deliberately NOT merged into isPrivateIP
// because widening isPrivateIP would also widen isLocalWhisperEndpoint, which
// is shared with the transcribe-path warn-only guard (#100); that guard's
// behaviour must not change as part of this fix.
func isPrivateOrLinkLocalIP(ip net.IP) bool {
	return isPrivateIP(ip) || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
}

// isLocalWhisperEndpoint reports whether the given URL host resolves to a
// loopback, Docker-internal, or RFC-1918 private address.
//
// Historically (issue #100) call-transcription data was required to stay
// local; that policy has since been reversed by explicit user decision — this
// collector now defaults to OpenAI's gpt-4o-transcribe-diarize (a cloud
// endpoint). This function is retained because CollectStream still needs to
// know whether the endpoint is local for two purposes unrelated to the old
// local-only mandate: (1) log level selection for the cloud-endpoint guard
// (see cfg.WhisperCloudAllowed), and (2) the 25 MiB hard file-size cap that
// only OpenAI's hosted API enforces (whisperCloudMaxFileBytes). A non-local
// endpoint no longer hard-fails or blocks the collector either way.
//
// Locality detection strategy (#153):
//  1. Well-known literal aliases (localhost, 127.0.0.1, ::1, host.docker.internal).
//  2. Single-label hostnames without a dot (e.g. "whisper") — these are
//     Docker / docker-compose service names that only resolve inside a private
//     network and cannot be public DNS names; treat as local.
//  3. Numeric IP: check loopback and RFC-1918 ranges directly.
//  4. Multi-label hostname: attempt DNS resolution; if ALL resolved IPs fall
//     within private/loopback ranges, treat as local. Resolution failure is
//     treated as non-local (safer: warn rather than silently suppress).
func isLocalWhisperEndpoint(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false // treat unparseable URL as non-local (warn)
	}
	host := u.Hostname()

	// 1. Well-known literal aliases.
	switch host {
	case "localhost", "127.0.0.1", "::1", "host.docker.internal":
		return true
	}

	// 2. Single-label hostnames (no dot) — Docker / compose service names.
	// These cannot be public internet hostnames (ICANN requires at least one dot).
	if !strings.Contains(host, ".") && host != "" {
		return true
	}

	// 3. Numeric IP: check directly without DNS.
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() {
			return true
		}
		return isPrivateIP(ip)
	}

	// 4. Multi-label hostname: resolve and check all returned IPs.
	// Use a short timeout so a misconfigured DNS server does not block a
	// collection cycle. Treat resolution failure as non-local.
	addrs, err := net.LookupHost(host)
	if err != nil || len(addrs) == 0 {
		return false
	}
	for _, addr := range addrs {
		ip := net.ParseIP(addr)
		if ip == nil || !isPrivateIP(ip) {
			return false
		}
	}
	return true
}

// whisperStreamBatchSize is the number of completed transcription documents
// accumulated by the drain goroutine before each onBatch (emit) call in
// CollectStream. It is intentionally small (4–8 range) so that the scheduler
// records each batch in the transcription ledger and upserts the documents
// incrementally during a long initial drain (hundreds of files / multiple
// hours). This means:
//   - The ledger fills live, so progress survives a mid-drain restart (already
//     transcribed source_ids are skipped on the next run) instead of starting
//     over from zero.
//   - The ledger's correctness can be observed during the drain rather than only
//     after the entire backlog completes.
//
// 5 balances ledger-write overhead (one batched round-trip per 5 files) against
// the recovery granularity above.
const whisperStreamBatchSize = 5

// Collect walks WhisperAudioDir recursively, transcribes audio files, and
// returns a Document per successful transcription.
//
// Collect is retained for backward compatibility and is implemented as a thin
// accumulator around CollectStream: it appends every emitted batch into a single
// slice and returns it. Callers processing a large backlog should prefer
// CollectStream directly so that transcripts are flushed incrementally (the
// scheduler does this — see CollectStream).
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
// non-local host, a message is logged on every collect call — Info when
// cfg.WhisperCloudAllowed acknowledges the policy exception, Warn otherwise.
// Issue #100 originally mandated that call transcription stay LOCAL; this
// collector now defaults to OpenAI's gpt-4o-transcribe-diarize (a cloud
// endpoint) by design, with cfg.WhisperCloudAllowed documenting that the
// exception was explicit rather than accidental misconfiguration. The guard
// never blocks the request either way — it is observability, not enforcement.
//
// Cloud file-size guard: OpenAI's /v1/audio/transcriptions endpoint hard-caps
// uploads at 25 MiB (whisperCloudMaxFileBytes, verified against the live
// API). When the resolved endpoint is non-local, files over this limit are
// skipped (not quarantined — they are valid audio, just too large for a
// single request) and the walk continues; see CollectStream for the exact
// placement and the transcription-ledger interaction.
//
// Partial success: individual transcription failures are logged as warnings and
// the walk continues. The final error is nil as long as the directory walk
// itself succeeds.
func (c *WhisperCollector) Collect(ctx context.Context, since time.Time) ([]model.Document, error) {
	var docs []model.Document
	err := c.CollectStream(ctx, since, func(batch []model.Document) error {
		docs = append(docs, batch...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return docs, nil
}

// CollectStream walks WhisperAudioDir recursively, transcribes audio files, and
// emits successfully-transcribed documents in bounded batches via onBatch.
// It implements StreamingCollector so the scheduler records each batch in the
// transcription ledger and upserts it incrementally rather than waiting for the
// entire (potentially multi-hour) backlog to finish.
//
// Pipeline:
//   - Phase 1 (SEQUENTIAL walk): every filter (ext, cutover, authoritative
//     index-skip, cloud 25 MiB size cap, local size cap, failedQuarantine
//     skip-set, header check + quarantine) runs single-goroutine inside
//     WalkDir, preserving the single-writer safety of the failedQuarantine map
//     (no mutex). Files that pass become `pending`.
//   - Phase 2 (CONCURRENT transcription): a worker pool of c.concurrency()
//     workers transcribes pending files and sends each completed document to a
//     results channel. A SINGLE drain goroutine receives from that channel,
//     buffers documents, and calls onBatch once per whisperStreamBatchSize
//     (and once more for the final partial batch). Concentrating every onBatch
//     call in one goroutine is REQUIRED: the scheduler's processBatch (ledger
//     record + embedding + upsert) is not safe for concurrent invocation.
//
// Cancellation: ctx is honoured by both the workers (between files) and the
// drain goroutine (between batches). If onBatch returns an error the stream
// stops and that error is propagated (wrapped so it is distinguishable from an
// internal error). Individual transcription failures are logged and skipped
// (partial success), matching Collect's contract.
//
// Goroutine safety: the results channel is closed exactly once (after the worker
// WaitGroup drains) and the drain goroutine exits on channel close, so there is
// no goroutine leak.
func (c *WhisperCollector) CollectStream(ctx context.Context, since time.Time, onBatch func([]model.Document) error) error {
	if onBatch == nil {
		return fmt.Errorf("whisper: CollectStream requires non-nil onBatch")
	}

	// Cloud-endpoint guard (issue #100 policy exception). isLocal is computed
	// once here and reused below by the Phase 1 walk for the 25 MiB cloud
	// file-size cap, which applies only to non-local endpoints.
	isLocal := isLocalWhisperEndpoint(c.baseURL)
	if !isLocal {
		if c.cfg.WhisperCloudAllowed {
			slog.Info("whisper: call audio is intentionally sent to a cloud transcription endpoint (issue #100 policy exception acknowledged)",
				"endpoint", c.baseURL,
			)
		} else {
			slog.Warn("whisper: endpoint does not appear to be local — call transcription data may be sent to a cloud API; set WHISPER_CLOUD_ALLOWED=true to acknowledge this is intentional, or WHISPER_API_URL to a localhost/private-network address",
				"endpoint", c.baseURL,
			)
		}
	}

	now := time.Now().UTC()

	// Phase 1 — SEQUENTIAL walk + filtering.
	//
	// All filtering (ext, cutover, authoritative index-skip, size cap,
	// failedQuarantine skip-set, header check + quarantine) runs single-goroutine
	// inside WalkDir. This preserves the single-writer safety of the
	// failedQuarantine map (no mutex needed). Files that pass every filter are
	// collected into `pending`; the expensive transcription happens in Phase 2.
	var pending []pendingTranscription

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
			// Skip the quarantine directory entirely — files moved there are
			// corrupt or unreadable and must not be re-visited on subsequent
			// collection cycles (#152).
			if d.Name() == whisperQuarantineDir {
				return fs.SkipDir
			}
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
		//
		// Kept BEFORE the index-skip block (unchanged).
		if !c.cutover.IsZero() && recordingTime(d.Name(), info.ModTime()).Before(c.cutover) {
			return nil
		}

		// Authoritative index skip (infinite re-transcription loop fix).
		//
		// Audio files are IMMUTABLE: once a source_id has been transcribed it must
		// NEVER be transcribed again. When the index set is available
		// (c.indexedIDs != nil it is supplied by the scheduler as the UNION of the
		// active document index and the transcription ledger) it is AUTHORITATIVE:
		//   - source_id present  → skip (already transcribed; do not re-submit).
		//   - source_id absent   → transcribe, regardless of mtime. This preserves
		//     IndexAware recovery of late-arriving files (OneDrive FUSE) whose
		//     mtime predates the watermark.
		// The previous "mtimeNew || notIndexed" logic could only ADD files to the
		// set and never SUPPRESS an already-processed one, so a duplicate-rejected
		// or transcription-failed file (never in the active index) was re-submitted
		// every ~60s cycle forever.
		//
		// When the index set is unavailable (c.indexedIDs == nil — the scheduler's
		// store query failed) fall back to the mtime watermark: transcribe only
		// files modified after `since` (since.IsZero() ⇒ first run ⇒ all files).
		if c.indexedIDs != nil {
			if _, known := c.indexedIDs[sourceID]; known {
				return nil // authoritative: already transcribed, skip
			}
			// absent → transcribe (fall through), regardless of mtime
		} else {
			mtimeNew := since.IsZero() || info.ModTime().After(since)
			if !mtimeNew {
				return nil
			}
		}

		// Cloud API hard limit (25 MiB): applies only to non-local endpoints
		// (isLocal, captured once above CollectStream's walk). Unlike the
		// configurable c.maxFileBytes cap below (a local disk-usage guard),
		// this reflects a fixed server-side constraint — OpenAI rejects larger
		// uploads outright. Submitting an oversized file anyway would waste a
		// network round-trip and log a spurious API error, so it is caught
		// here first.
		//
		// The file is valid audio, not corrupt, so it is skipped (walk
		// continues) WITHOUT quarantine and WITHOUT emitting a Document —
		// chunking oversized files for the cloud API is out of scope for this
		// change.
		//
		// Ledger interaction: skipped files here are NEVER passed to the
		// scheduler's onBatch, so they are never recorded in the transcription
		// ledger either (RecordTranscribed only runs over documents that
		// actually reach a batch). This means an oversized file re-triggers
		// this same WARN on every future collection cycle until it either
		// shrinks, moves under a local endpoint, or is chunked externally —
		// deliberately mirroring the existing c.maxFileBytes cap immediately
		// below, which has always skipped without ledgering (a pre-existing,
		// accepted trade-off, not one introduced by this change). Doing
		// anything else here (e.g. ledgering a skip so the WARN fires only
		// once) would require plumbing store/ledger access into
		// WhisperCollector, which today only produces model.Document values
		// and has no store dependency at all — out of scope for this change,
		// and unlike the original infinite-retry bug (#158) this cost is a
		// single log line per cycle, not a repeated expensive network/CPU
		// operation.
		if !isLocal && info.Size() > whisperCloudMaxFileBytes {
			slog.Warn("whisper: skipping file over cloud API 25 MiB limit",
				"path", path,
				"size_bytes", info.Size(),
				"limit_bytes", whisperCloudMaxFileBytes,
			)
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
		// On a writable mount: the file is quarantined (moved to
		// <audioDir>/.quarantine/) and logged once. Subsequent walks skip it
		// because it is no longer in the audio directory tree (#152).
		//
		// On a read-only mount (e.g. /data/call:ro): physical quarantine is
		// impossible. quarantineCorruptAudio returns false, and the path is
		// recorded in the in-memory failedQuarantine skip-set so that subsequent
		// WalkDir passes within the same process lifetime bypass this file
		// silently — eliminating the infinite-retry loop (#158).
		//
		// This whole block stays in the SEQUENTIAL walk so writes to
		// failedQuarantine remain single-goroutine (no mutex required).

		// Skip-set check: if a prior Collect already failed to quarantine this
		// file (ro mount), bypass it silently without re-running the header check.
		if _, skip := c.failedQuarantine[path]; skip {
			return nil
		}

		if err := checkAudioFileHeader(path, ext); err != nil {
			if !quarantineCorruptAudio(path, c.cfg.WhisperAudioDir, info.Size(), err) {
				// Physical quarantine failed (e.g. read-only mount). Record the
				// path in the in-memory skip-set so this file is not re-visited
				// on subsequent collection cycles within this process (#158).
				c.failedQuarantine[path] = struct{}{}
			}
			return nil
		}

		// Passed all filters — defer transcription to the Phase 2 worker pool.
		pending = append(pending, pendingTranscription{
			path:     path,
			info:     info,
			relPath:  relPath,
			sourceID: sourceID,
			title:    strings.TrimSuffix(d.Name(), filepath.Ext(d.Name())),
		})
		return nil
	})
	if err != nil {
		return fmt.Errorf("whisper: walk %q: %w", c.cfg.WhisperAudioDir, err)
	}

	// Phase 2 — CONCURRENT transcription + INCREMENTAL emit.
	//
	// Up to c.concurrency() files are transcribed in parallel (a load balancer can
	// fan requests across multiple whisper backends). Completed documents flow
	// through a results channel to a single drain goroutine that emits them in
	// batches of whisperStreamBatchSize via onBatch. When concurrency == 1 the
	// transcription order is sequential, identical to the original behaviour.
	//
	// transcribeFile / diarizeAudio internals are unchanged; only the dispatch is
	// parallelised and the emit is incremental.
	emitted, streamErr := c.streamPending(ctx, pending, now, onBatch)

	slog.Info("whisper: collected transcripts", "count", emitted, "audio_dir", c.cfg.WhisperAudioDir)
	return streamErr
}

// pendingTranscription is one audio file that passed every sequential filter in
// the walk and is awaiting transcription by the Phase 2 worker pool.
type pendingTranscription struct {
	path     string
	info     fs.FileInfo
	relPath  string
	sourceID string
	title    string
}

// streamPending transcribes the pending items using a worker pool of size
// c.concurrency() and emits the resulting documents incrementally via onBatch in
// batches of whisperStreamBatchSize. It returns the total number of documents
// emitted and an error (the caller's onBatch error if emit failed, a context
// error on cancellation, or nil).
//
// Concurrency model:
//   - A feeder goroutine dispatches pending items on the `jobs` channel.
//   - c.concurrency() worker goroutines transcribe files in parallel and send
//     each completed document on the unbuffered `results` channel.
//   - A closer goroutine waits for the worker WaitGroup and closes `results`.
//   - THIS goroutine is the single drain/emitter: it receives from `results`,
//     buffers documents, and calls onBatch once per whisperStreamBatchSize
//     (plus a final partial flush). Funnelling every onBatch call through ONE
//     goroutine is REQUIRED because the scheduler's processBatch (ledger
//     record, embed, upsert) is not concurrency-safe.
//
// Each completed document is sent exactly once and received by exactly one drain
// iteration, so every file appears in exactly one batch (no duplicates, no
// omissions) regardless of concurrency.
//
// Leak safety: streamPending derives a cancellable streamCtx and defers its
// cancel. Workers and the feeder select on streamCtx.Done() for both their job
// dequeue and their `results <- doc` send, so when the drain loop returns early
// (onBatch error or parent ctx cancellation) the deferred cancel unblocks every
// worker. The closer goroutine then observes the WaitGroup drain and closes
// `results`. No goroutine is left blocked.
//
// When concurrency == 1 a single worker processes the channel, reproducing the
// original sequential transcription order.
func (c *WhisperCollector) streamPending(ctx context.Context, pending []pendingTranscription, now time.Time, onBatch func([]model.Document) error) (int, error) {
	if len(pending) == 0 {
		return 0, nil
	}

	workers := c.concurrency()
	if workers > len(pending) {
		workers = len(pending)
	}

	// streamCtx is cancelled when this function returns (deferred cancel) so that
	// any worker still blocked on a `results <- doc` send — because the drain loop
	// has already returned and stopped receiving — is unblocked and exits. This
	// guarantees the worker WaitGroup eventually drains and the closer goroutine
	// closes `results`, leaving no leaked goroutine.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan pendingTranscription)
	results := make(chan model.Document)

	var wg sync.WaitGroup

	worker := func() {
		defer wg.Done()
		for item := range jobs {
			// Respect cancellation inside the worker so an aborted collection
			// stops issuing expensive transcription calls promptly.
			if streamCtx.Err() != nil {
				return
			}
			doc, ok := c.buildDocument(streamCtx, item, now)
			if !ok {
				continue // failure already logged; partial success
			}
			// Send the completed document to the single drain goroutine. Select on
			// streamCtx.Done() so a cancelled or early-returning collection does
			// not block this worker forever when no receiver remains.
			select {
			case results <- doc:
			case <-streamCtx.Done():
				return
			}
		}
	}

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go worker()
	}

	// Feeder goroutine: dispatch pending items to the worker pool, then close
	// jobs so the workers exit their range loop. Running this concurrently with
	// the drain loop lets emit happen while transcription is still in flight.
	go func() {
		defer close(jobs)
		for _, item := range pending {
			select {
			case jobs <- item:
			case <-streamCtx.Done():
				return
			}
		}
	}()

	// Closer goroutine: once all workers have finished, close results so the
	// drain loop terminates. This is the ONLY place results is closed.
	go func() {
		wg.Wait()
		close(results)
	}()

	// Drain loop (THIS goroutine — the single emitter). Buffers up to
	// whisperStreamBatchSize documents and calls onBatch. onBatch is invoked only
	// here, so it is never called concurrently.
	batch := make([]model.Document, 0, whisperStreamBatchSize)
	emitted := 0

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := onBatch(batch); err != nil {
			return err
		}
		emitted += len(batch)
		batch = batch[:0] // reuse backing array for the next batch
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			// Parent cancellation: emit what is buffered (best-effort) then report
			// the cancellation. The deferred cancel() unblocks the workers.
			_ = flush()
			return emitted, ctx.Err()
		case doc, ok := <-results:
			if !ok {
				// All workers finished and results is closed: emit the final
				// partial batch and return.
				if err := flush(); err != nil {
					return emitted, err
				}
				return emitted, nil
			}
			batch = append(batch, doc)
			if len(batch) >= whisperStreamBatchSize {
				if err := flush(); err != nil {
					// onBatch failed: abort. The deferred cancel() unblocks every
					// worker still attempting to send, so no goroutine leaks.
					return emitted, err
				}
			}
		}
	}
}

// buildDocument transcribes a single pending file and assembles the
// model.Document (sidecar merge + optional diarization). It returns (doc, true)
// on success and (zero, false) when transcription failed — the caller skips
// failures (partial success). This is the per-file body that previously lived
// inline in the walk; it is unchanged except for being extracted so the worker
// pool can call it concurrently. transcribeFile / diarizeAudio internals are
// untouched.
func (c *WhisperCollector) buildDocument(ctx context.Context, item pendingTranscription, now time.Time) (model.Document, bool) {
	txResult, err := c.transcribeFile(ctx, item.path)
	if err != nil {
		slog.Warn("whisper: transcription failed", "path", item.path, "error", err)
		return model.Document{}, false // partial success — skip
	}

	mtime := item.info.ModTime().UTC()
	meta := map[string]any{
		"relative_path": item.relPath,
		"language":      c.cfg.WhisperLanguage,
		"audio_size":    item.info.Size(),
		"model":         c.cfg.WhisperModel,
	}

	// Merge sidecar metadata when present (written by the ingest-recording
	// handler alongside the audio file). Missing sidecar = historical file or
	// OneDrive-staged file — silently skip, no error.
	if sidecarMeta, ok := readRecordingSidecar(item.path); ok {
		for k, v := range sidecarMeta {
			meta[k] = v
		}
	}

	// Use the plain transcript as the default content.
	content := txResult.text

	// Diarization post-processing.
	//
	// Native-diarizing models (gpt-4o-transcribe-diarize) ALWAYS return
	// speaker-labelled segments in the transcription response itself —
	// transcribeFile ALWAYS requests response_format=diarized_json for them,
	// unconditionally of cfg.DiarizationEnabled (see transcribeFile doc
	// comment for why: the model cannot transcribe without also diarizing, so
	// gating on the flag would mean paying the full cloud-transcription cost
	// while discarding the speaker labels we already received — the worst of
	// both worlds, and a silent one). No second API call is made or needed
	// here; this branch is the replacement for the local pyannote microservice
	// for these models.
	//
	// Non-native models (e.g. local whisper.cpp deployments) fall through to
	// the pyannote alignment path, gated by cfg.DiarizationEnabled as before
	// (unchanged since #111): a second request is sent to DiarizationAPIURL
	// and the result is aligned against the verbose_json segments via
	// labelTranscript. This is the ONLY branch where DiarizationEnabled still
	// has meaning.
	//
	// Both paths converge on the same renderSpeakerBlocks formatter so stored
	// content is identical in shape ("[화자N] text" grouped blocks) regardless
	// of which path produced it; meta["diarization"] records which one did,
	// for observability. The native path does NOT go through labelTranscript's
	// overlap alignment — each diarizedSegment already carries its own
	// authoritative speaker, so alignment would only introduce error (see
	// nativeDiarizedSegsToRawLabelled doc comment).
	switch {
	case modelSupportsNativeDiarization(c.cfg.WhisperModel):
		if len(txResult.diarized) > 0 {
			labelled, nSpeakers := renderSpeakerBlocks(nativeDiarizedSegsToRawLabelled(txResult.diarized))
			if labelled != "" {
				content = labelled
				meta["speaker_count"] = nSpeakers
				meta["diarization"] = "native"
			} else {
				slog.Warn("whisper: renderSpeakerBlocks produced empty output for native diarization — using plain transcript",
					"path", item.path)
			}
		}
		// len(txResult.diarized) == 0: transcribeFile already warned and
		// content already defaults to the flat text — nothing more to do.

	case c.cfg.DiarizationEnabled && c.cfg.DiarizationAPIURL != "" && len(txResult.segments) > 0:
		audioBytes, readErr := os.ReadFile(item.path)
		if readErr != nil {
			slog.Warn("whisper: diarization skipped — cannot re-read audio file",
				"path", item.path, "error", readErr)
		} else {
			isCall := isTPhoneCallPath(item.path)
			diarSegs, diarErr := c.diarizeAudio(ctx, item.path, audioBytes, isCall)
			if diarErr != nil {
				slog.Warn("whisper: diarization failed — using plain transcript",
					"path", item.path, "error", diarErr)
			} else {
				labelled, nSpeakers := labelTranscript(txResult.segments, diarSegs)
				if labelled != "" {
					content = labelled
					meta["speaker_count"] = nSpeakers
					meta["diarization"] = "pyannote"
				} else {
					slog.Warn("whisper: labelTranscript produced empty output — using plain transcript",
						"path", item.path)
				}
			}
		}
	}

	// PII redaction (issue #163), now gated by cfg.PIIRedactionEnabled (default
	// false — see that flag's doc comment in internal/config/config.go for the
	// owner's rationale: this is a single-user personal knowledge base, and a
	// transcript reading "[REDACTED]" instead of the actual phone number is
	// strictly less useful to the person who owns that data). When enabled,
	// this reproduces the original #163 behaviour byte-for-byte: redaction is
	// applied unconditionally to ALL whisper transcript content — not just
	// TPhone call recordings — because voice memos can equally contain a
	// spoken phone number or account number, and distinguishing "call" vs
	// "memo" here to redact only one would leave an unprincipled gap for no
	// real benefit. Applied to Content only: SourceID (dedup/upsert identity)
	// and Metadata are left untouched either way so dedup behaviour is
	// unaffected by this flag.
	// title defaults to the raw filename stem, unmodified — "stored exactly as
	// produced" when redaction is disabled. The underscore→space swap below
	// exists ONLY to make RedactPII's \b-boundary patterns match (see the
	// comment inside the branch); it is not an independent formatting feature,
	// so it stays inside the PIIRedactionEnabled branch rather than applying
	// unconditionally.
	title := item.title
	if c.cfg.PIIRedactionEnabled {
		content = smsmap.RedactPII(content)

		// Title redaction (issue #163 follow-up): item.title is the audio
		// filename stem, and historical filenames (e.g. TPhoneCallRecords'
		// "<number>_<timestamp>.ext") embed a raw phone number just as
		// directly as spoken content does — so Title carries the same PII
		// risk as Content and must go through the same RedactPII pass.
		//
		// RedactPII's structured-PII patterns (koreanPhoneRe, rrnRe) require a
		// non-word character immediately after the digit run to satisfy their
		// trailing \b boundary; in a filename stem the digit run instead
		// directly abuts the timestamp segment's leading "_" (both are word
		// characters), which defeats \b and would silently leave the number
		// unredacted. Since underscores in a filename-derived title are purely
		// a display separator (no semantic meaning worth preserving over the
		// security property), swapping them for spaces before redaction lets
		// the boundary-sensitive patterns match without touching the shared
		// smsmap regexes used by other callers (SMS bodies, spoken transcript
		// content) that don't have this adjacency problem. This only changes
		// the Title *value* stored on the document — it does not touch the
		// on-disk filename or SourceID.
		title = smsmap.RedactPII(strings.ReplaceAll(item.title, "_", " "))
	}

	return model.Document{
		ID:          uuid.New(),
		SourceType:  model.SourceCallTranscript,
		SourceID:    item.sourceID,
		Title:       title,
		Content:     content,
		Metadata:    meta,
		OccurredAt:  &mtime,
		CollectedAt: now,
	}, true
}

// readRecordingSidecar attempts to read and parse the sidecar metadata file
// written by the ingest-recording handler at audioPath + ".meta.json".
//
// When the sidecar exists and is valid JSON, the function returns a map
// containing the recording metadata fields present in the file
// (contact_name, direction, recording_type, duration_seconds, and — issue
// #164 policy reversal, additive — number) and true. Only non-empty/non-zero
// values are included so callers do not overwrite existing metadata with
// zero-value defaults.
//
// number (issue #164 additive improvement): the sidecar's "number" key is
// only present when the ingest-recording handler wrote it with
// PIINumberHashingEnabled=false (see recordingSidecar's doc comment in
// internal/api/ingest_recording.go). When present, it is surfaced here as
// Metadata["number"] on the call-transcript document, making the phone number
// searchable/visible — the point of disabling hashing. The sidecar's
// "number_hash" key is deliberately NOT parsed into metadata: a hash provides
// no searchability benefit, so surfacing it would add noise without value.
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
		Number          string `json:"number"`
		Direction       string `json:"direction"`
		RecordingType   string `json:"recording_type"`
		DurationSeconds int    `json:"duration_seconds"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		// Corrupt sidecar — skip without surfacing an error.
		return nil, false
	}

	result := make(map[string]any, 5)
	if raw.ContactName != "" {
		result["contact_name"] = raw.ContactName
	}
	if raw.Number != "" {
		result["number"] = raw.Number
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
	text     string            // flat transcript text
	segments []whisperSegment  // time-stamped segments (verbose_json / pyannote path); may be nil/empty for older servers
	diarized []diarizedSegment // speaker-labelled segments (diarized_json / native-diarizing models); nil unless requested
}

// transcribeFile posts the audio file at path to the Whisper transcription
// endpoint and returns the transcript text plus, depending on which request
// shape was used, either time-stamped segments (verbose_json — pyannote
// alignment path) or speaker-labelled segments (diarized_json — native
// diarization path).
//
// The request is a multipart/form-data POST to {baseURL}/audio/transcriptions
// with the "file", "model", and "language" fields always present (language
// omitted when empty). The remaining fields are chosen in priority order:
//
//  1. modelSupportsNativeDiarization(cfg.WhisperModel): ALWAYS
//     response_format=diarized_json, chunking_strategy=cfg.WhisperChunkingStrategy
//     (field OMITTED entirely when the config value is empty string — NOT sent
//     as an empty value) — regardless of cfg.DiarizationEnabled.
//
//     cfg.DiarizationEnabled is deliberately NOT consulted here. For a
//     native-diarizing model, transcription and diarization are the same API
//     call — there is no cheaper "just transcribe" request to fall back to
//     when the flag is off. Gating this branch on the flag (as an earlier
//     version of this function did) would mean: with DIARIZATION_ENABLED at
//     its default false, every call recording still gets sent to the cloud
//     model (that decision is made by WHISPER_API_URL + WHISPER_MODEL, not by
//     this flag) but the code would deliberately request response_format
//     other than diarized_json, discarding the speaker labels the model
//     already computed as part of that same request. That is full privacy
//     cost for zero benefit, and it fails silently — no error, just
//     unlabelled text. So: if the model is native-diarizing, always ask for
//     diarized_json.
//
//     chunking_strategy is REQUIRED by the live API for the response to
//     contain a "segments" array at all: verified against the real OpenAI
//     endpoint, omitting it yields a text-only response and the model
//     degenerates into repeating text. The response is parsed via
//     diarizedResponse; speaker values are "A", "B", … per the verified
//     response shape.
//
//  2. Non-native model AND cfg.DiarizationEnabled (e.g. a local whisper.cpp
//     server that understands verbose_json but not diarized_json, paired with
//     the pyannote microservice): response_format=verbose_json, parsed via
//     whisperVerboseResponse. IDENTICAL to the #111 behaviour.
//     cfg.DiarizationEnabled retains its original meaning ONLY for this
//     branch — it gates the legacy pyannote alignment path, not native
//     diarization.
//
//  3. Neither: no response_format field at all, plain {"text":"..."} response
//     parsed via whisperTranscribeResponse. IDENTICAL to the original
//     pre-#111 implementation — this preserves zero behaviour change for
//     local whisper.cpp deployments running with the flag off.
//
// Cases 2 and 3 exist so local whisper.cpp deployments (which do not speak
// diarized_json) are byte-for-byte unaffected by the introduction of the
// native diarization path.
//
// The Authorization header is set only when cfg.WhisperAPIKey is non-empty,
// enabling use with local whisper.cpp servers that do not require authentication.
func (c *WhisperCollector) transcribeFile(ctx context.Context, path string) (transcribeFileResult, error) {
	audioBytes, err := os.ReadFile(path)
	if err != nil {
		return transcribeFileResult{}, fmt.Errorf("read audio file: %w", err)
	}

	// See doc comment above: native diarization is requested unconditionally
	// of cfg.DiarizationEnabled, which retains its original meaning only for
	// the legacy pyannote path (case 2 below).
	nativeDiarize := modelSupportsNativeDiarization(c.cfg.WhisperModel)

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

	switch {
	case nativeDiarize:
		// diarized_json is the only format that returns speaker labels for
		// native-diarizing models. See doc comment above for why
		// chunking_strategy is required.
		if err := mw.WriteField("response_format", "diarized_json"); err != nil {
			return transcribeFileResult{}, fmt.Errorf("write response_format field: %w", err)
		}
		if c.cfg.WhisperChunkingStrategy != "" {
			if err := mw.WriteField("chunking_strategy", c.cfg.WhisperChunkingStrategy); err != nil {
				return transcribeFileResult{}, fmt.Errorf("write chunking_strategy field: %w", err)
			}
		}
	case c.cfg.DiarizationEnabled:
		// Legacy pyannote-alignment path: verbose_json gives per-segment
		// timestamps, unchanged since #111.
		if err := mw.WriteField("response_format", "verbose_json"); err != nil {
			return transcribeFileResult{}, fmt.Errorf("write response_format field: %w", err)
		}
	}
	// Neither branch taken: no response_format field — original pre-#111 shape.

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

	// Parse response according to the request shape actually sent above.
	switch {
	case nativeDiarize:
		var result diarizedResponse
		if err := json.Unmarshal(body, &result); err != nil {
			return transcribeFileResult{}, fmt.Errorf("decode response JSON: %w", err)
		}
		if len(result.Segments) == 0 {
			// Observed when chunking_strategy is missing/misconfigured, or for
			// very short clips the model chooses not to segment. Fall back to
			// the flat text rather than erroring — buildDocument still
			// produces a (non-diarized) document rather than dropping the
			// file entirely.
			slog.Warn("whisper: diarized_json response had zero segments — falling back to flat text",
				"path", path, "model", c.cfg.WhisperModel)
			return transcribeFileResult{text: result.Text}, nil
		}
		return transcribeFileResult{text: result.Text, diarized: result.Segments}, nil

	case !c.cfg.DiarizationEnabled:
		// Plain {"text":"..."} — identical to the original pre-#111 path.
		var result whisperTranscribeResponse
		if err := json.Unmarshal(body, &result); err != nil {
			return transcribeFileResult{}, fmt.Errorf("decode response JSON: %w", err)
		}
		return transcribeFileResult{text: result.Text}, nil

	default:
		// verbose_json {"text":"...","segments":[...]} for pyannote alignment.
		var result whisperVerboseResponse
		if err := json.Unmarshal(body, &result); err != nil {
			return transcribeFileResult{}, fmt.Errorf("decode response JSON: %w", err)
		}
		return transcribeFileResult{
			text:     result.Text,
			segments: result.Segments,
		}, nil
	}
}

// isTPhoneCallPath reports whether path looks like a TPhoneCallRecords audio
// file based on the reTPhone filename pattern. When true, the LEGACY pyannote
// diarization request (diarizeAudio) is sent with num_speakers=2 (phone calls
// are always two-party).
//
// Heuristic: the TPhone pattern requires ANY prefix followed by a 14-digit
// timestamp (see reTPhone — it anchors only on the trailing
// "_YYYYMMDDHHMMSS[-N].ext" suffix, not on the prefix's content). This still
// matches the post-#164 upload filename scheme
// ("{numHash}_YYYYMMDDHHMMSS.ext", where the raw phone number was replaced by
// its one-way hash to stop plaintext-number persistence): only the prefix
// changed, and the function never inspected the prefix. Voice memos and other
// recordings without a trailing 14-digit timestamp do not match, so
// num_speakers is omitted for them.
//
// This function (and reTPhone, which recordingTime's Pattern B also uses to
// extract the recording timestamp) is NOT dead code, but it is only ever
// CALLED from the legacy pyannote branch in buildDocument. A native-diarizing
// model (gpt-4o-transcribe-diarize) never reaches that branch — see the
// buildDocument switch — because the model returns per-segment speakers
// directly and has no num_speakers-style hint parameter to feed. So for
// audio files transcribed via the cloud native-diarize path specifically,
// this function is simply never invoked; it remains load-bearing for the
// local-inference (`local-inference` compose profile) pyannote fallback.
func isTPhoneCallPath(path string) bool {
	return reTPhone.MatchString(filepath.Base(path))
}

// diarizeAudio posts the audio bytes to the diarization microservice and
// returns the speaker segments. On any error (service unavailable, bad
// response, empty segments, non-local endpoint) the error is returned and the
// caller should fall back to the plain transcript.
//
// When isTPhoneCall is true the request includes num_speakers=2; otherwise
// the field is omitted and the service auto-detects the number of speakers.
//
// Locality guard (#165): unlike transcribeFile's endpoint — where a non-local
// URL is only logged as a warning once per collection cycle in CollectStream,
// since deliberately routing whisper to a cloud API is a supported (if
// discouraged) operator choice — DiarizationAPIURL is a SEPARATE,
// independently-configured endpoint that receives the SAME raw call-audio
// bytes. It had no locality check at all before this fix, so a misconfigured
// or malicious DIARIZATION_API_URL would silently ship raw call audio off the
// machine. This check runs first, before any audio bytes are placed in the
// request body, and REFUSES the call outright (hard error, not a warn-and-
// proceed) rather than mirroring the softer transcribe-endpoint behaviour —
// audio bytes must never leave the machine for diarization. The caller
// (buildDocument) already treats any diarizeAudio error as "fall back to the
// plain transcript" (see the existing diarErr handling), so refusing here
// reuses that same degrade path with no additional plumbing required.
//
// This is a cheap fast-fail pre-flight only (#169): it inspects the URL
// string and, for multi-label hostnames, does a one-off DNS lookup — it does
// NOT protect against a single-label hostname being blindly trusted, nor
// against the address the transport actually dials later differing from what
// this check observed (DNS rebinding). The request below is made through
// c.diarizeHTTPClient, whose Transport.DialContext (verifiedDialContext)
// performs the real, dial-time defence: it re-verifies locality of every
// resolved address and pins the dial to the exact address it verified.
//
// Placed once at the top of diarizeAudio (the sole call site for the
// diarization request) so every caller is covered without needing a
// per-call-site guard.
func (c *WhisperCollector) diarizeAudio(ctx context.Context, path string, audioBytes []byte, isTPhoneCall bool) ([]diarSegment, error) {
	if !isLocalWhisperEndpoint(c.cfg.DiarizationAPIURL) {
		slog.Warn("whisper: diarization endpoint does not appear to be local — refusing to send call audio for diarization; falling back to plain transcript",
			"endpoint", c.cfg.DiarizationAPIURL,
		)
		return nil, fmt.Errorf("diarization endpoint %q is not local; refusing to send audio (#165)", c.cfg.DiarizationAPIURL)
	}

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

	// diarizeHTTPClient (not the shared httpClient used by transcribeFile)
	// enforces dial-time locality verification — see verifiedDialContext.
	resp, err := c.diarizeHTTPClient.Do(req)
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

// defaultDiarizeResolveHost is the production DNS resolver used by
// verifiedDialContext when diarizeResolveHost is not overridden. Tests inject
// an alternative to simulate DNS-rebinding scenarios deterministically.
func defaultDiarizeResolveHost(ctx context.Context, host string) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, host)
}

// verifiedDialContext is the DialContext used by diarizeHTTPClient's
// Transport (#169). It is the real defence-in-depth layer behind the cheap
// isLocalWhisperEndpoint pre-flight check in diarizeAudio, and closes two
// gaps that check leaves open:
//
//  1. Single-label hostnames (e.g. a Docker/compose service name) are
//     blindly trusted as local by isLocalWhisperEndpoint with NO resolution
//     at all. Here, a non-IP-literal host is ALWAYS resolved and every
//     returned address is verified before any address is dialled.
//  2. Even isLocalWhisperEndpoint's multi-label DNS-resolution branch has a
//     classic check/dial TOCTOU (DNS rebinding): the address it looked up
//     during the pre-flight check is never pinned to what the transport
//     subsequently dials — a second lookup (e.g. from a malicious or
//     misconfigured resolver) could return a different, public address.
//     Here the dial ALWAYS targets the exact address this function just
//     verified; it is never re-resolved.
//
// Algorithm:
//  1. Split addr into host and port (addr is what net/http passes for the
//     network connection, e.g. "diarize-host:8080").
//  2. If host is already a numeric IP literal, verify it directly.
//  3. Otherwise resolve host via diarizeResolveHost (net.DefaultResolver by
//     default; injectable in tests) and verify EVERY returned address is
//     loopback/RFC-1918/link-local. Any non-local address anywhere in the
//     result refuses the entire dial.
//  4. Dial ONLY the first verified address (pinned) — never the original
//     hostname, so no further resolution can occur between verification and
//     connection.
//
// On any refusal, a descriptive error is returned and no connection is
// attempted to the untrusted address.
func (c *WhisperCollector) verifiedDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("whisper: diarization dial: invalid address %q: %w", addr, err)
	}

	var pinnedIP string

	if ip := net.ParseIP(host); ip != nil {
		// Numeric IP literal — verify directly, no resolution needed.
		if !isPrivateOrLinkLocalIP(ip) {
			return nil, fmt.Errorf("whisper: diarization dial: address %q is not local — refusing dial (#169)", host)
		}
		pinnedIP = host
	} else {
		resolve := c.diarizeResolveHost
		if resolve == nil {
			resolve = defaultDiarizeResolveHost
		}
		addrs, resolveErr := resolve(ctx, host)
		if resolveErr != nil {
			return nil, fmt.Errorf("whisper: diarization dial: resolve %q: %w", host, resolveErr)
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("whisper: diarization dial: no addresses resolved for %q", host)
		}
		for _, a := range addrs {
			ip := net.ParseIP(a)
			if ip == nil || !isPrivateOrLinkLocalIP(ip) {
				return nil, fmt.Errorf("whisper: diarization dial: resolved address %q for host %q is not local — refusing dial (#169 DNS-rebinding guard)", a, host)
			}
		}
		// Pin the dial to the FIRST verified address. It is never re-resolved,
		// so a later (possibly different, e.g. rebound) DNS answer can never be
		// substituted between this check and the actual connection.
		pinnedIP = addrs[0]
	}

	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return dialer.DialContext(ctx, network, net.JoinHostPort(pinnedIP, port))
}

// whisperQuarantineDir is the subdirectory name within WhisperAudioDir where
// corrupt or unreadable audio files are moved for manual review.
const whisperQuarantineDir = ".quarantine"

// quarantineCorruptAudio moves a corrupt or unreadable audio file to the
// quarantine subdirectory (<audioDir>/.quarantine/) and logs a single
// diagnostic message. After a successful move the file is no longer in the
// audio tree so subsequent walks will not revisit it, breaking the
// infinite-retry loop caused by permanently-corrupt files (#152).
//
// On read-only mounts (e.g. /data/call:ro) os.MkdirAll and os.Rename both
// fail with EROFS. In that case the function returns false and the caller
// records the path in an in-memory skip-set (failedQuarantine) to prevent
// re-visiting within the current process lifetime (#158).
//
// Physical quarantine to a separate rw volume would persist the block across
// restarts, but the marginal benefit (one extra warning on restart) does not
// justify the operational complexity of adding a second mount. The skip-set
// fully eliminates the loop for the current process lifetime.
//
// Returns true when the file was successfully moved to quarantine,
// false when mkdir or rename failed (caller must use skip-set).
//
// Sidecar files (<path>.meta.json) are moved alongside the audio file when
// present, so that provenance information is preserved for manual inspection.
func quarantineCorruptAudio(path, audioDir string, sizeBytes int64, reason error) bool {
	qDir := filepath.Join(audioDir, whisperQuarantineDir)
	if err := os.MkdirAll(qDir, 0o755); err != nil {
		slog.Warn("whisper: cannot create quarantine dir — corrupt file added to in-memory skip-set (#158)",
			"path", path,
			"quarantine_dir", qDir,
			"size_bytes", sizeBytes,
			"reason", reason,
			"mkdir_error", err,
		)
		return false
	}

	dest := filepath.Join(qDir, filepath.Base(path))
	if err := os.Rename(path, dest); err != nil {
		slog.Warn("whisper: quarantine move failed — corrupt file added to in-memory skip-set (#158)",
			"path", path,
			"dest", dest,
			"size_bytes", sizeBytes,
			"reason", reason,
			"move_error", err,
		)
		return false
	}

	slog.Warn("whisper: corrupt audio file quarantined — will not be retried",
		"path", path,
		"quarantine", dest,
		"size_bytes", sizeBytes,
		"reason", reason,
	)

	// Move sidecar alongside the audio file (best-effort: no error on absence).
	sidecar := path + ".meta.json"
	if _, err := os.Stat(sidecar); err == nil {
		sidecarDest := dest + ".meta.json"
		if err := os.Rename(sidecar, sidecarDest); err != nil {
			slog.Warn("whisper: could not move sidecar to quarantine",
				"sidecar", sidecar,
				"dest", sidecarDest,
				"error", err,
			)
		}
	}
	return true
}
