// Package httperr 는 외부 HTTP API(임베딩·리랭크·LLM 등)의 응답을 읽을 때
// 쓰는 상한 있는 읽기와 오류 정제 헬퍼를 모은다(#288 3항).
//
// 이 패키지가 막는 것은 두 가지다.
//
//  1. 외부 API 의 오류 본문이 오류 문자열로 새는 것. 임베딩·리랭크 API 는
//     요청 본문(사용자 질의, 문서 조각)을 오류 메시지에 되돌려 싣는 경우가
//     있다. 그 오류는 slog·OTel span 으로 퍼지므로, 본문 대신 상태 코드와
//     정제한 error.type/code 만 남긴다.
//  2. 응답 본문을 상한 없이 읽는 것. 고장 난(또는 가로챈) 응답이 메모리를
//     끝없이 쓰지 못하게 성공 본문에도 호출별 상한을 둔다.
//
// 기준 구현은 search.opensearchStatusError 다. 그 함수의 읽기 64KB·드레인
// 1MB 규칙을 그대로 일반화했다.
package httperr

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

const (
	// MaxErrorBodyBytes 는 비-2xx 응답 본문에서 error.type/code 를 뽑으려고
	// 읽는 최대 바이트다. 오류 본문은 정상이면 수백 바이트다.
	MaxErrorBodyBytes = 64 << 10

	// MaxDrainBytes 는 읽고 남은 본문을 버리며 읽는 상한이다. net/http 는
	// 본문을 끝까지 읽고 닫아야 keep-alive 연결을 다시 쓴다. 다만 끝없이
	// 읽으면 고장 난 서버의 거대한 본문에 시간을 쓰므로 상한을 두고, 넘으면
	// 그 연결만 버린다(다음 요청은 새 연결).
	MaxDrainBytes = 1 << 20
)

// ErrBodyTooLarge 는 성공 응답 본문이 호출자가 정한 상한을 넘었다는 뜻이다.
// 같은 요청을 다시 보내도 같은 크기가 돌아올 가능성이 높으므로 호출자는 이
// 오류를 재시도하지 않는다.
var ErrBodyTooLarge = errors.New("upstream response body exceeds limit")

// errorFieldRe 는 오류 문자열에 넣어도 되는 error.type/code 값의 모양이다.
//
// OpenAI(invalid_request_error, rate_limit_exceeded 등)와 OpenSearch
// (query_shard_exception 등)의 값은 소문자 식별자다. 숫자를 일부러 뺐다 —
// 숫자를 허용하면 프록시가 type 자리에 전화번호 같은 값을 되돌려 줄 때
// 그대로 통과한다. 모양이 맞지 않으면 필드를 버린다(상태 코드는 남는다).
//
// 이 모양은 로마자 이름(john)이나 이메일 로컬파트(kim.minsu) 같은 값도
// 통과시킨다. 그래도 받아들이는 이유: 이 필드는 공급자가 정한 고정 어휘
// (OpenAI 의 invalid_request_error·rate_limit_exceeded, OpenSearch 의 예외
// 이름)를 담는 자리이고, 요청 내용이 되돌아오는 자리는 message·reason 이다.
// 그 둘은 아예 읽지 않는다. 어휘 목록을 허용 목록으로 고정하면 공급자가 새
// 값을 추가할 때마다 진단 정보를 잃는다. 숫자를 막아 번호 모양만 차단하는
// 수준으로 둔다.
var errorFieldRe = regexp.MustCompile(`^[a-z][a-z_.-]{0,63}$`)

// StatusError 는 외부 API 의 비-2xx 응답을 나타낸다. 응답 본문은 담지 않는다.
//
// Error() 형식: "{Prefix} {StatusCode}" 에, Type·Code 가 있으면
// " (error.type=…, code=…)" 를 붙인다. Prefix 는 호출처가 쓰던 기존 문구
// ("embed API status" 등)를 그대로 넘겨 로그 검색·테스트가 계속 맞게 한다.
//
// 타입으로 두었으므로 호출자는 문자열이 아니라 errors.As 로 상태 코드를
// 판정할 수 있다.
type StatusError struct {
	Prefix     string
	StatusCode int
	Type       string
	Code       string
}

// Error 는 본문 없이 상태 코드와 정제한 필드만 담은 문자열을 돌려준다.
func (e *StatusError) Error() string {
	var b strings.Builder
	b.WriteString(e.Prefix)
	b.WriteByte(' ')
	b.WriteString(strconv.Itoa(e.StatusCode))
	var parts []string
	if e.Type != "" {
		parts = append(parts, "error.type="+e.Type)
	}
	if e.Code != "" {
		parts = append(parts, "code="+e.Code)
	}
	if len(parts) > 0 {
		b.WriteString(" (")
		b.WriteString(strings.Join(parts, ", "))
		b.WriteByte(')')
	}
	return b.String()
}

// ReadStatusError 는 비-2xx 응답 본문을 MaxErrorBodyBytes 까지 읽어
// error.type/code 만 뽑고, 남은 본문을 MaxDrainBytes 까지 버린 뒤
// StatusError 를 돌려준다. 본문은 닫지 않는다(호출자가 닫는다).
//
// 알아보는 본문 모양:
//   - OpenAI 모양: {"error":{"message":…,"type":…,"code":…}}
//   - OpenSearch 모양: {"error":{"type":…,"reason":…}}
//
// message·reason 은 요청 조각을 담을 수 있으므로 읽지 않는다. error 가
// 문자열인 모양(Ollama 등)이나 해석에 실패한 본문은 필드 없이 상태 코드만
// 남는다. 읽기 오류도 같은 결과다 — 판정은 상태 코드로 하므로 본문을 못
// 읽어도 호출자의 재시도 판단은 바뀌지 않는다.
func ReadStatusError(prefix string, resp *http.Response) *StatusError {
	se := &StatusError{Prefix: prefix, StatusCode: resp.StatusCode}
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBodyBytes))
	Drain(resp.Body)
	if err != nil {
		return se
	}
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(b, &envelope) != nil || len(envelope.Error) == 0 {
		return se
	}
	// code 는 문자열·숫자·null 이 모두 올 수 있어 RawMessage 로 받고,
	// 문자열일 때만 쓴다.
	var obj struct {
		Type json.RawMessage `json:"type"`
		Code json.RawMessage `json:"code"`
	}
	if json.Unmarshal(envelope.Error, &obj) != nil {
		return se
	}
	se.Type = safeField(obj.Type)
	se.Code = safeField(obj.Code)
	return se
}

// safeField 는 JSON 문자열 값이 errorFieldRe 모양일 때만 돌려준다.
func safeField(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil || !errorFieldRe.MatchString(s) {
		return ""
	}
	return s
}

// Drain 은 r 을 MaxDrainBytes 까지 읽어 버린다. 내용은 쓰지 않는다. 연결
// 재사용을 위한 것이므로 읽기 오류는 무시한다.
func Drain(r io.Reader) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r, MaxDrainBytes))
}

// ReadBody 는 성공 응답 본문을 max 바이트까지 읽는다. 본문이 max 를 넘으면
// 넘은 부분을 읽지 않고 ErrBodyTooLarge 를 돌려준다. 오류 문자열에는 본문이
// 들어가지 않는다.
//
// max 가 0 이하이면 호출자 실수이므로 상한 없이 읽지 않고 ErrBodyTooLarge 로
// 거부한다.
func ReadBody(r io.Reader, max int64) ([]byte, error) {
	if max <= 0 {
		return nil, fmt.Errorf("%w (limit=%d)", ErrBodyTooLarge, max)
	}
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%w (limit=%d)", ErrBodyTooLarge, max)
	}
	return b, nil
}
