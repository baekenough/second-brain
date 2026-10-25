package mcpauth

import (
	"context"
	"strings"
)

// PrincipalKind 는 요청이 어떤 자격으로 들어왔는지 나타낸다.
type PrincipalKind int

const (
	// PrincipalNone: 유효한 자격이 없다.
	PrincipalNone PrincipalKind = iota
	// PrincipalStatic: 정적 API_KEY Bearer(서버 운영용)이며 전권이다.
	PrincipalStatic
	// PrincipalOAuth: 이 서버가 발급한 액세스 토큰. 토큰의 스코프만 허용된다.
	PrincipalOAuth
)

// Principal 은 인증된 호출자다.
type Principal struct {
	Kind     PrincipalKind
	Scopes   []string
	ClientID string
	TokenID  string
}

// HasScope 는 호출자가 scope 를 가졌는지 본다. 정적 키 주체는 모든 스코프를
// 가진다(쓰기 포함). 자격이 없는 주체는 아무것도 갖지 않는다.
func (p Principal) HasScope(scope string) bool {
	switch p.Kind {
	case PrincipalStatic:
		return true
	case PrincipalOAuth:
		for _, s := range p.Scopes {
			if s == scope {
				return true
			}
		}
	}
	return false
}

type principalKey struct{}

// WithPrincipal 은 ctx 에 주체를 싣는다.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom 은 ctx 의 주체를 꺼낸다. 없으면 ok=false.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// splitScope 는 공백으로 구분된 scope 문자열을 나눈다.
func splitScope(s string) []string {
	return strings.Fields(s)
}

// grantedScope 는 요청된 scope 에서 이 서버가 발급할 수 있는 것만 남긴다.
//
// 결정(OIDC 스코프): openid·profile·email·offline_access 같은 알 수 없는
// 스코프는 거절하지 않고 조용히 버린다. ChatGPT 는 기본으로 OIDC 스코프를
// 함께 요청하는 것으로 알려져 있어 invalid_scope 로 거절하면 연결 자체가 깨진다.
// 대신 메타데이터는 brain:read 만 광고하고 OIDC 기능(id_token, userinfo)은
// 주장하지 않는다. 남은 것이 없으면(빈 요청 또는 알 수 없는 것뿐) brain:read 를
// 발급한다: 이 서버가 발급할 수 있는 유일한 스코프이고, 동의 화면이 실제 발급
// 스코프를 보여 주며, 토큰 응답의 scope 로 클라이언트에 알려 준다(RFC 6749 §3.3).
func grantedScope(requested string) string {
	want := map[string]bool{}
	for _, s := range splitScope(requested) {
		want[s] = true
	}
	var out []string
	for _, s := range supportedScopes {
		if want[s] {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return defaultScope
	}
	return strings.Join(out, " ")
}

// supportedScopes 는 scopes_supported 로 광고하고 발급할 수 있는 스코프다.
// ScopeWrite 는 일부러 없다.
var supportedScopes = []string{ScopeRead}

// defaultScope 는 요청에 쓸 만한 스코프가 없을 때 발급하는 값이다.
const defaultScope = ScopeRead

// Supports 는 이 서버가 OAuth 토큰에 발급할 수 있는 스코프인지 알려 준다. 도구가
// 요구하는 스코프가 여기에 없으면 재인가로는 해결되지 않는다(예: 쓰기 도구).
func Supports(scope string) bool {
	for _, s := range supportedScopes {
		if s == scope {
			return true
		}
	}
	return false
}
