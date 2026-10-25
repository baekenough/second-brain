package mcpauth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Client ID Metadata Document (CIMD) 지원.
//
// client_id 가 HTTPS URL 이면 그 URL 의 JSON 문서가 클라이언트 메타데이터다.
// OpenAI 가 권장하는 등록 방식이라 사용자가 콜백 URL 을 붙여 넣지 않아도 된다.
//
// 이 서버는 인증 없는 요청이 임의 URL 을 가져오게 하는 셈이므로(SSRF 표면) 아래를
// 모두 지킨다.
//   - client_id URL 은 허용 패턴(기본 chatgpt.com 의 /oauth/client.json,
//     /oauth/{id}/client.json)에 글자 그대로 맞아야 한다. 패턴 밖은 가져오지도 않는다.
//   - 연결 시점에 실제 접속하는 IP 가 공개 주소인지 검사한다(DialContext 의 Control).
//     DNS 재바인딩과 사설·루프백·링크로컬·메타데이터 주소를 막는다. 프록시는 쓰지 않는다.
//   - HTTPS 만, 리다이렉트 불가, 짧은 타임아웃, 본문 크기 상한, JSON 콘텐츠 타입.
//   - 문서의 client_id 는 URL 과 같아야 하고, redirect_uris 는 모두 허용 호스트의
//     https 여야 하며, 인증 방식은 none 이 가능해야 한다(client_secret 거절. JWKS 등
//     private_key_jwt 필드는 쓰지 않으므로 무시).
//   - 결과는 캐시한다(성공 10분, 실패 1분, 오류 시 24시간까지 이전 성공값 재사용).

const (
	cimdFetchTimeout   = 5 * time.Second
	cimdMaxDocBytes    = 16 << 10
	cimdCacheTTL       = 10 * time.Minute
	cimdNegativeTTL    = time.Minute
	cimdStaleTTL       = 24 * time.Hour
	cimdMaxRedirects   = 10
	cimdMaxClientIDLen = 256
	cimdMaxInflight    = 4
	cimdCacheMax       = 256

	// 가져오기 속도 제한. 허용 패턴이 {id} 를 받으므로 인증 없는 요청이 서로 다른 client_id 로
	// 외부 호출을 일으킬 수 있다. 그 양을 IP 별·전체로 묶는다.
	cimdFetchWindow   = time.Minute
	cimdFetchesPerIP  = 6
	cimdFetchesGlobal = 30
)

var cimdIDSegment = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)

// cimdFetchFunc 은 URL 의 문서 본문을 가져온다. 기본 구현은 secureFetcher 다.
type cimdFetchFunc func(ctx context.Context, rawURL string) ([]byte, error)

type cimdPattern struct {
	host string // 포트 포함, 소문자
	segs []string
}

type cimdEntry struct {
	client    Client
	ok        bool
	expires   time.Time
	staleTill time.Time
}

type cimdRegistry struct {
	patterns      []cimdPattern
	redirectHosts map[string]bool
	fetch         cimdFetchFunc
	now           func() time.Time
	log           *slog.Logger

	mu      sync.Mutex
	cache   map[string]*cimdEntry
	sem     chan struct{}
	limiter *attemptLimiter // 가져오기 속도 제한(IP 별·전체)
}

func newCIMDRegistry(patterns, redirectHosts []string, fetch cimdFetchFunc, now func() time.Time, log *slog.Logger) (*cimdRegistry, error) {
	r := &cimdRegistry{
		redirectHosts: map[string]bool{},
		fetch:         fetch,
		now:           now,
		log:           log,
		cache:         map[string]*cimdEntry{},
		sem:           make(chan struct{}, cimdMaxInflight),
		limiter:       newAttemptLimiter(now, cimdFetchWindow, cimdFetchesPerIP, cimdFetchesGlobal),
	}
	for _, p := range patterns {
		u, err := url.Parse(p)
		if err != nil {
			return nil, fmt.Errorf("cimd pattern: %w", err)
		}
		r.patterns = append(r.patterns, cimdPattern{
			host: strings.ToLower(u.Host),
			segs: strings.Split(strings.Trim(u.Path, "/"), "/"),
		})
	}
	for _, h := range redirectHosts {
		r.redirectHosts[strings.ToLower(h)] = true
	}
	return r, nil
}

// looksLikeURL 은 client_id 가 CIMD 후보 모양인지 본다(정적 클라이언트와 구분).
func looksLikeURL(id string) bool { return strings.HasPrefix(id, "https://") }

// matches 는 client_id 가 허용 패턴과 정확히 맞는지 본다.
func (r *cimdRegistry) matches(id string) bool {
	if len(id) == 0 || len(id) > cimdMaxClientIDLen {
		return false
	}
	u, err := url.Parse(id)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || u.RawPath != "" || u.Opaque != "" {
		return false
	}
	if u.Path == "" || u.Path != path.Clean(u.Path) || strings.HasSuffix(u.Path, "/") {
		return false
	}
	host := strings.ToLower(u.Host)
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	for _, p := range r.patterns {
		if p.host != host || len(p.segs) != len(segs) {
			continue
		}
		ok := true
		for i, ps := range p.segs {
			switch {
			case ps == "{id}":
				ok = ok && cimdIDSegment.MatchString(segs[i]) && segs[i] != "." && segs[i] != ".."
			default:
				ok = ok && ps == segs[i]
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// Lookup 은 ClientRegistry 를 구현한다. 가져오기에 실패하면(오류·속도 제한·동시
// 가져오기 상한) 마지막 성공값을 24시간까지 대신 쓴다.
func (r *cimdRegistry) Lookup(ctx context.Context, id string) (Client, bool) {
	if !r.matches(id) {
		return Client{}, false
	}
	now := r.now()
	r.mu.Lock()
	e := r.cache[id]
	if e != nil && now.Before(e.expires) {
		c, ok := e.client, e.ok
		r.mu.Unlock()
		return c, ok
	}
	r.mu.Unlock()

	staleOK := func() (Client, bool) {
		if e != nil && e.ok && now.Before(e.staleTill) {
			return e.client, true
		}
		return Client{}, false
	}

	// 속도 제한에 걸리면 가져오지 않고, 부정 결과를 캐시하지도 않는다(제한이 풀리면
	// 정상 클라이언트가 곧바로 쓸 수 있어야 한다).
	ip, _ := ctx.Value(clientIPKey{}).(string)
	if ip == "" {
		ip = "unknown"
	}
	if _, ok, _ := r.limiter.reserve(ip); !ok {
		return staleOK()
	}

	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	default:
		return staleOK() // 동시 가져오기 상한: 새로 가져오지 않는다
	}

	body, err := r.fetch(ctx, id)
	var client Client
	if err == nil {
		client, err = parseCIMDDocument(id, body, r.redirectHosts)
	}
	if err != nil {
		r.log.Warn("mcp oauth: client metadata document rejected", "client_id", id, "reason", err.Error())
		if c, ok := staleOK(); ok {
			// 이전 성공값을 유지하되 잠시 쉬었다가 다시 가져온다.
			r.store(id, &cimdEntry{client: c, ok: true, expires: now.Add(cimdNegativeTTL), staleTill: e.staleTill}, now)
			return c, true
		}
		r.store(id, &cimdEntry{expires: now.Add(cimdNegativeTTL)}, now)
		return Client{}, false
	}
	r.store(id, &cimdEntry{client: client, ok: true, expires: now.Add(cimdCacheTTL), staleTill: now.Add(cimdCacheTTL + cimdStaleTTL)}, now)
	return client, true
}

// store 는 캐시에 넣는다. 캐시가 가득 차면 쓸모없는 항목(부정 결과, 완전히 낡은 항목)을
// 먼저 지운다. 부정 결과가 성공 항목을 밀어내지 못하게 하고, 성공 항목끼리는 만료가
// 가장 이른 것을 내보낸다. 그래서 무작위 client_id 요청으로 정상 클라이언트의 캐시를
// 오염시킬 수 없다.
func (r *cimdRegistry) store(id string, e *cimdEntry, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.cache[id]; !exists && len(r.cache) >= cimdCacheMax {
		for k, old := range r.cache {
			if !old.ok || now.After(old.staleTill) {
				delete(r.cache, k)
			}
		}
		if len(r.cache) >= cimdCacheMax {
			if !e.ok {
				return // 부정 결과는 성공 항목을 밀어내지 않는다
			}
			var oldest string
			var oldestExp time.Time
			for k, old := range r.cache {
				if oldest == "" || old.expires.Before(oldestExp) {
					oldest, oldestExp = k, old.expires
				}
			}
			delete(r.cache, oldest)
		}
	}
	r.cache[id] = e
}

// cimdDocument 는 문서에서 읽는 필드다. 모르는 필드는 무시한다.
type cimdDocument struct {
	ClientID                string          `json:"client_id"`
	ClientName              string          `json:"client_name"`
	RedirectURIs            []string        `json:"redirect_uris"`
	TokenEndpointAuthMethod *string         `json:"token_endpoint_auth_method"`
	AuthMethodsSupported    []string        `json:"token_endpoint_auth_methods_supported"`
	GrantTypes              []string        `json:"grant_types"`
	ResponseTypes           []string        `json:"response_types"`
	ClientSecret            json.RawMessage `json:"client_secret"`
}

func cimdAllowsNone(d cimdDocument) bool {
	for _, m := range d.AuthMethodsSupported {
		if m == "none" {
			return true
		}
	}
	return d.TokenEndpointAuthMethod == nil || *d.TokenEndpointAuthMethod == "none"
}

func present(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

// parseCIMDDocument 는 문서를 검증해 Client 로 만든다.
func parseCIMDDocument(id string, body []byte, redirectHosts map[string]bool) (Client, error) {
	var d cimdDocument
	if err := json.Unmarshal(body, &d); err != nil {
		return Client{}, errors.New("not valid JSON")
	}
	if d.ClientID != id {
		return Client{}, errors.New("client_id does not equal the document URL")
	}
	// ChatGPT 의 실제 문서는 token_endpoint_auth_method=private_key_jwt 이면서
	// token_endpoint_auth_methods_supported=["none","private_key_jwt"] 를 함께 싣는다.
	// ChatGPT 는 자기 목록과 서버가 광고한 방식(우리는 none 뿐)의 교집합에서 고르고, 단수
	// 필드는 그것이 교집합 안일 때만 쓴다. 그러므로 none 이 목록에 있거나 단수 필드가
	// none/생략이면 받아들인다. jwks·jwks_uri·token_endpoint_auth_signing_alg 는
	// private_key_jwt 전용이라 쓰지 않으므로 무시한다. 공유 비밀(client_secret)은 공개
	// 클라이언트 모델과 맞지 않아 여전히 거절한다.
	if !cimdAllowsNone(d) {
		return Client{}, errors.New("token_endpoint_auth_method none is not offered")
	}
	if present(d.ClientSecret) {
		return Client{}, errors.New("client_secret is not allowed (public client only)")
	}
	for _, g := range d.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			return Client{}, errors.New("unsupported grant_types")
		}
	}
	for _, rt := range d.ResponseTypes {
		if rt != "code" {
			return Client{}, errors.New("unsupported response_types")
		}
	}
	if len(d.RedirectURIs) == 0 || len(d.RedirectURIs) > cimdMaxRedirects {
		return Client{}, errors.New("redirect_uris must have 1..10 entries")
	}
	var uris []string
	for _, ru := range d.RedirectURIs {
		u, err := url.Parse(ru)
		if err != nil || len(ru) > 512 || u.Scheme != "https" || u.User != nil || u.Fragment != "" ||
			u.Hostname() == "" || u.Port() != "" || strings.Contains(ru, "#") || strings.ContainsAny(ru, "*{}") ||
			!redirectHosts[strings.ToLower(u.Hostname())] {
			return Client{}, errors.New("redirect_uris contains an entry outside the allowed https hosts")
		}
		uris = append(uris, ru)
	}
	name := strings.TrimSpace(d.ClientName)
	if len([]rune(name)) > 100 {
		name = string([]rune(name)[:100])
	}
	return Client{ID: id, Name: name, RedirectURIs: uris}, nil
}

// ---------------------------------------------------------------------------
// 안전한 HTTPS 가져오기
// ---------------------------------------------------------------------------

// deniedPrefixes 는 IsGlobalUnicast/IsPrivate 가 걸러 주지 않는 비공개 대역이다.
var deniedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), // 6to4: 안에 임의 IPv4 를 담을 수 있다
}

// isPublicIP 는 인터넷에서 라우팅되는 공개 유니캐스트 주소인지 본다.
func isPublicIP(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	if !a.IsValid() || !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() ||
		a.IsLinkLocalUnicast() || a.IsMulticast() || a.IsUnspecified() {
		return false
	}
	for _, p := range deniedPrefixes {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

var errNonPublicAddr = errors.New("destination address is not public")

// guardDial 은 net.Dialer.Control 에 꽂는 검사다. 이름 해석이 끝난 뒤 실제로 접속할
// IP 에 대해 불리므로 DNS 재바인딩으로 사설 주소를 가리켜도 연결 전에 막힌다.
func guardDial(network, address string, _ syscall.RawConn) error {
	if network != "tcp4" && network != "tcp6" {
		return errNonPublicAddr
	}
	ap, err := netip.ParseAddrPort(address)
	if err != nil || !isPublicIP(ap.Addr()) {
		return errNonPublicAddr
	}
	return nil
}

// newSecureFetcher 는 CIMD 문서를 가져오는 함수를 만든다. 테스트는 tlsCfg 와
// dialControl 을 바꿔 로컬 TLS 서버를 쓴다(운영은 nil, guardDial).
func newSecureFetcher(tlsCfg *tls.Config, dialControl func(string, string, syscall.RawConn) error) cimdFetchFunc {
	if dialControl == nil {
		dialControl = guardDial
	}
	dialer := &net.Dialer{Timeout: cimdFetchTimeout, Control: dialControl}
	client := &http.Client{
		Timeout: cimdFetchTimeout,
		Transport: &http.Transport{
			Proxy:                  nil, // 환경 프록시를 쓰지 않는다
			DialContext:            dialer.DialContext,
			TLSClientConfig:        tlsCfg,
			TLSHandshakeTimeout:    cimdFetchTimeout,
			ResponseHeaderTimeout:  cimdFetchTimeout,
			DisableKeepAlives:      true,
			MaxResponseHeaderBytes: 16 << 10,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return func(ctx context.Context, rawURL string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, cimdFetchTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, errors.New("bad request")
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "second-brain-mcp-oauth")
		resp, err := client.Do(req)
		if err != nil {
			return nil, errors.New("fetch failed")
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("unexpected status %d (redirects are not followed)", resp.StatusCode)
		}
		mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if err != nil || mt != "application/json" {
			return nil, errors.New("content type is not application/json")
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, cimdMaxDocBytes+1))
		if err != nil {
			return nil, errors.New("read failed")
		}
		if len(body) > cimdMaxDocBytes {
			return nil, errors.New("document too large")
		}
		return body, nil
	}
}

// ---------------------------------------------------------------------------
// 레지스트리 조합
// ---------------------------------------------------------------------------

// multiRegistry 는 미리 등록된 클라이언트를 먼저 보고, URL 모양의 client_id 만
// CIMD 로 넘긴다.
type multiRegistry struct {
	static ClientRegistry
	cimd   *cimdRegistry
}

// Plausible 은 네트워크 접근 없이 client_id 가 알려진 클라이언트일 수 있는지 본다(토큰
// 엔드포인트용): 미리 등록된 ID 이거나 CIMD 허용 패턴에 맞는 URL.
func (m multiRegistry) Plausible(id string) bool {
	if _, ok := m.static.Lookup(context.Background(), id); ok {
		return true
	}
	return m.cimd != nil && looksLikeURL(id) && m.cimd.matches(id)
}

func (m multiRegistry) Lookup(ctx context.Context, id string) (Client, bool) {
	if c, ok := m.static.Lookup(ctx, id); ok {
		return c, true
	}
	if m.cimd != nil && looksLikeURL(id) {
		return m.cimd.Lookup(ctx, id)
	}
	return Client{}, false
}
