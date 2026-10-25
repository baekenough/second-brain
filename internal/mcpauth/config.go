// Package mcpauth 는 MCP 서버(cmd/mcp)에 OAuth 2.1 인가를 붙이는 작은 구현이다.
//
// 범위(의도적으로 좁다):
//   - 혼합 모드: Authorization 헤더가 없는 initialize/tools/list 는 통과하고 도구 호출만
//     인증을 요구한다. 잘못된 토큰은 401.
//   - 사용자 한 명, 소유자 한 명. 동의 화면은 "소유자 키"(긴 무작위 비밀)로
//     본인을 확인한다. 계정·비밀번호는 없다.
//   - 클라이언트는 공개 클라이언트(auth method none)뿐이다: 미리 등록된 고정
//     client_id 하나, 그리고 허용 목록에 맞는 HTTPS URL 형태 client_id(CIMD, cimd.go).
//     조회는 ClientRegistry 뒤에 있어 DCR 같은 방식을 나중에 붙일 자리가 있다.
//   - 읽기 전용 스코프 brain:read 하나. 쓰기 도구는 OAuth 토큰을 거절한다.
//   - 인가 서버와 리소스 서버가 같은 프로세스라 액세스 토큰은 HS256 JWT 다
//     (jwt.go 의 근거 참고). 리프레시 토큰은 해시로 Postgres 에 저장한다.
//
// 이 패키지는 OAuth 가 켜졌을 때만 쓰인다. 환경 변수가 비어 있으면
// ConfigFromEnv 가 (nil, nil) 을 돌려주고 cmd/mcp 는 기존 동작 그대로다.
package mcpauth

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// 환경 변수 이름. 값이 아니라 이름만 오류 문구에 쓴다.
const (
	EnvPublicBaseURL    = "MCP_PUBLIC_BASE_URL"
	EnvResource         = "MCP_OAUTH_RESOURCE"
	EnvClientID         = "MCP_OAUTH_CLIENT_ID"
	EnvRedirectURIs     = "MCP_OAUTH_REDIRECT_URIS"
	EnvOwnerKeySHA256   = "MCP_OAUTH_OWNER_KEY_SHA256"
	EnvSigningKey       = "MCP_OAUTH_SIGNING_KEY"
	EnvTrustedProxies   = "MCP_OAUTH_TRUSTED_PROXIES"
	EnvCIMDPatterns     = "MCP_OAUTH_CIMD_CLIENT_ID_PATTERNS"
	EnvCIMDRedirectHost = "MCP_OAUTH_CIMD_REDIRECT_HOSTS"
	DefaultClientID     = "dots"
	DefaultResourcePath = "/mcp"
	// DefaultRedirectURI 는 ChatGPT 커넥터의 안정적인 공용 OAuth 콜백이다.
	DefaultRedirectURI = "https://chatgpt.com/connector_platform_oauth_redirect"
)

const (
	// ScopeRead 는 검색·조회 도구를 허용하는 유일한 공개 스코프다.
	ScopeRead = "brain:read"
	// ScopeWrite 는 add_note 같은 쓰기 도구가 요구하는 내부 스코프다.
	// scopes_supported 에 광고하지 않고 어떤 OAuth 토큰에도 발급하지 않는다.
	// 정적 API 키 주체만 모든 스코프를 가진다.
	ScopeWrite = "brain:write"
)

// 기본 수명. 값을 바꾸려면 Config 의 필드를 직접 채운다(테스트용).
const (
	DefaultAccessTokenTTL  = time.Hour
	DefaultRefreshTokenTTL = 30 * 24 * time.Hour
	DefaultCodeTTL         = 60 * time.Second
)

// MinSigningKeyBytes 는 서명 키의 최소 길이다(HS256 은 해시 출력 크기 이상 권장).
const MinSigningKeyBytes = 32

// Config 는 OAuth 설정이다. ConfigFromEnv 가 검증한 값만 담는다.
type Config struct {
	// Issuer 는 인가 서버 식별자(= 공개 기본 URL, 끝 슬래시 없음). PRM 의
	// authorization_servers 항목과 JWT iss 와 글자 그대로 같아야 한다.
	Issuer string
	// Resource 는 보호 리소스의 정규 URL(예: https://host/mcp). JWT aud 다.
	Resource string
	// ClientID 는 미리 등록된 공개 클라이언트의 ID 다.
	ClientID string
	// RedirectURIs 는 허용 목록이다. 문자열 완전 일치만 인정한다.
	RedirectURIs []string
	// OwnerKeySHA256 은 소유자 키의 SHA-256 이다. 키 자체는 저장하지 않는다.
	OwnerKeySHA256 [32]byte
	// SigningKey 는 JWT 서명과 동의 폼 토큰 파생 키의 원료다.
	SigningKey []byte
	// CIMDClientIDPatterns 는 Client ID Metadata Document 로 받아들일 client_id
	// URL 의 허용 패턴이다(예: https://chatgpt.com/oauth/{id}/client.json). 비어
	// 있으면 CIMD 는 꺼진다. 허용 목록 밖의 URL 은 가져오지도 않는다.
	CIMDClientIDPatterns []string
	// CIMDRedirectHosts 는 CIMD 문서의 redirect_uris 가 가질 수 있는 호스트다
	// (https 만).
	CIMDRedirectHosts []string
	// Warnings 는 기동을 막지는 않지만 운영자가 봐야 하는 설정 경고다(예: 서명 키의
	// 엔트로피를 확인할 수 없음). 비밀 값은 담지 않는다.
	Warnings []string
	// TrustedProxies 안에서 온 요청에만 CF-Connecting-IP 를 믿는다.
	TrustedProxies []netip.Prefix

	// AppName 은 동의 화면에 보이는 이름이다.
	AppName string

	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	CodeTTL         time.Duration
}

var clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,64}$`)

// defaultTrustedProxies 는 cloudflared 가 호스트에서 컨테이너로 들어올 때 보이는
// 출발지다. 호스트 루프백, 도커 기본 브리지 대역(172.16/12)만 넣는다. 10/8 이나
// 192.168/16 같은 LAN 대역은 넣지 않는다: 같은 LAN 의 다른 기기가 직접 8090 에
// 붙으면 그 출발지로 보이므로, 그 대역을 믿으면 CF-Connecting-IP 위조로 속도
// 제한을 비껴갈 수 있다. 필요하면 MCP_OAUTH_TRUSTED_PROXIES 로 명시한다.
var defaultTrustedProxies = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("172.16.0.0/12"),
}

// ConfigFromEnv 는 환경에서 OAuth 설정을 읽는다.
//
//   - 관련 변수가 하나도 없으면 (nil, nil): OAuth 꺼짐.
//   - 하나라도 있는데 필수 값이 빠지거나 값이 잘못됐으면 오류: 기동 중단.
//     "반쯤 켜진" 상태로 뜨면 인증이 조용히 약해질 수 있기 때문이다.
//
// 오류 문구에는 변수 이름만 담고 값(키·해시)은 담지 않는다.
func ConfigFromEnv(getenv func(string) string) (*Config, error) {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }

	all := []string{EnvPublicBaseURL, EnvResource, EnvClientID, EnvRedirectURIs,
		EnvOwnerKeySHA256, EnvSigningKey, EnvTrustedProxies, EnvCIMDPatterns, EnvCIMDRedirectHost}
	anySet := false
	for _, k := range all {
		if get(k) != "" {
			anySet = true
			break
		}
	}
	if !anySet {
		return nil, nil
	}

	var missing []string
	for _, k := range []string{EnvPublicBaseURL, EnvOwnerKeySHA256, EnvSigningKey} {
		if get(k) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("mcp oauth partially configured: %s must also be set (or unset all MCP_OAUTH_*/MCP_PUBLIC_BASE_URL to disable OAuth)",
			strings.Join(missing, ", "))
	}

	issuer, err := parseBaseURL(get(EnvPublicBaseURL))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvPublicBaseURL, err)
	}

	resource := issuer + DefaultResourcePath
	if v := get(EnvResource); v != "" {
		resource, err = parseResourceURL(v, issuer)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvResource, err)
		}
	}

	clientID := DefaultClientID
	if v := get(EnvClientID); v != "" {
		clientID = v
	}
	if !clientIDPattern.MatchString(clientID) {
		return nil, fmt.Errorf("%s: must match %s", EnvClientID, clientIDPattern)
	}

	// 미리 등록된 클라이언트의 콜백. 비우면 ChatGPT 의 안정적인 공용 콜백(서버가
	// RFC 9207 iss 응답을 지원할 때 쓰이는 주소)이 기본값이다. 값을 주면 그 목록으로
	// 대체된다 — 기본 콜백도 쓰려면 목록에 함께 적는다.
	redirectsRaw := get(EnvRedirectURIs)
	if redirectsRaw == "" {
		redirectsRaw = DefaultRedirectURI
	}
	redirects, err := parseRedirectURIs(redirectsRaw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvRedirectURIs, err)
	}

	raw, err := hex.DecodeString(get(EnvOwnerKeySHA256))
	if err != nil || len(raw) != 32 {
		return nil, fmt.Errorf("%s: must be a 64-character hex SHA-256 digest", EnvOwnerKeySHA256)
	}
	var ownerHash [32]byte
	copy(ownerHash[:], raw)

	// 서명 키는 앞뒤 공백을 자르지 않은 원문이 아니라 get() 으로 다듬은 값을 쓴다.
	// .env 파일 줄 끝 공백이 키에 섞여 들어가 환경마다 키가 달라지는 일을 막는다.
	signing := []byte(get(EnvSigningKey))
	if len(signing) < MinSigningKeyBytes {
		return nil, fmt.Errorf("%s: must be at least %d bytes (generate with: openssl rand -base64 48)",
			EnvSigningKey, MinSigningKeyBytes)
	}

	var warnings []string
	switch n, kind := signingKeyEntropyBytes(string(signing)); {
	case kind == "":
		warnings = append(warnings, EnvSigningKey+" is not hex or base64, so its entropy cannot be verified; generate it with: openssl rand -base64 48")
	case n < MinSigningKeyBytes:
		return nil, fmt.Errorf("%s: looks like %s and decodes to only %d bytes; need at least %d bytes of entropy (generate with: openssl rand -base64 48)",
			EnvSigningKey, kind, n, MinSigningKeyBytes)
	}

	proxies := defaultTrustedProxies
	if v := get(EnvTrustedProxies); v != "" {
		proxies, err = parseTrustedProxies(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvTrustedProxies, err)
		}
	}

	cimdPatterns, err := parseCIMDPatterns(get(EnvCIMDPatterns))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvCIMDPatterns, err)
	}
	cimdHosts, err := parseCIMDRedirectHosts(get(EnvCIMDRedirectHost))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvCIMDRedirectHost, err)
	}

	return &Config{
		CIMDClientIDPatterns: cimdPatterns,
		CIMDRedirectHosts:    cimdHosts,
		Issuer:               issuer,
		Resource:             resource,
		ClientID:             clientID,
		RedirectURIs:         redirects,
		OwnerKeySHA256:       ownerHash,
		SigningKey:           signing,
		TrustedProxies:       proxies,
		Warnings:             warnings,
		AppName:              "second-brain",
		AccessTokenTTL:       DefaultAccessTokenTTL,
		RefreshTokenTTL:      DefaultRefreshTokenTTL,
		CodeTTL:              DefaultCodeTTL,
	}, nil
}

// isLoopbackHost 는 개발용으로 http 를 허용할 호스트인지 본다.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return a.IsLoopback()
	}
	return false
}

// parseBaseURL 은 https://host[:port] 모양만 받아 끝 슬래시 없는 문자열로
// 돌려준다. 경로·쿼리·프래그먼트·사용자 정보는 거절한다: issuer 가 경로를
// 가지면 메타데이터 경로 규칙이 달라지는데, 이 서버는 그걸 쓰지 않는다.
func parseBaseURL(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil {
		return "", errors.New("not a valid URL")
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", errors.New("must be an absolute URL such as https://brain-mcp.example.com")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
		return "", errors.New("must use https (http is allowed only for localhost)")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery ||
		(u.Path != "" && u.Path != "/") {
		return "", errors.New("must be a bare origin without path, query, fragment or user info")
	}
	return u.Scheme + "://" + strings.ToLower(u.Host), nil
}

// parseResourceURL 은 MCP_OAUTH_RESOURCE 재정의를 검증한다. 같은 출처여야
// 한다: PRM 을 이 서버가 서빙하고 WWW-Authenticate 가 이 서버를 가리키므로
// 다른 호스트의 리소스를 선언해 봐야 클라이언트가 혼란스러울 뿐이다.
func parseResourceURL(s, issuer string) (string, error) {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", errors.New("must be an absolute URL")
	}
	if u.Scheme+"://"+strings.ToLower(u.Host) != issuer {
		return "", errors.New("must have the same origin as " + EnvPublicBaseURL)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", errors.New("must not contain query, fragment or user info")
	}
	if u.Path == "" || u.Path == "/" {
		return "", errors.New("must include the MCP path (e.g. /mcp)")
	}
	return s, nil
}

// parseRedirectURIs 는 쉼표로 구분된 허용 목록을 검증한다. 와일드카드나 패턴은
// 없다 — 비교는 항상 문자열 완전 일치다.
func parseRedirectURIs(s string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		u, err := url.Parse(part)
		if err != nil || u.Host == "" || u.Hostname() == "" {
			return nil, errors.New("every entry must be an absolute URL")
		}
		if u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
			return nil, errors.New("every entry must use https (http is allowed only for localhost)")
		}
		if u.Fragment != "" || u.User != nil || strings.Contains(part, "#") {
			return nil, errors.New("entries must not contain a fragment or user info")
		}
		if strings.ContainsAny(part, "*{}") {
			return nil, errors.New("wildcards or templates are not supported; paste the exact callback URL")
		}
		if !seen[part] {
			seen[part] = true
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("at least one redirect URI is required")
	}
	return out, nil
}

func parseTrustedProxies(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if p, err := netip.ParsePrefix(part); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(part)
		if err != nil {
			return nil, errors.New("entries must be IP addresses or CIDR prefixes")
		}
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	if len(out) == 0 {
		return nil, errors.New("no valid entries")
	}
	return out, nil
}

// 기본 CIMD client_id 패턴: ChatGPT 가 호스팅하는 클라이언트 메타데이터 문서.
// {id} 는 경로 세그먼트 하나(콜백 ID)에 대응한다.
var defaultCIMDPatterns = []string{
	"https://chatgpt.com/oauth/client.json",
	"https://chatgpt.com/oauth/{id}/client.json",
}

// parseCIMDPatterns 는 CIMD 허용 패턴을 검증한다. "off" 는 CIMD 를 끈다.
// 패턴은 https://host[:port]/path 이며 경로의 세그먼트 하나를 {id} 로 둘 수 있다.
func parseCIMDPatterns(s string) ([]string, error) {
	if s == "" {
		return append([]string(nil), defaultCIMDPatterns...), nil
	}
	if strings.EqualFold(s, "off") {
		return nil, nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		u, err := url.Parse(part)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
			u.RawQuery != "" || u.Fragment != "" || u.Path == "" || u.Path == "/" {
			return nil, errors.New("entries must be https://host/path URL patterns")
		}
		if strings.ContainsAny(u.Hostname(), "*{}") || strings.Contains(part, "*") {
			return nil, errors.New("wildcards are not supported (only a whole {id} path segment)")
		}
		for _, seg := range strings.Split(strings.Trim(u.Path, "/"), "/") {
			if strings.ContainsAny(seg, "{}") && seg != "{id}" {
				return nil, errors.New("{id} must be a whole path segment")
			}
			if seg == "" || seg == "." || seg == ".." {
				return nil, errors.New("invalid path segment")
			}
		}
		out = append(out, part)
	}
	if len(out) == 0 {
		return nil, errors.New("no valid entries (use \"off\" to disable)")
	}
	return out, nil
}

func parseCIMDRedirectHosts(s string) ([]string, error) {
	if s == "" {
		return []string{"chatgpt.com"}, nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		if strings.ContainsAny(part, "/*:@") || net.ParseIP(part) != nil {
			return nil, errors.New("entries must be plain DNS host names")
		}
		out = append(out, part)
	}
	if len(out) == 0 {
		return nil, errors.New("no valid entries")
	}
	return out, nil
}

// signingKeyEntropyBytes 는 서명 키가 hex 또는 base64(패딩 있는 표준 형식, openssl
// rand -base64/-hex 의 출력)처럼 보이면 디코딩한 바이트 수를 돌려준다. 문자열 길이가
// 충분해도 실제 엔트로피는 디코딩한 길이이기 때문이다(예: 32자 base64 는 24바이트).
// 어느 쪽도 아니면 kind 가 빈 문자열이다.
func signingKeyEntropyBytes(s string) (n int, kind string) {
	if len(s)%2 == 0 {
		if b, err := hex.DecodeString(s); err == nil {
			return len(b), "hex"
		}
	}
	if len(s)%4 == 0 {
		if b, err := base64.StdEncoding.Strict().DecodeString(s); err == nil {
			return len(b), "base64"
		}
	}
	return 0, ""
}
