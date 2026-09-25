package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"unicode/utf8"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
)

// searchRequestMaxBytes 는 POST /api/v1/search 요청 본문 상한이다(#282).
//
// 질의 본문은 search.MaxQueryBytes(1KB)로 따로 막지만, JSON 은 한 바이트를
// \u0000 형태 6바이트로 이스케이프할 수 있어 같은 질의가 본문에서는 최대 6KB
// 가 된다. 여기에 exclude_source_types 목록과 나머지 필드를 더해도 16KB 면
// 넉넉하다. 이 상한의 목적은 정상 요청을 자르는 것이 아니라 디코딩 전에
// 메모리 사용을 묶는 것이다. 초과하면 413 을 돌려준다(notes.go 와 같은 규약).
const searchRequestMaxBytes = 16 << 10

// graphqlRequestMaxBytes 는 /api/v1/graphql 요청 본문과 쿼리스트링 각각의
// 상한이다(#282). GraphQL 은 검색 외의 조회도 한 본문에 담을 수 있어 검색
// 본문보다 넉넉하게 64KB 로 둔다. 역시 디코딩 전 메모리 상한이 목적이다.
// 적용은 graphql_limits.go 의 guardGraphQL 이 한다.
const graphqlRequestMaxBytes = 64 << 10

// errSearchTimeout 은 검색 서비스 호출이 searchTimeout 안에 끝나지 않았다는
// 뜻이다. 핸들러는 이 오류를 504 로 바꾼다.
var errSearchTimeout = errors.New("search timed out")

// errBodyTooLarge / errBodyInvalidJSON 은 decodeBoundedJSON 의 실패
// 종류다. 오류 문구에는 본문 내용이 들어가지 않는다.
var (
	errBodyTooLarge    = errors.New("request body too large")
	errBodyInvalidJSON = errors.New("invalid JSON body")
)

// decodeBoundedJSON 은 POST 본문(/api/v1/search, /api/v1/ask, feedback 류
// #286)을 상한 안에서 읽어 dst 로 디코딩한다.
//
// json.Decoder 로 바로 읽지 않고 먼저 바이트로 읽는 이유: encoding/json 은
// 문자열 안의 잘못된 UTF-8 바이트를 U+FFFD 로 조용히 바꿔 버린다. 그러면
// 사용자가 보낸 것과 다른 질의로 검색하게 되므로, 디코딩 전에 본문 전체의
// UTF-8 유효성을 확인해 400 으로 거부한다(JSON 명세 RFC 8259 도 UTF-8 을
// 요구한다). "\u0000" 같은 이스케이프는 여기서 걸리지 않고 디코딩 뒤
// search.ValidateInputText 의 NUL 검사가 잡는다.
//
// 이 검사가 막는 것은 "원 바이트"가 잘못된 UTF-8 인 경우뿐이다. 짝 없는
// 서로게이트 이스케이프("\ud800")는 본문 자체가 유효한 UTF-8 이라 통과하고,
// encoding/json 이 디코딩하면서 U+FFFD 로 바꾼다(#286 항목 4a). 이것은 거부하지
// 않는다: 디코딩 뒤에는 사용자가 실제로 보낸 U+FFFD 와 구분할 수 없어 U+FFFD
// 를 거부하면 복사해 온 깨진 문자 같은 정상 입력까지 막게 되고, U+FFFD 는
// PostgreSQL text·jsonb 에 안전해 보안 영향이 없다. 원문 이스케이프를 직접
// 훑어 정확히 잡는 방법은 복잡도에 비해 얻는 것이 없다.
func decodeBoundedJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errBodyTooLarge
		}
		return errBodyInvalidJSON
	}
	if !utf8.Valid(body) {
		return &search.InputError{Field: "body", Reason: search.ErrInputInvalidUTF8}
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return errBodyInvalidJSON
	}
	return nil
}

// writeBoundedJSONError 는 decodeBoundedJSON 의 실패를 HTTP 응답으로 바꾼다.
// invalidMsg 는 JSON 해석 실패 때 쓸 문구다(핸들러마다 기존 문구를 유지하려고 받는다).
func writeBoundedJSONError(w http.ResponseWriter, err error, maxBytes int64, invalidMsg string) {
	switch {
	case errors.Is(err, errBodyTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("request body exceeds maximum size of %d bytes", maxBytes))
	case errors.Is(err, search.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusBadRequest, invalidMsg)
	}
}

// searchWithTimeout 은 검색 서비스 호출 하나에 s.searchTimeout 을 건다(#282).
//
// ctx 가 끝나면 pgx 는 호출자를 즉시 돌려보내고, 연결을 닫으면서 서버에
// 취소 요청을 보내 문장 자체도 멈춘다(pgx v5.9.2 기본 동작, 실DB 검증:
// internal/store 의 TestPool_ContextTimeoutCancelsServerSide). 그래서 풀 설정
// 변경이나 statement_timeout 없이 이 ctx 하나로 DB 작업까지 묶인다.
//
// 타임아웃 판정은 반환 오류가 아니라 이 함수가 만든 ctx 의 상태로 한다.
// 검색 서비스는 레인마다 오류를 다르게 감싸고(문자열화, 57014 PgError 등)
// 일부 레인 실패는 다른 오류로 이어지기도 해서, 오류 체인에 context 오류가
// 남는다는 보장이 없다. 체인만 보면 타임아웃을 500 으로 오분류할 수 있다.
func (s *Server) searchWithTimeout(parent context.Context, q model.SearchQuery) ([]*model.SearchResult, error) {
	ctx, cancel := context.WithTimeout(parent, s.searchTimeout)
	defer cancel()

	results, err := s.search.Search(ctx, q)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("%w: %w", errSearchTimeout, err)
	}
	return results, err
}

// writeSearchFailure 는 검색 서비스 오류를 HTTP 응답으로 바꾼다.
//
//   - 타임아웃 → 504. 서버가 스스로 끊은 것이지 클라이언트 입력이 틀린 것이
//     아니고, 재시도하면 성공할 수 있다는 신호(콜드 스타트 #195 등)가 된다.
//   - search.ErrInvalidInput → 400. 핸들러가 먼저 검사하므로 정상 경로에서는
//     오지 않지만, 서비스 내부 방어선이 잡은 경우에도 500 으로 새지 않게 한다.
//   - 그 밖 → 500.
//
// 응답 본문과 로그 어디에도 질의 원문을 넣지 않는다. 로그에는 오류와 설정된
// 타임아웃만 남는다(pgx 오류는 바인딩 파라미터 값을 담지 않는다).
func (s *Server) writeSearchFailure(w http.ResponseWriter, label string, err error) {
	switch {
	case errors.Is(err, errSearchTimeout):
		slog.Warn(label+": timed out", "timeout", s.searchTimeout.String(), "error", err)
		writeError(w, http.StatusGatewayTimeout, "search timed out")
	case errors.Is(err, search.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, searchInputMessage(err))
	default:
		slog.Error(label+": query failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}

// searchInputMessage 는 검증 오류에서 클라이언트에 보여 줄 문구를 꺼낸다.
// search.InputError 와 feedbackInputError(#286)의 문구는 필드 이름과 고정
// 사유만 담으므로 그대로 쓴다.
func searchInputMessage(err error) string {
	var ie *search.InputError
	if errors.As(err, &ie) {
		return ie.Error()
	}
	var fe *feedbackInputError
	if errors.As(err, &fe) {
		return fe.Error()
	}
	return "invalid search input"
}
