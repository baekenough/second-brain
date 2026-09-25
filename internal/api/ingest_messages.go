package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/baekenough/second-brain/internal/chunker"
	"github.com/baekenough/second-brain/internal/collector/smsmap"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// defaultIngestMaxBatchMessages is the per-request record count cap used when
// WithIngestMessages receives a zero/negative maxBatchMessages argument.
const defaultIngestMaxBatchMessages = 5000

// ingest/messages 의 크기·시간 상한(#290, #288 1항).
const (
	// DefaultIngestMessagesMaxBodyBytes 는 요청 본문 상한의 기본값(32 MiB)이다.
	// 정상 최대치는 300건(앱 BATCH_SIZE) × (본문 64 KiB + 필드·JSON 약 0.3 KiB)
	// ≈ 19.8 MB 이고, 그 1.6배다. 운영 실측(2026-09-25, 활성 SMS 3,395건)은
	// 본문 최대 7,454 B·p99 2,018 B 라 실제 요청은 수십~수백 KB 다.
	// INGEST_MESSAGES_MAX_BODY_BYTES 로 바꾼다.
	DefaultIngestMessagesMaxBodyBytes int64 = 32 << 20

	// ingestSMSBodyMaxBytes 는 SMS 본문 하나의 상한이다. 앱은 content://sms 만
	// 읽는다(MMS 제외). 이어붙인 SMS 의 이론상 최대(255세그먼트: GSM 7bit
	// 약 39 KB, UCS-2 한글 UTF-8 약 51 KB)보다 크다.
	ingestSMSBodyMaxBytes = 64 << 10

	// ingestIdentityFieldMaxBytes 는 address·number·contact_name 의 상한이다.
	// 전화번호·영숫자 발신 ID(11자 이하)·이메일→SMS 주소(254자 이하)를 넉넉히
	// 덮는다.
	ingestIdentityFieldMaxBytes = 1 << 10

	// IngestMessagesBudget 은 요청 하나의 처리 예산이다. 게이트웨이·클라이언트
	// 타임아웃(HTTP WriteTimeout 90초 < Cloudflare 100초 < OkHttp read 120초)보다
	// 확실히 짧아야 "서버는 아직 처리 중인데 게이트웨이는 이미 502" 인 상태
	// (2026-06-21 사고)가 생기지 않는다. 임베딩을 요청 경로에서 뺐으므로 정상
	// 300건은 몇 초 안에 끝난다. 예산이 끝나면 다음 DB 호출이 곧바로 실패하고
	// 503 으로 끝난다(풀 연결을 무기한 기다리지 않는다).
	IngestMessagesBudget = 45 * time.Second

	// IngestMessagesGateWait 는 ingest 게이트(동시 1건)를 기다리는 상한이다.
	// 앞 요청이 막 끝나는 순간의 겹침만 흡수하고, 그보다 길게 겹치면 503 이다.
	IngestMessagesGateWait = 2 * time.Second

	// ingestMessagesRetryAfter 는 503 의 Retry-After(초)다. 앱은 이 헤더를 읽지
	// 않고 WorkManager 지수 백오프(1분부터)를 쓰지만, 다른 클라이언트와 명세를
	// 위해 붙인다.
	ingestMessagesRetryAfter = "30"

	// ingestMaxFutureSkew 는 date_ms 가 서버 시각보다 앞설 수 있는 상한이다.
	ingestMaxFutureSkew = 7 * 24 * time.Hour
)

// ingestMinDate 는 date_ms 의 하한이다. 그 이전 값은 폰 시계 오류나 잘못된
// 필드로 보고 레코드를 건너뛴다(DB 22008 에 기대지 않는다).
var ingestMinDate = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// IngestMessagesUpserter 는 ingest/messages 핸들러가 쓰는 문서 저장
// 인터페이스다. *store.DocumentStore 가 만족시킨다.
//
// UpsertTrackedWithChunks 는 doc 을 upsert 하고, 내용이 바뀐 경우에만 같은
// 트랜잭션 안에서 청크를 buildChunks(doc) 로 교체한다(#290). 청크 단계가
// 실패하면 문서 행도 되돌아가므로, 503 뒤 재전송은 "내용 변경 없음" 빠른
// 경로로 빠져 청크 없는 문서를 남기지 않고 두 단계를 모두 다시 한다.
//
// contentChanged=false 는 저장된 내용이 들어온 문서와 바이트 단위로 같아
// 청크 작업을 건너뛰었다는 뜻이다 — 재전송 배치를 싸게 유지하는 빠른 경로다
// (2026-06-21, 224건 재전송 사고). store.ErrDuplicateTranscript 는 그대로
// 또는 %w 로 감싸 돌려줘야 한다(핸들러가 errors.Is 로 가른다). 구현은
// 멱등이어야 한다: 같은 문서를 다시 보내도 저장 상태가 망가지면 안 된다.
type IngestMessagesUpserter interface {
	UpsertTrackedWithChunks(ctx context.Context, doc *model.Document, buildChunks func(*model.Document) []store.Chunk) (contentChanged bool, err error)
}

// WithIngestMessages attaches the dependencies required by
// POST /api/v1/ingest/messages and enables the route.
//
// maxBatchMessages caps the total number of SMS + call records accepted in a
// single request; 0 uses the package default (5000). Pass
// cfg.IngestMaxBatchMessages here.
//
// cutover is the optional floor time inherited from cfg.CollectorCutover:
// records whose OccurredAt is before this time are silently skipped.
// Zero time.Time{} disables the floor.
//
// 임베딩은 요청 경로에서 하지 않는다(결정 D2). 새 청크는 embedding IS NULL 로
// 저장되고, collector 의 청크 백필(scheduler.backfillChunkEmbeddings)과 문서
// 백필(backfillEmbeddings)이 다음 수집 주기에 채운다. FTS·bigm 검색은 즉시,
// 벡터 검색은 최대 수집 주기만큼 늦게 반영된다.
//
// Must be called before the first call to Handler().
func (s *Server) WithIngestMessages(
	upserter IngestMessagesUpserter,
	maxBatchMessages int,
	cutover time.Time,
) *Server {
	s.messagesUpserter = upserter
	if maxBatchMessages <= 0 {
		s.messagesMaxBatch = defaultIngestMaxBatchMessages
	} else {
		s.messagesMaxBatch = maxBatchMessages
	}
	s.messagesCutover = cutover
	if s.messagesMaxBody <= 0 {
		s.messagesMaxBody = DefaultIngestMessagesMaxBodyBytes
	}
	s.messagesBudget = IngestMessagesBudget
	s.messagesGate = semaphore.NewWeighted(1)
	s.messagesGateWait = IngestMessagesGateWait
	return s
}

// WithIngestMessagesMaxBody 는 ingest/messages 요청 본문 상한
// (INGEST_MESSAGES_MAX_BODY_BYTES)을 건다. 0 이하 값은 무시하고 기본값
// (32 MiB)을 쓴다 — 상한을 끄는 설정은 두지 않는다. WithIngestMessages 앞뒤
// 어느 쪽에서 불러도 된다. Handler() 전에 불러야 한다.
func (s *Server) WithIngestMessagesMaxBody(maxBytes int64) *Server {
	if maxBytes > 0 {
		s.messagesMaxBody = maxBytes
	}
	return s
}

// ingestSMSRecord is one element in the "sms" array of the request body.
type ingestSMSRecord struct {
	Address     string `json:"address"`
	Body        string `json:"body"`
	DateMs      int64  `json:"date_ms"`
	Type        int    `json:"type"`
	ContactName string `json:"contact_name"`
}

// ingestCallRecord is one element in the "calls" array of the request body.
type ingestCallRecord struct {
	Number      string `json:"number"`
	DateMs      int64  `json:"date_ms"`
	DurationSec int    `json:"duration_sec"`
	Type        int    `json:"type"`
	ContactName string `json:"contact_name"`
}

// IngestMessagesRequest 는 POST /api/v1/ingest/messages 가 받는 JSON 본문
// 형식이다(문서용 — 핸들러는 원소를 하나씩 따로 푼다, ingestMessagesEnvelope
// 참고).
type IngestMessagesRequest struct {
	SMS   []ingestSMSRecord  `json:"sms"`
	Calls []ingestCallRecord `json:"calls"`
}

// ingestMessagesEnvelope 는 핸들러가 실제로 디코딩하는 모양이다. 원소를
// json.RawMessage 로 받아 레코드마다 따로 푼다 — 원소 하나의 타입 오류
// (예: date_ms 가 문자열)가 배치 전체 400 이 되지 않고 그 레코드만 건너뛰게
// 하려는 것이다. 배열 자체가 배열이 아니면(구조 오류) 배치 400 이다.
type ingestMessagesEnvelope struct {
	SMS   []json.RawMessage `json:"sms"`
	Calls []json.RawMessage `json:"calls"`
}

// IngestMessagesResponse 는 201 Created 와 함께 돌려주는 본문이다.
//
// Sanitized 는 NUL(U+0000)을 지우고 저장한 레코드 수다(결정 D1: 실제 메시지를
// 버리지 않는다). PostgreSQL text·jsonb 는 NUL 을 받지 않는다. 앱은
// ignoreUnknownKeys=true 라 새 필드가 호환에 문제없다.
type IngestMessagesResponse struct {
	Accepted  int      `json:"accepted"`
	Skipped   int      `json:"skipped"`
	Sanitized int      `json:"sanitized"`
	Errors    []string `json:"errors"`
}

// 503 응답 문구(고정). 입력 값을 담지 않는다.
const (
	ingestUnavailableMsg = "ingest temporarily unavailable"
	ingestBusyMsg        = "ingest busy"
)

// ingestMessagesHandler handles POST /api/v1/ingest/messages.
//
// # 응답 계약(#290)
//
// 폰 앱(Uploader.kt)은 2xx 면 errors[] 를 보지 않고 커서를 배치 끝까지
// 전진하고, 5xx 면 커서를 멈추고 같은 배치를 다시 보내며, 401/403 이 아닌
// 4xx 도 재시도한다. 그래서 상태 코드가 곧 "이 배치를 다시 받아야 하는가" 다.
//
//	201 {accepted, skipped, sanitized, errors[]} — 성공
//	    배치를 끝까지 처리했다. 다시 보낼 필요가 없다. errors[] 는 영구 오류
//	    (필수 필드 누락·필드 상한 초과·date 범위 밖·원소 타입 오류·SQLSTATE
//	    22·54 클래스와 23502·23514)로 건너뛴 레코드다 — 다시 보내도 결과가
//	    같다. skipped 는 컷오버 이전 레코드와 통화 중복 전사다. sanitized 는
//	    저장에 성공한 레코드 가운데 NUL 을 지운 것의 수다.
//	503 + Retry-After: 30 — 다시 보내라
//	    일시 오류(DB 연결 끊김·풀 고갈·예산 45초 초과·클라이언트 이탈·
//	    분류되지 않은 오류) 또는 다른 ingest 요청이 처리 중(게이트). 첫 일시
//	    오류에서 멈추고 나머지 레코드는 처리하지 않는다. 앞서 저장된 레코드는
//	    재전송 때 "변경 없음" 빠른 경로를 탄다. 잘린 업로드도 503 이다.
//	413 본문이 상한(기본 32 MiB)을 넘거나 레코드 수가 상한(기본 5000)을 넘음.
//	    정상 앱(300건)은 도달할 수 없다. 앱에는 poison 이 되므로 slog.Error 로
//	    경보한다(결정 D4).
//	400 JSON 문법 오류·최상위 구조 오류·빈 본문. 앱 버그이며 서버가 구제할 수
//	    없다. 레코드 하나의 문제(잘못된 UTF-8·NUL·필드 과대·원소 타입 오류)로는
//	    절대 400 을 주지 않는다.
//	401 API 키 없음·틀림(미들웨어). 앱은 재시도를 멈춘다.
//
// # 처리 순서
//
//  0. ingest 게이트(동시 1건, 최대 IngestMessagesGateWait 대기) — 못 얻으면 503.
//  1. 요청 예산 ctx(IngestMessagesBudget).
//  2. 본문 디코딩(ingest 전용, decodeBoundedJSON 은 쓰지 않는다 — 그 함수는
//     본문 전체 UTF-8 이 틀리면 400 을 준다. encoding/json 은 잘못된 UTF-8 을
//     U+FFFD 로 바꾸므로 레코드를 보존할 수 있다).
//  3. 레코드마다(SMS → calls): 원소 디코딩 → NUL 제거 → 검증 → 매핑·컷오버 →
//     UpsertTrackedWithChunks(문서 upsert + 청크 교체 한 트랜잭션) → 오류
//     분류(classifyIngestErr).
//
// 로그·오류 문구에는 개인정보(본문·주소·번호·이름)를 넣지 않는다. 인덱스·개수·
// SQLSTATE·오류 범주만 남긴다.
func (s *Server) ingestMessagesHandler(w http.ResponseWriter, r *http.Request) {
	if s.messagesUpserter == nil {
		writeError(w, http.StatusServiceUnavailable, "message ingest not configured")
		return
	}

	release, ok := s.acquireIngestMessagesGate(r.Context())
	if !ok {
		slog.Warn("ingest_messages: another ingest request is in progress, rejecting with 503")
		writeIngestUnavailable(w, ingestBusyMsg)
		return
	}
	defer release()

	budgetCtx, cancel := context.WithTimeout(r.Context(), s.messagesBudget)
	defer cancel()

	env, ok := s.decodeIngestMessages(w, r)
	if !ok {
		return
	}

	total := len(env.SMS) + len(env.Calls)
	if total > s.messagesMaxBatch {
		slog.Error("ingest_messages: batch record count exceeds limit",
			"records", total, "limit", s.messagesMaxBatch)
		writeError(w, http.StatusRequestEntityTooLarge,
			"batch size exceeds maximum allowed records")
		return
	}

	res := newIngestMessagesResult()
	for i, raw := range env.SMS {
		rec, verr := s.prepareSMSRecord(i, raw, res)
		if !s.storeMessageRecord(budgetCtx, "sms", i, rec, verr, res) {
			writeIngestUnavailable(w, ingestUnavailableMsg)
			return
		}
	}
	for i, raw := range env.Calls {
		rec, verr := s.prepareCallRecord(i, raw, res)
		if !s.storeMessageRecord(budgetCtx, "call", i, rec, verr, res) {
			writeIngestUnavailable(w, ingestUnavailableMsg)
			return
		}
	}

	res.logSummary(total)
	writeJSON(w, http.StatusCreated, IngestMessagesResponse{
		Accepted:  res.accepted,
		Skipped:   res.skipped,
		Sanitized: res.sanitized,
		Errors:    res.errs,
	})
}

// ingestMessagesResult 는 한 요청의 누적 결과다.
//
// errorReasons·skipReasons 는 사유 코드별 개수다(로그 전용). 코드는 고정
// 문자열(예: missing_date_ms, db_sqlstate_23514)이라 개인정보가 없다. 운영
// 로그만 보고 "왜 건너뛰었는가" 를 가를 수 있게 하려는 것이다 — 특히 앱과
// 서버의 필드 이름이 어긋나면(계약 드리프트) 모든 레코드가 같은 사유로
// 거부되는데, 개수만 남기면 그 원인이 보이지 않는다.
type ingestMessagesResult struct {
	accepted     int
	skipped      int
	sanitized    int
	errs         []string
	errorReasons map[string]int
	skipReasons  map[string]int
}

func newIngestMessagesResult() *ingestMessagesResult {
	return &ingestMessagesResult{
		errs:         []string{},
		errorReasons: map[string]int{},
		skipReasons:  map[string]int{},
	}
}

// reject 는 레코드 하나를 영구 오류로 건너뛴 것을 적는다.
func (r *ingestMessagesResult) reject(verr *ingestValidationError) {
	r.errs = append(r.errs, verr.Error())
	r.errorReasons[verr.code]++
}

// skip 은 오류가 아닌 이유(컷오버·중복 전사)로 건너뛴 것을 적는다.
func (r *ingestMessagesResult) skip(reason string) {
	r.skipped++
	r.skipReasons[reason]++
}

// logSummary 는 201 로 끝난 배치의 건너뛴 사유를 남긴다. 저장된 레코드가
// 하나도 없는데 거부된 레코드가 있으면(= 건너뛰지 않은 레코드가 모두 거부됨)
// slog.Error 로 올린다. 한 배치의 모든 레코드가 거부되는 것은 입력 탓이
// 아니라 앱·서버 계약이 어긋났다는 신호일 가능성이 크고, 201 이라 폰은 커서를
// 전진하므로 그 레코드들은 다시 오지 않는다. 경보가 없으면 조용한 유실이 된다.
func (r *ingestMessagesResult) logSummary(total int) {
	if len(r.errs) == 0 && r.skipped == 0 {
		return
	}
	attrs := []any{
		"records", total,
		"accepted", r.accepted,
		"skipped", r.skipped,
		"errors", len(r.errs),
		"sanitized", r.sanitized,
		"error_reasons", r.errorReasons,
		"skip_reasons", r.skipReasons,
	}
	switch {
	case r.accepted == 0 && len(r.errs) > 0:
		slog.Error("ingest_messages: every non-skipped record was rejected — possible app/server contract drift", attrs...)
	case len(r.errs) > 0:
		slog.Warn("ingest_messages: records skipped with permanent errors", attrs...)
	default:
		slog.Info("ingest_messages: records skipped", attrs...)
	}
}

// 사유 코드(로그 키). 값은 고정이며 입력을 담지 않는다.
const (
	skipReasonCutover       = "before_cutover"
	skipReasonDupTranscript = "duplicate_transcript"
)

// preparedMessageRecord 는 검증·매핑을 마친 레코드다. doc 이 nil 이면 컷오버로
// 건너뛴 레코드다(이미 skipped 에 셌다). sanitized 는 NUL 을 지웠는지다 —
// 저장에 성공했을 때만 응답의 sanitized 에 더한다.
type preparedMessageRecord struct {
	doc       *model.Document
	sanitized bool
}

// acquireIngestMessagesGate 는 ingest 게이트를 최대 messagesGateWait 동안
// 기다린다. 폰은 1대라 정상일 때 동시 요청은 0~1건이다. 겹친다면 앞 요청이
// 아직 서버에서 도는 중에 재시도가 온 것이므로, 같은 배치를 두 번 처리하지 않게
// 503 으로 돌려보낸다(2026-06-21 사고의 구조).
//
// 검색 게이트(search_gate.go)와는 다른 세마포어이고 중첩해서 잡지 않는다.
// 게이트 덕분에 이 경로는 풀 연결을 동시에 최대 1개만 쓴다 — 검색 게이트가
// 남겨 두는 MaxConns−K 개 안에 든다. 게이트 대기는 풀 연결을 잡지 않는다.
func (s *Server) acquireIngestMessagesGate(ctx context.Context) (release func(), ok bool) {
	if s.messagesGate == nil {
		return func() {}, true
	}
	waitCtx, cancel := context.WithTimeout(ctx, s.messagesGateWait)
	defer cancel()
	if err := s.messagesGate.Acquire(waitCtx, 1); err != nil {
		return nil, false
	}
	var once sync.Once
	return func() { once.Do(func() { s.messagesGate.Release(1) }) }, true
}

// writeIngestUnavailable 은 503 + Retry-After 를 쓴다. 문구는 고정이다.
func writeIngestUnavailable(w http.ResponseWriter, msg string) {
	w.Header().Set("Retry-After", ingestMessagesRetryAfter)
	writeError(w, http.StatusServiceUnavailable, msg)
}

// readErrRecorder 는 본문을 읽다가 난 첫 오류(깨끗한 io.EOF 제외)를 기억한다.
// json.Decoder 는 "본문이 JSON 값 중간에서 끝났다" 를 io.ErrUnexpectedEOF 하나로
// 돌려주므로, 그것이 업로드가 잘린 것(전송 계층 오류 → 503, 재전송하면 해결)
// 인지 클라이언트가 보낸 JSON 자체가 잘린 것(깨끗한 EOF → 400)인지는 아래
// 리더가 실제로 돌려준 오류를 봐야 가를 수 있다.
type readErrRecorder struct {
	r   io.Reader
	err error
}

func (rr *readErrRecorder) Read(p []byte) (int, error) {
	n, err := rr.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && rr.err == nil {
		rr.err = err
	}
	return n, err
}

// decodeIngestMessages 는 본문을 스트리밍으로 디코딩한다(io.ReadAll 로 한 번 더
// 복사하지 않는다). 실패하면 응답을 쓰고 ok=false.
func (s *Server) decodeIngestMessages(w http.ResponseWriter, r *http.Request) (env ingestMessagesEnvelope, ok bool) {
	rec := &readErrRecorder{r: http.MaxBytesReader(w, r.Body, s.messagesMaxBody)}
	err := json.NewDecoder(rec).Decode(&env)
	if err == nil {
		return env, true
	}

	var mbe *http.MaxBytesError
	switch {
	case errors.As(rec.err, &mbe) || errors.As(err, &mbe):
		// 정상 앱은 도달할 수 없는 크기다. 앱에는 poison 이므로 경보한다(D4).
		slog.Error("ingest_messages: request body exceeds limit",
			"limit_bytes", s.messagesMaxBody)
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
	case rec.err != nil:
		// 본문을 다 받기 전에 전송이 끊겼다. 재전송하면 해결된다.
		slog.Warn("ingest_messages: request body read failed, returning 503",
			"err_type", fmt.Sprintf("%T", rec.err))
		writeIngestUnavailable(w, ingestUnavailableMsg)
	default:
		// 문법 오류·최상위 타입 오류·빈 본문·JSON 이 도중에 끝남(깨끗한 EOF).
		writeError(w, http.StatusBadRequest, "invalid request body")
	}
	return ingestMessagesEnvelope{}, false
}

// stripNUL 은 s 에서 NUL(U+0000)을 지운다. 지웠으면 changed=true.
// PostgreSQL text·jsonb 는 NUL 을 저장하지 못한다(22021·22P05). 결정적 변환이라
// 같은 입력은 언제나 같은 결과가 되고 재전송 멱등성도 유지된다.
func stripNUL(s string) (out string, changed bool) {
	if !strings.ContainsRune(s, 0) {
		return s, false
	}
	return strings.ReplaceAll(s, "\x00", ""), true
}

// validateIngestDate 는 date_ms 가 0 이 아니고 [2000-01-01, now+7일] 안에
// 있는지 본다. 사유 코드와 문구를 돌려주고, 문제없으면 둘 다 빈 문자열이다.
func validateIngestDate(dateMs int64, now time.Time) (code, reason string) {
	if dateMs == 0 {
		return "missing_date_ms", "missing date_ms"
	}
	if dateMs < ingestMinDate.UnixMilli() || dateMs > now.Add(ingestMaxFutureSkew).UnixMilli() {
		return "date_ms_out_of_range", "date_ms out of range"
	}
	return "", ""
}

// prepareSMSRecord 는 SMS 원소 하나를 풀고, NUL 을 지우고, 검증한 뒤 문서로
// 매핑한다. 거부하면 검증 오류를 돌려준다. 컷오버 이전이면 doc 이 nil 이고
// skipped 를 센다.
func (s *Server) prepareSMSRecord(i int, raw json.RawMessage, res *ingestMessagesResult) (preparedMessageRecord, *ingestValidationError) {
	invalid := func(code, reason string) (preparedMessageRecord, *ingestValidationError) {
		return preparedMessageRecord{}, &ingestValidationError{kind: "sms", index: i, code: code, reason: reason}
	}

	var rec ingestSMSRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return invalid("invalid_record", "invalid record")
	}

	var c1, c2, c3 bool
	rec.Address, c1 = stripNUL(rec.Address)
	rec.Body, c2 = stripNUL(rec.Body)
	rec.ContactName, c3 = stripNUL(rec.ContactName)

	switch {
	case rec.Address == "":
		return invalid("missing_address", "missing address")
	case len(rec.Address) > ingestIdentityFieldMaxBytes:
		return invalid("address_too_long", fmt.Sprintf("address exceeds %d bytes", ingestIdentityFieldMaxBytes))
	case len(rec.ContactName) > ingestIdentityFieldMaxBytes:
		return invalid("contact_name_too_long", fmt.Sprintf("contact_name exceeds %d bytes", ingestIdentityFieldMaxBytes))
	case len(rec.Body) > ingestSMSBodyMaxBytes:
		return invalid("body_too_long", fmt.Sprintf("body exceeds %d bytes", ingestSMSBodyMaxBytes))
	}
	if code, reason := validateIngestDate(rec.DateMs, time.Now()); code != "" {
		return invalid(code, reason)
	}

	doc := smsmap.MapSMS(rec.Address, rec.Body, rec.DateMs, rec.Type, rec.ContactName, s.piiNumberHashingEnabled, s.piiNameRedactionEnabled)
	if s.beforeMessagesCutover(&doc) {
		res.skip(skipReasonCutover)
		return preparedMessageRecord{}, nil
	}
	return preparedMessageRecord{doc: &doc, sanitized: c1 || c2 || c3}, nil
}

// prepareCallRecord 는 prepareSMSRecord 의 통화 판이다.
func (s *Server) prepareCallRecord(i int, raw json.RawMessage, res *ingestMessagesResult) (preparedMessageRecord, *ingestValidationError) {
	invalid := func(code, reason string) (preparedMessageRecord, *ingestValidationError) {
		return preparedMessageRecord{}, &ingestValidationError{kind: "call", index: i, code: code, reason: reason}
	}

	var rec ingestCallRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return invalid("invalid_record", "invalid record")
	}

	var c1, c2 bool
	rec.Number, c1 = stripNUL(rec.Number)
	rec.ContactName, c2 = stripNUL(rec.ContactName)

	switch {
	case rec.Number == "":
		return invalid("missing_number", "missing number")
	case len(rec.Number) > ingestIdentityFieldMaxBytes:
		return invalid("number_too_long", fmt.Sprintf("number exceeds %d bytes", ingestIdentityFieldMaxBytes))
	case len(rec.ContactName) > ingestIdentityFieldMaxBytes:
		return invalid("contact_name_too_long", fmt.Sprintf("contact_name exceeds %d bytes", ingestIdentityFieldMaxBytes))
	}
	if code, reason := validateIngestDate(rec.DateMs, time.Now()); code != "" {
		return invalid(code, reason)
	}

	doc := smsmap.MapCall(rec.Number, rec.DateMs, rec.DurationSec, rec.Type, rec.ContactName, s.piiNumberHashingEnabled, s.piiNameRedactionEnabled)
	if s.beforeMessagesCutover(&doc) {
		res.skip(skipReasonCutover)
		return preparedMessageRecord{}, nil
	}
	return preparedMessageRecord{doc: &doc, sanitized: c1 || c2}, nil
}

// beforeMessagesCutover 는 문서가 컷오버 이전인지 본다. 0 컷오버는 하한 없음.
func (s *Server) beforeMessagesCutover(doc *model.Document) bool {
	return !s.messagesCutover.IsZero() && doc.OccurredAt != nil && doc.OccurredAt.Before(s.messagesCutover)
}

// storeMessageRecord 는 준비된 레코드 하나를 저장하고 결과를 res 에 센다.
// 배치를 계속해도 되면 true, 일시 오류라 배치를 멈추고 503 을 줘야 하면 false.
//
// verr 는 prepare 단계의 검증 오류다(영구 → errors[]). rec.doc 과 verr 가 모두
// nil 이면 컷오버로 건너뛴 레코드다(이미 skipped 에 셌다).
//
// sanitized 는 저장에 성공한 레코드만 센다. 거부·중복·일시 오류 레코드까지
// 세면 errors[] 와 이중으로 잡혀 "NUL 을 지우고 저장한 수" 라는 뜻과 어긋나고,
// 503 으로 끝난 배치는 응답 본문이 없어 어차피 전달되지 않는다.
func (s *Server) storeMessageRecord(
	ctx context.Context,
	kind string,
	index int,
	rec preparedMessageRecord,
	verr *ingestValidationError,
	res *ingestMessagesResult,
) bool {
	if verr != nil {
		res.reject(verr)
		return true
	}
	doc := rec.doc
	if doc == nil {
		return true
	}

	_, err := s.messagesUpserter.UpsertTrackedWithChunks(ctx, doc, buildMessageChunks)
	if err == nil {
		res.accepted++
		if rec.sanitized {
			res.sanitized++
		}
		return true
	}

	class := classifyIngestErr(ctx, err)
	attrs := append([]any{"kind", kind, "record_index", index, "class", class.String()}, ingestErrLogAttrs(err)...)
	switch class {
	case ingestErrDuplicate:
		res.skip(skipReasonDupTranscript)
		slog.Info("ingest_messages: duplicate call transcript, skipped", attrs...)
		return true
	case ingestErrPermanent:
		state := pgSQLState(err)
		res.reject(&ingestValidationError{
			kind: kind, index: index,
			code:   "db_sqlstate_" + state,
			reason: fmt.Sprintf("rejected by database (sqlstate %s)", state),
		})
		slog.Warn("ingest_messages: record rejected permanently, skipped", attrs...)
		return true
	default:
		attrs = append(attrs, "accepted_before_abort", res.accepted)
		slog.Error("ingest_messages: transient failure, aborting batch with 503", attrs...)
		return false
	}
}

// buildMessageChunks 는 SMS·통화 문서의 청크를 만든다. store 가 upsert 와 같은
// 트랜잭션 안에서, 내용이 바뀐 경우에만 부른다(doc.ID 는 이미 채워져 있다).
// 임베딩은 하지 않는다 — 청크는 embedding IS NULL 로 저장되고 collector 의
// 청크 백필이 채운다(결정 D2, WithIngestMessages 주석).
func buildMessageChunks(doc *model.Document) []store.Chunk {
	texts := chunker.Split(doc.Content, chunker.SelectOptions(*doc))
	chunks := make([]store.Chunk, 0, len(texts))
	for i, t := range texts {
		chunks = append(chunks, store.Chunk{
			DocumentID: doc.ID,
			ChunkIndex: i,
			Content:    t,
			ByteSize:   len(t),
		})
	}
	return chunks
}
