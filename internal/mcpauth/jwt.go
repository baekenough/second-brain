package mcpauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 액세스 토큰은 HS256 JWT 다.
//
// 대칭 서명(HS256)을 쓰는 근거: 토큰을 발급하는 쪽(인가 서버)과 검증하는 쪽
// (리소스 서버)이 같은 프로세스이고 키가 밖으로 나가지 않는다. 비대칭 키가
// 막아 주는 것은 "검증자가 발급 권한을 갖는 것"인데, 여기서는 검증자가 곧
// 발급자다. 대신 공개키 배포(JWKS)·키 회전 구조가 필요 없다. 서명 키를 바꾸면
// 이미 나간 액세스 토큰이 즉시 모두 무효가 된다(운영 문서의 긴급 폐기 절차).
// 나중에 리소스 서버를 분리하면 EdDSA 로 바꾼다.
//
// 알고리즘 혼동 공격을 막으려고 검증은 헤더의 alg 를 "HS256" 과 글자 그대로
// 비교하고(none·RS256 등 거절), typ 도 "at+jwt"(RFC 9068) 여야 한다. 그래서
// 같은 키로 서명한 다른 용도의 JWT 도 통과하지 못한다.

const (
	jwtAlg = "HS256"
	jwtTyp = "at+jwt"

	// jwtMaxBytes 는 파싱 전에 거는 길이 상한이다. 우리가 발급하는 토큰은
	// 300바이트 안팎이다.
	jwtMaxBytes = 2048

	// jwtNotBeforeLeeway 는 nbf/iat 를 검사할 때 시계가 잠깐 뒤로 갔을 경우를
	// 흡수하는 여유다. exp 에는 여유를 주지 않는다.
	jwtNotBeforeLeeway = 5 * time.Second

	// SubjectOwner 는 모든 토큰의 sub 다. 소유자가 한 명뿐이다.
	SubjectOwner = "owner"
)

var jwtHeaderSegment = base64.RawURLEncoding.EncodeToString(
	[]byte(`{"alg":"` + jwtAlg + `","typ":"` + jwtTyp + `"}`))

// ErrInvalidToken 은 서명·형식·클레임 검증 실패 전반을 가리킨다. 호출자는
// 구체 사유를 응답에 싣지 않는다(oracle 방지). 로그에는 사유 문자열만 남긴다.
var ErrInvalidToken = errors.New("invalid access token")

// Claims 는 액세스 토큰 클레임이다.
type Claims struct {
	Issuer    string `json:"iss"`
	Audience  string `json:"aud"`
	Subject   string `json:"sub"`
	Scope     string `json:"scope"`
	ClientID  string `json:"client_id,omitempty"`
	IssuedAt  int64  `json:"iat"`
	NotBefore int64  `json:"nbf"`
	ExpiresAt int64  `json:"exp"`
	ID        string `json:"jti"`
}

// verifyClaims 는 aud 가 문자열이거나 배열일 수 있어 받는 쪽 모양이 다르다.
type verifyClaims struct {
	Issuer    string          `json:"iss"`
	Audience  json.RawMessage `json:"aud"`
	Subject   string          `json:"sub"`
	Scope     string          `json:"scope"`
	ClientID  string          `json:"client_id"`
	IssuedAt  int64           `json:"iat"`
	NotBefore int64           `json:"nbf"`
	ExpiresAt int64           `json:"exp"`
	ID        string          `json:"jti"`
}

// tokenSigner 는 JWT 를 서명·검증한다.
type tokenSigner struct {
	key      []byte
	issuer   string
	resource string
}

func (t *tokenSigner) sign(c Claims) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	signingInput := jwtHeaderSegment + "." + base64.RawURLEncoding.EncodeToString(payload)
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(t.mac(signingInput)), nil
}

func (t *tokenSigner) mac(signingInput string) []byte {
	h := hmac.New(sha256.New, t.key)
	h.Write([]byte(signingInput))
	return h.Sum(nil)
}

// verify 는 서명, 헤더, iss, aud, sub, exp/nbf/iat, jti 를 모두 확인한다.
// 스코프는 호출자가 도구별로 따로 확인한다.
func (t *tokenSigner) verify(token string, now time.Time) (*Claims, error) {
	if len(token) == 0 || len(token) > jwtMaxBytes {
		return nil, fmt.Errorf("%w: bad length", ErrInvalidToken)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: not a compact JWS", ErrInvalidToken)
	}
	enc := base64.RawURLEncoding.Strict()

	// 서명을 가장 먼저 본다. 서명이 맞기 전에는 헤더·본문을 해석하지 않는다.
	sig, err := enc.DecodeString(parts[2])
	if err != nil || len(sig) != sha256.Size {
		return nil, fmt.Errorf("%w: bad signature encoding", ErrInvalidToken)
	}
	if !hmac.Equal(sig, t.mac(parts[0]+"."+parts[1])) {
		return nil, fmt.Errorf("%w: signature mismatch", ErrInvalidToken)
	}

	rawHeader, err := enc.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: bad header encoding", ErrInvalidToken)
	}
	var header struct {
		Alg  string   `json:"alg"`
		Typ  string   `json:"typ"`
		Crit []string `json:"crit"`
	}
	if err := json.Unmarshal(rawHeader, &header); err != nil {
		return nil, fmt.Errorf("%w: bad header", ErrInvalidToken)
	}
	if header.Alg != jwtAlg || header.Typ != jwtTyp || len(header.Crit) > 0 {
		return nil, fmt.Errorf("%w: unexpected header", ErrInvalidToken)
	}

	rawClaims, err := enc.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: bad claims encoding", ErrInvalidToken)
	}
	var vc verifyClaims
	if err := json.Unmarshal(rawClaims, &vc); err != nil {
		return nil, fmt.Errorf("%w: bad claims", ErrInvalidToken)
	}

	if vc.Issuer != t.issuer {
		return nil, fmt.Errorf("%w: issuer mismatch", ErrInvalidToken)
	}
	if !audienceIsExactly(vc.Audience, t.resource) {
		return nil, fmt.Errorf("%w: audience mismatch", ErrInvalidToken)
	}
	if vc.Subject != SubjectOwner {
		return nil, fmt.Errorf("%w: subject mismatch", ErrInvalidToken)
	}
	if vc.ID == "" {
		return nil, fmt.Errorf("%w: missing jti", ErrInvalidToken)
	}
	if vc.ExpiresAt == 0 || vc.NotBefore == 0 || vc.IssuedAt == 0 {
		return nil, fmt.Errorf("%w: missing time claims", ErrInvalidToken)
	}
	if !now.Before(time.Unix(vc.ExpiresAt, 0)) {
		return nil, fmt.Errorf("%w: expired", ErrInvalidToken)
	}
	if now.Add(jwtNotBeforeLeeway).Before(time.Unix(vc.NotBefore, 0)) ||
		now.Add(jwtNotBeforeLeeway).Before(time.Unix(vc.IssuedAt, 0)) {
		return nil, fmt.Errorf("%w: not yet valid", ErrInvalidToken)
	}

	return &Claims{
		Issuer:    vc.Issuer,
		Audience:  t.resource,
		Subject:   vc.Subject,
		Scope:     vc.Scope,
		ClientID:  vc.ClientID,
		IssuedAt:  vc.IssuedAt,
		NotBefore: vc.NotBefore,
		ExpiresAt: vc.ExpiresAt,
		ID:        vc.ID,
	}, nil
}

// audienceIsExactly 는 aud 가 want 하나뿐인지 본다. 배열이면 원소가 정확히
// 하나이고 그것이 want 일 때만 인정한다: 다른 리소스용 토큰에 우리 리소스가
// 끼어 있는 경우도 혼동 위험이므로 받지 않는다.
func audienceIsExactly(raw json.RawMessage, want string) bool {
	if len(raw) == 0 {
		return false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s == want
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return len(arr) == 1 && arr[0] == want
	}
	return false
}
