package api

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// feedback 류 진입점의 입력 상한(#286 항목 1).
//
// 대상은 다섯 경로다: POST /api/v1/feedback, POST /api/v1/feedback/evidence,
// GraphQL createFeedback, POST /api/v1/golden/judgments 와 /golden/feedback.
// 모두 사용자 입력을 DB 에 쓰는 경로라, 검색 입력(#282)과 같은 이유로
// 길이·UTF-8·NUL 을 DB 에 가기 전에 검사한다. 길이 상한이 없으면 요청 하나의
// 저장량이 본문 크기만큼 커지고, NUL·잘못된 UTF-8 은 PostgreSQL 이
// 22021(text)·22P05(jsonb) 로 거부해 500 이 된다.
const (
	// feedbackRequestMaxBytes 는 /api/v1/feedback 와 /feedback/evidence 본문
	// 상한이다. /ask 본문 상한(askRequestMaxBytes)과 같은 계산이다: 질문 4KB 가
	// JSON 이스케이프로 최대 6배(24KB)가 되고 나머지 필드를 더해 32KB.
	feedbackRequestMaxBytes = 32 << 10

	// feedbackQueryMaxBytes 는 feedback 의 query 상한이다. evidence 의 query 는
	// /ask 질문 그 자체라, ask 상한보다 작으면 답을 받은 질문에 투표할 수 없는
	// 회귀가 생긴다. 숫자를 복제하지 않고 같은 상수를 참조해 어긋날 수 없게 한다.
	feedbackQueryMaxBytes = askQuestionBytes

	// feedbackCommentMaxBytes 는 자유 입력 comment 상한이다. 리포 안 클라이언트는
	// 없고, 질의와 같은 예산을 준다.
	feedbackCommentMaxBytes = 4 << 10

	// feedbackSourceMaxBytes / feedbackIDMaxBytes 는 식별자류 필드 상한이다.
	// source 는 "search"·"api" 같은 짧은 값, session_id·user_id·conversation_id
	// 는 서버 발급 UUID(36바이트)나 외부 식별자라 128 이면 넉넉하다.
	feedbackSourceMaxBytes = 64
	feedbackIDMaxBytes     = 128

	// feedbackMetadataMaxBytes 는 metadata 를 직렬화한 뒤의 상한이다(jsonb
	// 저장량 상한). evidence 의 metadata 는 서버가 rank·layer 로 만든다.
	feedbackMetadataMaxBytes = 4 << 10

	// feedbackMetadataMaxDepth 는 metadata 중첩 깊이 상한이다(metadata 객체
	// 자신이 깊이 1). 4KB 안에서도 {"":{"":… 로 깊이 천 단위를 만들 수 있어
	// PostgreSQL jsonb 파서의 스택을 쓰게 하는 입력을 미리 막는다.
	feedbackMetadataMaxDepth = 16

	// goldenRequestMaxBytes 는 /api/v1/golden/judgments·/golden/feedback 본문
	// 상한이다. 판정 100개 × 약 110바이트 ≈ 11KB 에 query_text 4KB 의 이스케이프
	// 최악(24KB)을 더해도 64KB 안이다. hermes 가 대화 한 번에 보내는 판정 수 건은
	// 이보다 한참 작다.
	goldenRequestMaxBytes = 64 << 10

	// goldenJudgmentsMax 는 요청 하나의 판정 수 상한이다. 판정 하나가 한
	// 트랜잭션 안에서 SQL 1~3문장이 되므로(store.GoldenStore.UpsertJudgments)
	// 개수가 곧 요청당 DB 작업 배수다. 검토 UI 는 후보 10개(goldenNextDefaultLimit)
	// 를 한 번에 보낸다.
	goldenJudgmentsMax = 100

	// goldenQueryTextMaxBytes 는 golden/feedback 의 query_text 상한이다. golden
	// 질의는 /ask 질문에서 오므로 ask 질문 상한과 같다.
	goldenQueryTextMaxBytes = askQuestionBytes

	// goldenAskedAtMaxBytes 는 asked_at(RFC3339) 문자열 상한이다.
	goldenAskedAtMaxBytes = 64

	// goldenRankMax 는 판정의 rank(판정 시점 후보 순위, golden_judgments.
	// rank_at_judgment INT4) 상한이다. 후보 순위는 1 부터 golden/next 의
	// limit(기본 10)까지이고, hermes 는 rank 를 생략해 0 을 보낼 수 있어 하한은
	// 0 이다. 상한이 없으면 INT4 범위를 넘는 값이 22003(→500)이 되고, 범위 안의
	// 터무니없는 값은 순위 편향 분석을 오염시킨다. 10000 은 어떤 후보 목록
	// 크기보다도 넉넉한 값이다.
	goldenRankMax = 10000
)

// evidenceLayers 는 /api/v1/feedback/evidence 가 받는 layer 값이다. web BFF
// (web/src/app/api/feedback/evidence/route.ts 의 LAYERS)와 같은 집합이고, BFF 는
// 모르는 값을 ""로 바꿔 보낸다. 서버도 같은 집합으로 막아야 BFF 를 거치지 않는
// 호출이 metadata 에 임의 문자열을 쌓지 못한다.
var evidenceLayers = map[string]bool{"": true, "note": true, "observed": true, "insight": true}

// errMetadataTooDeep / errMetadataTooLarge 는 metadata 검증의 거부 사유다.
// 문구는 고정이며 입력값을 담지 않는다.
var (
	errMetadataTooDeep  = errors.New("nested too deeply")
	errMetadataTooLarge = errors.New("too large")
)

// feedbackInputError 는 search.InputError 로 표현할 수 없는 feedback 전용
// 거부 사유(UUID 형식, metadata 깊이·크기)다. Error() 는 필드 이름과 고정
// 문구만 담아 응답 본문·로그에 실어도 입력값이 새지 않는다. search.ErrInvalidInput
// 으로도 판별되게 해서 writeBoundedJSONError·searchInputMessage 를 그대로 쓴다.
type feedbackInputError struct {
	msg string
}

func (e *feedbackInputError) Error() string { return e.msg }
func (e *feedbackInputError) Unwrap() error { return search.ErrInvalidInput }

// validateFeedback 은 REST /api/v1/feedback 와 GraphQL createFeedback 이 함께
// 쓰는 검증이다. 검증을 한 곳에 두어 두 진입점이 어긋나지 않게 한다.
//
// GraphQL 경로는 본문 UTF-8 검사(decodeBoundedJSON)를 거치지 않으므로 여기가
// 그 경로의 유일한 UTF-8 방어선이다 — metadata 의 키까지 모두 검사한다.
//
// f 를 포인터로 받는 이유: document_id 를 정규형으로 바꿔 둔다(canonicalUUID
// 참고). 검증을 통과한 값과 DB 로 가는 값이 같아야 한다.
func validateFeedback(f *store.Feedback) error {
	checks := []struct {
		field string
		value *string
		max   int
	}{
		{"query", f.Query, feedbackQueryMaxBytes},
		{"comment", f.Comment, feedbackCommentMaxBytes},
		{"source", &f.Source, feedbackSourceMaxBytes},
		{"session_id", f.SessionID, feedbackIDMaxBytes},
		{"user_id", f.UserID, feedbackIDMaxBytes},
	}
	for _, c := range checks {
		if c.value == nil {
			continue
		}
		if err := search.ValidateInputText(c.field, *c.value, c.max); err != nil {
			return err
		}
	}
	// 형식이 틀린 UUID 는 DB 에서 22P02(→500)가 되므로 여기서 400 으로 거른다.
	if f.DocumentID != nil {
		id, ok := canonicalUUID(*f.DocumentID)
		if !ok {
			return &feedbackInputError{msg: "document_id must be a UUID"}
		}
		f.DocumentID = &id
	}
	return validateFeedbackMetadata(f.Metadata)
}

// validateFeedbackMetadata 는 metadata 의 모든 키와 문자열 값을 검사하고
// 깊이·직렬화 크기 상한을 건다. 길이는 직렬화 결과 하나로 묶으므로 개별 문자열
// 에는 내용 검사(UTF-8·NUL)만 한다. NUL 은 json.Marshal 이 \u0000 으로 내보내고
// jsonb 가 22P05 로 거부한다.
func validateFeedbackMetadata(meta map[string]any) error {
	if meta == nil {
		return nil
	}
	if err := validateJSONValue(meta, 1); err != nil {
		return err
	}
	// 재귀 검사를 먼저 해서 깊이 상한이 걸린 값만 직렬화한다. store 가 한 번 더
	// 직렬화하지만 4KB 이하라 비용은 무시할 수 있고, store 시그니처는 바꾸지 않는다.
	b, err := json.Marshal(meta)
	if err != nil {
		return &feedbackInputError{msg: "metadata: not serializable"}
	}
	if len(b) > feedbackMetadataMaxBytes {
		return &feedbackInputError{msg: "metadata: " + errMetadataTooLarge.Error() +
			" (max " + strconv.Itoa(feedbackMetadataMaxBytes) + " bytes serialized)"}
	}
	return nil
}

// validateJSONValue 는 JSON 으로 해석된 값 하나를 재귀로 검사한다. depth 는 v
// 가 컨테이너일 때의 깊이다(metadata 객체 자신이 1).
func validateJSONValue(v any, depth int) error {
	switch x := v.(type) {
	case string:
		return search.ValidateInputText("metadata", x, 0)
	case map[string]any:
		if depth > feedbackMetadataMaxDepth {
			return &feedbackInputError{msg: "metadata: " + errMetadataTooDeep.Error()}
		}
		for k, child := range x {
			if err := search.ValidateInputText("metadata", k, 0); err != nil {
				return err
			}
			if err := validateJSONValue(child, depth+1); err != nil {
				return err
			}
		}
	case []any:
		if depth > feedbackMetadataMaxDepth {
			return &feedbackInputError{msg: "metadata: " + errMetadataTooDeep.Error()}
		}
		for _, child := range x {
			if err := validateJSONValue(child, depth+1); err != nil {
				return err
			}
		}
	}
	// 숫자·불리언·null 은 검사할 것이 없다.
	return nil
}

// canonicalUUID 는 s 를 UUID 로 해석해 정규형(소문자, 하이픈 36자)으로 돌려준다.
//
// uuid.Parse 는 표준형 말고도 "urn:uuid:" 접두사, 중괄호, 하이픈 없는 32자,
// 대문자를 받아들인다. 그런데 PostgreSQL uuid 입력은 urn 형식을 22P02 로
// 거부한다. 검사만 하고 원문을 그대로 넘기면 검증을 통과한 값이 DB 에서 500 이
// 되고, PG 오류 문구에 원문이 담겨 slog.Error 로 남는다. 그래서 거부 대신
// 정규화를 택했다: uuid.Parse 가 받아들인 형식은 모두 같은 128비트 값을
// 뜻하므로 정규형으로 바꿔 저장하면 의미가 보존되고, DB 에는 언제나 PG 가
// 받는 형식만 간다(#286 deep-verify).
func canonicalUUID(s string) (string, bool) {
	id, err := uuid.Parse(s)
	if err != nil {
		return "", false
	}
	return id.String(), true
}

// isForeignKeyViolation 은 err 가 PostgreSQL FK 위반(23503)인지 본다.
// feedback 의 document_id·chunk_id 는 documents·chunks 를 참조하므로 존재하지
// 않는 ID 는 이 오류가 된다. 클라이언트 입력 문제이므로 500 이 아니라 400 이다.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
