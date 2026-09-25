package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
//
// Reason 은 Skipped 일 때 그 사유 코드다(recordingSkipReason*). 앱
// (ApiModels.kt RecordingResponse)은 모르는 키를 무시하므로 더해도 안전하다.
type IngestRecordingResponse struct {
	Accepted   bool   `json:"accepted"`
	Skipped    bool   `json:"skipped"`
	Reason     string `json:"reason,omitempty"`
	DocumentID string `json:"document_id,omitempty"`
}

// 녹음 응답의 skipped 사유 코드. 값은 고정이며 입력을 담지 않는다.
const (
	// recordingSkipReasonCutover: 컷오버 이전 녹음이라 파일도 문서도 만들지
	// 않았다(accepted=false).
	recordingSkipReasonCutover = "cutover"
	// recordingSkipReasonDuplicate: 오디오·사이드카는 저장했고(accepted=true)
	// WhisperCollector 가 전사한다. 다른 source_id 의 활성 통화 문서와 요약
	// 내용이 같아(store.ErrDuplicateTranscript) 이 녹음의 대기(pending) 문서만
	// 만들지 않았다. 다시 보내도 결과가 같다.
	recordingSkipReasonDuplicate = "duplicate_content"
)

// 녹음 업로드의 읽기·쓰기 기한(#292).
//
// 서버 전체의 ReadTimeout(15초, cmd/server/main.go)은 요청 본문까지 포함한다.
// 모바일 망에서 수십 MiB 녹음을 올리면 15초를 넘길 수 있고, 그러면 본문 읽기가
// i/o timeout 으로 끊긴다. 예전에는 이것이 400 이 되어 앱이 '전송 완료'로
// 표시했고(영구 유실), 이번에 일시 오류(503)로 바꾸면 같은 파일이 매번 같은
// 이유로 끊겨 앱의 녹음 루프가 거기서 멈춘다(poison — 그 뒤 녹음 전부가 막힘).
// 그래서 이 핸들러는 기한을 앱의 호출 제한(OkHttp callTimeout 180초,
// SyncWorker.buildUploader)보다 길게 늘린다. 이 기한에 서버가 먼저 끊는 일은
// 없고, 그보다 느린 업로드는 앱이 스스로 끊어 네트워크 오류(일시)로 본다.
// 인증 미들웨어를 통과한 요청에서만 늘어나므로 느린 클라이언트 방어(slowloris)는
// 인증 전 단계에서 그대로다.
const (
	recordingReadDeadline  = 200 * time.Second
	recordingWriteDeadline = recordingReadDeadline + 30*time.Second
)

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
//  4. Create a model.Document (SourceType=SourceCall) with the same 4-line
//     call summary content as smsmap.MapCall (metadata.transcription="pending"
//     for kind=call; voice-memo keeps a "[TRANSCRIPTION PENDING]" placeholder,
//     since it has no summary format of its own) and idempotently upsert it.
//     WhisperCollector transcribes the audio on its next scheduled run and,
//     for kind=call recordings, merges the transcript into THIS SAME document
//     via store.AttachTranscript (see model.SourceCall's doc comment) rather
//     than creating a second document.
//  5. The upsert is idempotent: same inputs → same SourceID.
//     - call:       call-log:{date_ms}:{numHash}:{durHash}  (mirrors smsmap.MapCall)
//     - voice-memo: call-log:voice-memo:{hash(originalFilename)}
//     Hash is over the original upload filename only — dateMs is excluded so
//     that the same file re-uploaded with a different timestamp produces the
//     same SourceID (fully idempotent). Different filenames still produce
//     distinct IDs.
//
// 응답 계약(#292). 폰 앱(Uploader.handleRecordingResponse·SyncWorker)의 동작과
// 짝을 이룬다. 상태 코드가 곧 "이 파일을 다시 보내야 하는가" 다.
//
//	201 accepted            저장 완료. 앱은 전송 완료로 표시한다.
//	200 accepted+skipped    오디오는 저장, 대기 문서는 중복이라 생략
//	                        (reason=duplicate_content). 전송 완료.
//	200 skipped             컷오버 이전(reason=cutover). 전송 완료.
//	400·413·422             다시 보내도 결과가 같은 영구 오류. 앱은 전송
//	                        완료로 표시하고 다음 파일로 넘어간다 — 이 파일은
//	                        다시 오지 않는다. 그래서 형식 오류·손상 오디오·
//	                        크기 초과·DB 가 입력 때문에 거부한 경우에만 쓴다.
//	503 + Retry-After       일시 오류(업로드 절단·읽기 시간 초과·디스크 오류·
//	                        DB 장애). 앱은 표시하지 않고 이번 실행의 녹음
//	                        루프를 멈춘 뒤 다음 실행에 같은 파일부터 다시 보낸다.
//
// 반대로 일시 오류에 4xx 를 주면 그 녹음은 영구히 유실되고, 영구 오류에 5xx 를
// 주면 그 파일에서 녹음 루프가 매번 멈춰 뒤의 녹음이 전부 막힌다(poison).
// 로그에는 파일 이름·경로·번호·연락처를 남기지 않는다 — 통화 녹음 파일 이름에는
// 전화번호가 들어 있다(PII 해싱을 끈 기본 설정에서 저장 파일 이름도 번호다).
func (s *Server) ingestRecordingHandler(w http.ResponseWriter, r *http.Request) {
	if s.recordingUpserter == nil || s.recordingDir == "" {
		writeError(w, http.StatusServiceUnavailable, "recording ingest not configured")
		return
	}

	// 인증이 꺼져 있으면(API_KEY 없음 → requireAPIKey 가 통과시킴) 기한을
	// 늘리지 않는다. 늘리면 인증 없는 누구나 200초 동안 연결을 붙잡고 100 MiB 를
	// 밀어 넣을 수 있다. 시작 시 경고한다(cmd/server/main.go).
	if s.apiKey != "" {
		extendRecordingDeadlines(w)
	}

	// Enforce the per-upload size limit.
	maxBytes := s.recordingMaxFileBytes
	body := http.MaxBytesReader(w, r.Body, maxBytes)
	// readErrRecorder(ingest_messages.go) 는 본문을 읽다가 난 첫 전송 계층
	// 오류를 기억한다. multipart 파서는 그 오류를 감싸거나 다른 문구로 바꿔
	// 돌려주므로, 업로드가 잘렸는지(일시)·보낸 본문 자체가 틀렸는지(영구)는
	// 리더가 실제로 돌려준 오류로 가른다.
	bodyRec := &readErrRecorder{r: body}
	r.Body = recordingBody{Reader: bodyRec, Closer: body}

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		switch recordingFormErrClass(err, bodyRec.err) {
		case recordingFormTooLarge:
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("file exceeds maximum upload size of %d bytes", maxBytes))
		case recordingFormTransient:
			slog.Warn("ingest_recording: multipart read failed, returning 503",
				recordingErrAttrs(firstErr(bodyRec.err, err))...)
			writeIngestUnavailable(w, ingestUnavailableMsg)
		default:
			slog.Warn("ingest_recording: malformed multipart form, returning 400",
				"err_type", fmt.Sprintf("%T", err))
			writeError(w, http.StatusBadRequest, "invalid multipart form")
		}
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
			Reason:   recordingSkipReasonCutover,
		})
		return
	}

	// --- Read the audio file part ---
	f, fh, err := r.FormFile("file")
	if err != nil {
		if !errors.Is(err, http.ErrMissingFile) {
			// 파싱은 끝났는데 파트를 못 연다 — 32 MiB 를 넘어 디스크에 내린
			// 임시 파일을 열지 못한 경우다(디스크·임시 디렉터리 문제).
			slog.Error("ingest_recording: open uploaded part failed, returning 503", recordingErrAttrs(err)...)
			writeIngestUnavailable(w, ingestUnavailableMsg)
			return
		}
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
		// 본문은 이미 다 받았다. 여기서 나는 오류는 임시 파일 읽기(디스크) 오류다.
		slog.Error("ingest_recording: read audio bytes failed, returning 503", recordingErrAttrs(err)...)
		writeIngestUnavailable(w, ingestUnavailableMsg)
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
		// 파일 이름은 남기지 않는다 — 통화 녹음 파일 이름에 번호·연락처가 있다.
		// validationErr 는 audiovalidate 의 고정 문구다.
		slog.Warn("ingest_recording: rejecting corrupt audio upload",
			"kind", kind,
			"size_bytes", len(audioBytes),
			"reason", validationErr,
		)
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("audio file appears corrupt or invalid: %v", validationErr))
		return
	}

	// --- Determine audio file extension ---
	ext := recordingDiskExt(uploadedFilename)

	// --- Build output filename ---
	// WhisperCollector.recordingTime() parses the timestamp suffix to extract the
	// recording timestamp, so existing cutover / watermark logic works for both kinds.
	localTime := recordedAt.In(time.Local)
	timestampStr := localTime.Format("20060102150405")

	var audioFilename string
	var sourceID string
	// numHash is the ShortHash of the counterpart phone number, computed only
	// for kind=call. It is reused below when writing the sidecar so the raw
	// phone number is never persisted to disk in plaintext (issue #164).
	var numHash string

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
			sanitized := boundedRecordingStem(sanitizeFilename(baseName), filenameHash)
			audioFilename = fmt.Sprintf("voice-memo_%s_%s%s", timestampStr, sanitized, ext)
		} else {
			// No original filename: fall back to timestamp-only form (legacy behaviour).
			audioFilename = fmt.Sprintf("voice-memo_%s%s", timestampStr, ext)
		}
	default: // "call"
		// Mirrors smsmap.MapCall SourceID format. This hashing is UNCONDITIONAL
		// — independent of s.piiNumberHashingEnabled — since changing the
		// SourceID scheme would orphan every already-ingested call-log document
		// (dedup/upsert identity). numHash is also reused below for the
		// audioFilename when hashing IS enabled (see the audioFilename comment).
		numHash = smsmap.ShortHash(number)
		durHash := smsmap.BodyShortHash(fmt.Sprintf("%d", durationSec))
		sourceID = fmt.Sprintf("call-log:%d:%s:%s", dateMs, numHash, durHash)

		// Format when s.piiNumberHashingEnabled: {numHash}_{YYYYMMDDHHMMSS}{ext}
		// Format when disabled (default, issue #164 policy reversal):
		// {sanitizePhoneNumber(number)}_{YYYYMMDDHHMMSS}{ext} — the pre-#164
		// behaviour, restored verbatim via sanitizePhoneNumber (digits/+/- only).
		//
		// Security note (retained for the hashing-enabled path): embedding
		// numHash instead of the raw number keeps PII out of filesystem paths,
		// logs, backups, and any sync tooling that mirrors the recording
		// directory. With hashing disabled this protection is intentionally
		// traded away — see PIINumberHashingEnabled's doc comment in
		// internal/config/config.go for the owner's rationale.
		//
		// WhisperCollector.recordingTime() (reTPhone) only parses the trailing
		// "_YYYYMMDDHHMMSS[-N]" timestamp suffix and does not care what
		// precedes it, so either naming choice is fully transparent to the
		// cutover / watermark logic and to isTPhoneCallPath's diarization
		// heuristic.
		filenamePrefix := numHash
		if !(s.piiNumberHashingEnabled || s.piiNameRedactionEnabled) {
			filenamePrefix = sanitizePhoneNumber(number)
			// 번호 필드는 길이 제한이 없다. 너무 길면 파일 이름 한도(255바이트)를
			// 넘겨 쓰기가 매번 ENAMETOOLONG(503)으로 실패하는 poison 이 되므로
			// 해시로 대신한다(recordingNameMaxStemBytes 주석).
			if len(filenamePrefix) > recordingNumberPrefixMaxBytes {
				filenamePrefix = numHash
			}
		}
		audioFilename = fmt.Sprintf("%s_%s%s",
			filenamePrefix,
			timestampStr,
			ext,
		)
	}

	// --- Ensure the recording directory exists ---
	// 디스크 오류는 모두 일시 오류(503)다. 폰에 원본이 남아 있으므로 다시 받으면
	// 되고, 4xx 를 주면 앱이 전송 완료로 표시해 그 녹음이 영구히 유실된다
	// (2026-08-26 bind mount 권한 장애가 이 모양이었다). 로그에는 오류 종류와
	// errno 만 남긴다 — *fs.PathError 의 문구에는 저장 경로(= 번호가 든 파일
	// 이름)가 들어 있다.
	if err := os.MkdirAll(s.recordingDir, 0o755); err != nil {
		slog.Error("ingest_recording: mkdir failed, returning 503", recordingErrAttrs(err)...)
		writeIngestUnavailable(w, ingestUnavailableMsg)
		return
	}

	// --- 사이드카 메타데이터를 쓰고, 그다음 오디오 파일을 쓴다 ---
	// The sidecar decouples the call-log doc (created here) from the
	// call-transcript doc (created later by WhisperCollector). WhisperCollector
	// reads {audioPath}.meta.json and merges its fields into the transcript
	// document metadata, giving call-transcript docs the full recording context
	// (contact_name, direction, recording_type, duration_seconds) without
	// requiring any coupling between the two pipelines.
	//
	// 사이드카 쓰기 실패도 503 이다(#292, 예전에는 Warn 만 남기고 201). 사이드카가
	// 없으면 WhisperCollector 가 이 녹음을 통화 문서에 병합하지 못하고(callLog
	// MergeSourceID 는 사이드카의 kind·date_ms·번호로 source_id 를 만든다) 따로
	// 떨어진 전사 문서를 만든다. 전사 원장이 같은 오디오의 재전사를 막으므로 한
	// 번 그렇게 되면 되돌릴 수 없다. 다시 받으면 같은 경로에 같은 내용을 쓰므로
	// 재업로드는 무해하다.
	//
	// 순서와 원자성: 사이드카를 먼저, 오디오를 나중에 쓰고, 둘 다 임시 파일에 쓴
	// 뒤 이름을 바꾼다(writeFileAtomic). WhisperCollector 는 오디오 확장자 파일만
	// 훑으므로, 오디오가 보이는 순간 사이드카는 이미 있다. 그리고 디스크가 차서
	// 쓰기가 도중에 실패해도 잘린 오디오가 보이지 않는다 — 잘린 파일이 먼저
	// 전사되면 원장에 올라, 재전송이 온전한 파일을 써도 다시 전사되지 않는다.
	// 임시 파일 이름은 .partial 로 끝나 WhisperCollector 가 무시한다.
	//
	// Security (issue #164), now conditional on s.piiNumberHashingEnabled: when
	// enabled, the counterpart phone number is never written to the sidecar in
	// plaintext — only its ShortHash (numHash, already computed above for the
	// SourceID) is stored, under the number_hash key, a one-way hash the
	// sidecar cannot be used to recover the raw number from. When disabled
	// (default, policy reversal), the raw number is written under the "number"
	// key instead — number_hash is omitted entirely — so WhisperCollector's
	// sidecar reader (readRecordingSidecar in internal/collector/whisper.go)
	// can additively surface it as Metadata["number"] on the eventual
	// call-transcript document (see that function's doc comment).
	destPath := filepath.Join(s.recordingDir, audioFilename)
	sidecarDirection := ""
	if kind == "call" {
		sidecarDirection = "incoming"
	}
	sidecar := recordingSidecar{
		ContactName:     contactName,
		Direction:       sidecarDirection,
		RecordingType:   kind,
		DurationSeconds: durationSec,
		DateMs:          dateMs,
		Kind:            kind,
	}
	if s.piiNumberHashingEnabled || s.piiNameRedactionEnabled {
		sidecar.NumberHash = numHash
	} else if kind == "call" {
		sidecar.Number = number
	}
	if s.piiNameRedactionEnabled && sidecar.ContactName != "" {
		sidecar.ContactName = smsmap.PIIRedactionToken
	}
	sidecarData, err := json.Marshal(sidecar)
	if err != nil {
		// 문자열·정수만 있는 구조체라 실제로는 일어나지 않는다.
		slog.Error("ingest_recording: marshal sidecar failed", "err_type", fmt.Sprintf("%T", err))
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if err := writeFileAtomic(destPath+".meta.json", sidecarData); err != nil {
		slog.Error("ingest_recording: write sidecar failed, returning 503",
			append([]any{"kind", kind}, recordingErrAttrs(err)...)...)
		writeIngestUnavailable(w, ingestUnavailableMsg)
		return
	}
	// audioBytes 는 위에서 검증했다. 메모리에서 디스크로 쓴다.
	if err := writeFileAtomic(destPath, audioBytes); err != nil {
		slog.Error("ingest_recording: write audio file failed, returning 503",
			append([]any{"kind", kind, "size_bytes", len(audioBytes)}, recordingErrAttrs(err)...)...)
		writeIngestUnavailable(w, ingestUnavailableMsg)
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
		// Security (issue #164), now conditional on s.piiNumberHashingEnabled:
		// when enabled AND the caller supplies no contact_name, never fall back
		// to the raw phone number for Title/Content — unlike SourceID, these are
		// plaintext user-facing fields. Fall back to a short, one-way hashed
		// label built from numHash (already computed above for the
		// SourceID/filename), matching the "number_hash" naming used elsewhere
		// (sidecar, SourceID). When hashing is disabled (default, policy
		// reversal), fall back to the raw number instead — that's the point of
		// disabling the flag.
		contact := contactName
		if contact == "" {
			if s.piiNumberHashingEnabled || s.piiNameRedactionEnabled {
				contact = "상대 " + numHash[:8]
			} else {
				contact = number
			}
		}
		title = fmt.Sprintf("incoming 통화 %s", contact)
		// content mirrors smsmap.MapCall's call-summary format exactly (no
		// "[TRANSCRIPTION PENDING]" placeholder) — see model.SourceCall's doc
		// comment: this document IS the call, recorded or not, so it must be
		// useful (searchable, readable) the instant it is created. When
		// WhisperCollector later transcribes the recording it replaces this
		// content via store.AttachTranscript; until then this 4-line summary
		// is what the user sees and what the FTS/vector indexes carry.
		recordedTime := recordedAt.Format("2006-01-02 15:04:05 MST")
		content = fmt.Sprintf("상대방: %s\n통화 방향: incoming\n시각: %s\n통화 시간: %ds",
			contact, recordedTime, durationSec)
		meta = map[string]any{
			"contact_name":     contactName,
			"direction":        "incoming",
			"recording_type":   "call",
			"duration_seconds": durationSec,
			"audio_file":       audioFilename,
			"transcription":    "pending",
		}
		// Additive (issue #164 policy reversal, not part of the original #164
		// behaviour): when hashing is disabled, also surface the raw number in
		// Metadata so it is searchable/visible in the UI — the point of turning
		// hashing off. Verified against prod: 0 of 1,759 existing call/sms
		// documents carry "number" or "number_hash" in Metadata today, so this
		// is a forward-only addition with no backfill implication.
		if !(s.piiNumberHashingEnabled || s.piiNameRedactionEnabled) {
			meta["number"] = number
		}
	}

	t := recordedAt
	doc := &model.Document{
		SourceType:  model.SourceCall,
		SourceID:    sourceID,
		Title:       title,
		Content:     content,
		Metadata:    meta,
		Status:      "active",
		OccurredAt:  &t,
		CollectedAt: time.Now().UTC(),
	}

	if s.piiNameRedactionEnabled {
		smsmap.RedactKnownContact(doc)
		if kind == "voice-memo" {
			doc.Title = "음성메모"
			doc.Content = fmt.Sprintf("녹음 시간: %ds\n[TRANSCRIPTION PENDING]", durationSec)
		}
	}
	if err := s.recordingUpserter.Upsert(r.Context(), doc); err != nil {
		// 오디오·사이드카는 이미 디스크에 있다. 분류는 ingest/messages 와 같은
		// classifyIngestErr 를 쓴다. source_id·경로는 로그에 남기지 않는다.
		class := classifyIngestErr(r.Context(), err)
		attrs := append([]any{"kind", kind, "class", class.String()}, ingestErrLogAttrs(err)...)
		switch class {
		case ingestErrDuplicate:
			// 영구 상태다: 같은 요약을 가진 다른 활성 통화 문서가 있는 한 다시
			// 보내도 결과가 같다. 오디오는 저장됐으므로 WhisperCollector 가
			// 전사해 병합한다 — 앱은 이 파일을 다시 보낼 필요가 없다. 예전의
			// 500 은 앱 녹음 루프를 이 파일에서 매번 멈춰 뒤의 녹음을 막았다.
			slog.Info("ingest_recording: duplicate call content, placeholder document skipped", attrs...)
			writeJSON(w, http.StatusOK, IngestRecordingResponse{
				Accepted: true,
				Skipped:  true,
				Reason:   recordingSkipReasonDuplicate,
			})
		case ingestErrPermanent:
			// DB 가 입력 때문에 결정적으로 거부했다(SQLSTATE 22·54, 23502·23514).
			// 다시 보내도 같으므로 5xx 는 poison 이 된다. 오디오는 저장돼 있다.
			slog.Warn("ingest_recording: document rejected permanently, returning 422", attrs...)
			writeError(w, http.StatusUnprocessableEntity,
				fmt.Sprintf("document rejected by database (sqlstate %s)", pgSQLState(err)))
		default:
			slog.Error("ingest_recording: transient upsert failure, returning 503", attrs...)
			writeIngestUnavailable(w, ingestUnavailableMsg)
		}
		return
	}

	writeJSON(w, http.StatusCreated, IngestRecordingResponse{
		Accepted:   true,
		Skipped:    false,
		DocumentID: doc.ID.String(),
	})
}

// recordingSidecar is the JSON structure written alongside each uploaded audio
// file as {audioPath}.meta.json. WhisperCollector reads this file and merges
// its fields into the call-transcript document's metadata, bridging the two
// independent pipelines without changing SourceIDs or the dedup logic.
//
// All fields are JSON-tagged with omitempty so that absent optional fields
// (e.g. contact_name for an anonymous call, direction for a voice-memo) are
// omitted from the sidecar rather than stored as empty strings.
//
// Security note (issue #164), now policy-flagged via cfg.PIINumberHashingEnabled:
// when the flag is true, the counterpart phone number is NOT stored in
// plaintext — NumberHash carries smsmap.ShortHash(number) instead, a one-way
// SHA-256-derived hash matching the hash already embedded in the call-log
// document's SourceID, and Number is left empty (omitted from the JSON).
// When the flag is false (the new default — see PIINumberHashingEnabled's doc
// comment in internal/config/config.go for the owner's rationale), Number
// carries the raw phone number instead and NumberHash is left empty. The
// handler (ingestRecordingHandler) only ever populates one of the two fields
// for a given upload, never both. WhisperCollector's sidecar reader
// (readRecordingSidecar in internal/collector/whisper.go) parses the "number"
// key and, when present, additively surfaces it as Metadata["number"] on the
// eventual call-transcript document — NumberHash is intentionally NOT
// surfaced there, since the whole point of the raw-number addition is
// searchability, which a hash does not provide.
type recordingSidecar struct {
	ContactName     string `json:"contact_name,omitempty"`
	Number          string `json:"number,omitempty"`
	NumberHash      string `json:"number_hash,omitempty"`
	Direction       string `json:"direction,omitempty"`
	RecordingType   string `json:"recording_type,omitempty"`
	DurationSeconds int    `json:"duration_seconds,omitempty"`
	DateMs          int64  `json:"date_ms,omitempty"`
	Kind            string `json:"kind,omitempty"`
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

// 저장 파일 이름 길이 한도(#292 리뷰). 파일 시스템의 이름 한도는 255바이트이고,
// writeFileAtomic 의 임시 이름은 원래 이름보다 약 20~30바이트 길다
// ("." + 이름 + ".{난수}.partial"), 사이드카는 여기에 ".meta.json" 이 더 붙는다.
// 이름이 길어 ENAMETOOLONG 이 나면 그 오류는 503(디스크 오류)이 되는데, 같은
// 파일은 매번 같은 이름이라 영원히 실패한다 — 앱 녹음 루프를 막는 poison 이다.
// 그래서 사용자 입력이 들어가는 부분마다 상한을 둔다. voice-memo 이름은
// "voice-memo_"(11) + 시각(14) + "_" + 줄기(≤100) + 확장자(≤6) ≈ 132바이트,
// 사이드카 임시 이름까지 더해도 180바이트 안쪽이다.
const (
	recordingNameMaxStemBytes     = 100
	recordingNumberPrefixMaxBytes = 32
)

// recordingDiskExts 는 저장 파일에 그대로 쓰는 확장자다(소문자 기준).
// WhisperCollector 의 whisperAudioExts(internal/collector/whisper.go)와 같은
// 집합이다 — 그 밖의 확장자는 예전에도 전사되지 않았다.
var recordingDiskExts = map[string]bool{
	".m4a": true, ".mp3": true, ".wav": true, ".aac": true, ".flac": true, ".ogg": true,
	".opus": true, ".webm": true, ".wma": true, ".aiff": true, ".mp4": true, ".oga": true,
}

// recordingUnknownExt 는 목록에 없는 확장자 대신 쓴다. 업로드 파일 이름의
// 확장자는 길이 제한이 없으므로 그대로 쓰면 이름 한도를 넘길 수 있다.
const recordingUnknownExt = ".audio"

// recordingDiskExt 는 업로드 파일 이름에서 저장 확장자를 고른다. 확장자가
// 없으면 예전처럼 ".m4a", 목록에 있으면 원래 표기(대소문자 포함) 그대로 —
// 이미 저장된 파일과 이름이 같아야 재업로드가 같은 경로를 덮어쓴다.
func recordingDiskExt(uploadedFilename string) string {
	e := filepath.Ext(uploadedFilename)
	switch {
	case e == "":
		return ".m4a"
	case recordingDiskExts[strings.ToLower(e)]:
		return e
	default:
		return recordingUnknownExt
	}
}

// boundedRecordingStem 은 voice-memo 저장 이름의 줄기를 상한 안으로 자른다.
// 비어 있으면 hash 를 쓴다. 자를 때는 앞부분 뒤에 hash(원래 파일 이름 전체의
// 해시)를 붙인다 — 앞 100바이트가 같은 서로 다른 파일이 같은 경로를 덮어써서
// 한쪽 오디오를 잃지 않게 한다. sanitizeFilename 의 결과는 ASCII 라 바이트로
// 잘라도 문자가 깨지지 않는다.
func boundedRecordingStem(sanitized, hash string) string {
	if sanitized == "" {
		return hash
	}
	if len(sanitized) <= recordingNameMaxStemBytes {
		return sanitized
	}
	keep := recordingNameMaxStemBytes - len(hash) - 1
	return strings.TrimRight(sanitized[:keep], "_-") + "_" + hash
}

// RecordingPartialMaxAge 는 시작 시 청소하는 임시 파일(.partial)의 최소 나이다.
// 요청 하나의 최대 수명(recordingWriteDeadline, 230초)보다 넉넉히 길다.
const RecordingPartialMaxAge = time.Hour

// CleanupStaleRecordingPartials 는 dir 에서 writeFileAtomic 이 남긴 오래된
// 임시 파일("." 로 시작하고 ".partial" 로 끝나는 일반 파일, now-olderThan
// 보다 오래됨)을 지운다. 정상 경로는 실패하면 스스로 지우므로, 남는 것은 쓰는
// 도중 프로세스가 죽은 경우뿐이다. 서버 시작 시 한 번 부른다. 지운 개수와 첫
// 오류를 돌려준다 — 오류 문구에는 경로(번호가 든 파일 이름)가 있으므로
// 호출자는 로그에 문구를 남기지 말 것. dir 이 없으면 (0, nil).
func CleanupStaleRecordingPartials(dir string, olderThan time.Duration, now time.Time) (removed int, err error) {
	if dir == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-olderThan)
	var first error
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".partial") {
			continue
		}
		info, infoErr := e.Info()
		if infoErr != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if rmErr := os.Remove(filepath.Join(dir, name)); rmErr != nil {
			if first == nil {
				first = rmErr
			}
			continue
		}
		removed++
	}
	return removed, first
}

// recordingBody 는 readErrRecorder 로 감싼 본문을 r.Body 로 다시 끼우기 위한
// io.ReadCloser 다. Close 는 원래 본문(MaxBytesReader)으로 보낸다.
type recordingBody struct {
	io.Reader
	io.Closer
}

// extendRecordingDeadlines 는 이 요청의 읽기·쓰기 기한을 늘린다
// (recordingReadDeadline 주석). 연결이 기한 조정을 지원하지 않으면(테스트의
// httptest.ResponseRecorder 등) 서버 기본값으로 계속한다.
func extendRecordingDeadlines(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	now := time.Now()
	if err := rc.SetReadDeadline(now.Add(recordingReadDeadline)); err != nil {
		slog.Debug("ingest_recording: read deadline not extended", "err_type", fmt.Sprintf("%T", err))
	}
	if err := rc.SetWriteDeadline(now.Add(recordingWriteDeadline)); err != nil {
		slog.Debug("ingest_recording: write deadline not extended", "err_type", fmt.Sprintf("%T", err))
	}
}

// recordingFormErrKind 는 multipart 파싱 실패의 분류다.
type recordingFormErrKind int

const (
	recordingFormMalformed recordingFormErrKind = iota
	recordingFormTooLarge
	recordingFormTransient
)

// recordingFormErrClass 는 ParseMultipartForm 오류를 분류한다. readErr 는
// readErrRecorder 가 기억한 본문 읽기 오류(깨끗한 EOF 제외)다.
//
//  1. 크기 초과(*http.MaxBytesError, multipart.ErrMessageTooLarge) → 413.
//     MaxBytesReader 의 오류도 리더를 거치므로 readErr 보다 먼저 본다.
//  2. 본문 읽기 오류가 있었다 → 일시(503). 업로드가 잘렸거나
//     (io.ErrUnexpectedEOF — Content-Length·chunked 본문이 도중에 끝남) 읽기
//     기한이 지났거나 연결이 끊긴 경우다. 다시 보내면 된다.
//  3. 임시 파일 오류(*fs.PathError, errno) → 일시(503). 32 MiB 를 넘는 파트는
//     임시 디렉터리에 내려 쓰는데, 디스크가 차거나(ENOSPC) 입출력 오류가 난
//     경우다. context 오류도 일시다.
//  4. 나머지 → 형식 오류(400). multipart 가 아님·경계 없음·파트 헤더 깨짐,
//     그리고 본문은 온전히 받았는데(깨끗한 EOF) multipart 가 끝나지 않은
//     경우다. 모두 클라이언트가 보낸 바이트 자체의 문제라 다시 보내도 같다.
//     표준 라이브러리 파서가 내는 오류는 위 셋 말고는 전부 이런 문구 오류라서,
//     ingest/messages 와 달리 모르는 오류의 기본값을 영구로 둔다 — 녹음은
//     일시 오류 하나가 앱 녹음 루프를 멈추므로 poison 의 비용이 더 크다.
func recordingFormErrClass(err, readErr error) recordingFormErrKind {
	var maxErr *http.MaxBytesError
	switch {
	case errors.As(err, &maxErr), errors.As(readErr, &maxErr),
		isMaxBytesError(err), errors.Is(err, multipart.ErrMessageTooLarge):
		return recordingFormTooLarge
	case readErr != nil:
		return recordingFormTransient
	case isDiskErr(err), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return recordingFormTransient
	default:
		return recordingFormMalformed
	}
}

// isDiskErr 는 파일 시스템 계층 오류인지 본다.
func isDiskErr(err error) bool {
	var (
		pathErr *fs.PathError
		linkErr *os.LinkError
		errno   syscall.Errno
	)
	return errors.As(err, &pathErr) || errors.As(err, &linkErr) || errors.As(err, &errno)
}

// recordingErrAttrs 는 녹음 경로의 디스크·전송 오류를 로그에 남길 때 쓰는
// 속성이다. 오류 문구는 남기지 않는다 — *fs.PathError·*os.LinkError 의 문구에는
// 저장 경로가 들어 있고, 통화 녹음의 저장 파일 이름은 기본 설정에서 전화번호다.
// 연산 이름(open·write·rename …)과 errno 문구(예: "no space left on device"),
// 시간 초과 여부만 남긴다.
func recordingErrAttrs(err error) []any {
	attrs := []any{"err_type", fmt.Sprintf("%T", err)}
	var (
		pathErr *fs.PathError
		linkErr *os.LinkError
		errno   syscall.Errno
		netErr  net.Error
	)
	switch {
	case errors.As(err, &pathErr):
		attrs = append(attrs, "op", pathErr.Op)
	case errors.As(err, &linkErr):
		attrs = append(attrs, "op", linkErr.Op)
	}
	if errors.As(err, &errno) {
		attrs = append(attrs, "errno", errno.Error())
	}
	if errors.As(err, &netErr) {
		attrs = append(attrs, "timeout", netErr.Timeout())
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		attrs = append(attrs, "truncated", true)
	}
	return attrs
}

// firstErr 는 nil 이 아닌 첫 오류다.
func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// writeFileAtomic 은 data 를 같은 디렉터리의 임시 파일(".partial" 로 끝남)에
// 쓰고 fsync 한 뒤 path 로 이름을 바꾼다. 실패하면 임시 파일을 지운다. 그래서
// path 에는 온전한 내용만 나타난다(같은 파일 시스템 안의 rename 은 원자적).
// 권한은 예전 os.WriteFile(…, 0o644) 과 같게 맞춘다.
func writeFileAtomic(path string, data []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.partial")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(0o644); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
