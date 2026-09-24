package search

import (
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/baekenough/second-brain/internal/model"
)

// MaxQueryBytes 는 외부 진입점(REST /api/v1/search, GraphQL search, MCP search
// 도구, 그래프 엔티티 검색)이 받는 검색 질의 문자열의 상한이다(#282).
//
// 바이트 기준인 이유: 질의가 만드는 비용 — pg_bigm 2-gram 수, LIKE 패턴 길이,
// tsquery 항 수, 임베딩 API 입력 토큰, 리랭커 요청 본문 — 은 전부 룬 수가
// 아니라 바이트 수에 비례한다. 룬 기준으로 두면 4바이트 문자만으로 같은 상한의
// 네 배 입력이 통과한다. 또 바이트 길이는 UTF-8 디코딩 없이 O(1) 로 먼저
// 거를 수 있어서, 거대한 입력을 끝까지 훑기 전에 거부할 수 있다.
//
// 1024 인 이유: 한글 기준 약 341자, ASCII 기준 1024자다. 검색 질의는 키워드나
// 짧은 문장이라 실사용 질의보다 한 자릿수 이상 크다. /api/v1/ask 의 질문
// 상한(internal/api 의 askQuestionBytes, 4KB)은 대화형 질문을 받는 별도
// 예산이므로 이 상수를 쓰지 않고 자기 상한으로 같은 검증 함수를 호출한다.
const MaxQueryBytes = 1024

// ErrInvalidInput 은 검색 입력 검증 실패 전체를 묶는 상위 오류다. 호출자는
// errors.Is(err, ErrInvalidInput) 하나로 "400 으로 돌려줄 입력 오류"를 판별한다.
var ErrInvalidInput = errors.New("invalid search input")

// 검증 실패 사유. 모두 ErrInvalidInput 과 함께 InputError 로 감싸 반환된다.
var (
	// ErrInputTooLong 은 입력이 허용 바이트 수를 넘었다는 뜻이다.
	ErrInputTooLong = errors.New("too long")
	// ErrInputInvalidUTF8 은 입력이 올바른 UTF-8 이 아니라는 뜻이다.
	// PostgreSQL 은 이런 text 파라미터를 SQLSTATE 22021 로 거부한다.
	ErrInputInvalidUTF8 = errors.New("not valid UTF-8")
	// ErrInputContainsNUL 은 입력에 NUL(0x00) 바이트가 있다는 뜻이다.
	// PostgreSQL text 는 NUL 을 담을 수 없어 역시 SQLSTATE 22021 로 끝난다.
	ErrInputContainsNUL = errors.New("contains NUL character")
)

// InputError 는 어느 입력 필드가 어떤 사유로 거부됐는지를 담는다.
//
// Error() 는 필드 이름과 고정 사유 문구만 조합하고 입력값 자체는 절대 넣지
// 않는다. 그래서 이 오류는 그대로 HTTP/GraphQL/MCP 응답 본문에 실어도, 로그에
// 남겨도 질의 원문(개인정보가 섞일 수 있음)을 노출하지 않는다.
type InputError struct {
	// Field 는 요청 스키마 쪽 필드 이름이다(예: "query", "source_type").
	Field string
	// Reason 은 ErrInputTooLong / ErrInputInvalidUTF8 / ErrInputContainsNUL 중 하나다.
	Reason error
	// MaxBytes 는 Reason 이 ErrInputTooLong 일 때 적용된 상한이다.
	MaxBytes int
}

func (e *InputError) Error() string {
	if errors.Is(e.Reason, ErrInputTooLong) {
		return e.Field + ": exceeds maximum length of " + strconv.Itoa(e.MaxBytes) + " bytes"
	}
	return e.Field + ": " + e.Reason.Error()
}

// Unwrap 은 상위 오류(ErrInvalidInput)와 구체 사유를 둘 다 노출해, 호출자가
// 어느 쪽으로든 errors.Is 판별을 할 수 있게 한다.
func (e *InputError) Unwrap() []error { return []error{ErrInvalidInput, e.Reason} }

// ValidateInputText 는 검색 경로로 들어가는 문자열 하나를 검사하는 공통
// 검증 함수다(#282). 모든 진입점이 이 함수 하나를 거친다.
//
// 검사 순서는 비용 순이다: 바이트 길이(O(1)) → UTF-8 유효성 → NUL 포함.
// maxBytes 가 0 이하이면 길이 검사를 건너뛴다 — 검색 서비스 내부의 방어선은
// 진입점마다 다른 길이 예산을 알 수 없어서 내용 검사만 한다.
//
// 이 함수는 입력을 고치지 않는다(NUL 제거·잘못된 바이트 치환 없음). 조용히
// 고쳐서 검색하면 사용자가 보낸 것과 다른 질의의 결과를 돌려주게 되므로,
// 거부하고 호출자에게 알리는 쪽을 택했다.
func ValidateInputText(field, s string, maxBytes int) error {
	if maxBytes > 0 && len(s) > maxBytes {
		return &InputError{Field: field, Reason: ErrInputTooLong, MaxBytes: maxBytes}
	}
	if !utf8.ValidString(s) {
		return &InputError{Field: field, Reason: ErrInputInvalidUTF8}
	}
	if strings.IndexByte(s, 0) >= 0 {
		return &InputError{Field: field, Reason: ErrInputContainsNUL}
	}
	return nil
}

// ValidateQueryInput 은 model.SearchQuery 에서 호출자가 채우는 문자열 필드
// 전부(질의 본문, 단수·복수 소스 필터, 제외 소스 목록, 정렬)를
// ValidateInputText 로 검사한다. 소스 필터도 SQL 파라미터로 바인딩되므로
// 질의 본문과 똑같이 22021 을 일으킬 수 있다.
//
// maxQueryBytes 는 질의 본문과 각 필터 값에 같이 적용된다. 필터 값은 수십
// 바이트짜리 식별자라 질의 상한보다 한참 짧으므로 따로 두지 않는다.
func ValidateQueryInput(q model.SearchQuery, maxQueryBytes int) error {
	if err := ValidateInputText("query", q.Query, maxQueryBytes); err != nil {
		return err
	}
	if q.SourceType != nil {
		if err := ValidateInputText("source_type", string(*q.SourceType), maxQueryBytes); err != nil {
			return err
		}
	}
	for _, st := range q.SourceTypes {
		if err := ValidateInputText("source_types", string(st), maxQueryBytes); err != nil {
			return err
		}
	}
	for _, st := range q.ExcludeSourceTypes {
		if err := ValidateInputText("exclude_source_types", string(st), maxQueryBytes); err != nil {
			return err
		}
	}
	return ValidateInputText("sort", q.Sort, maxQueryBytes)
}
