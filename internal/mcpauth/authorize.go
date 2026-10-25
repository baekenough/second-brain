package mcpauth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/baekenough/second-brain/internal/logsafe"
)

// authzParams 는 인가 요청 파라미터다. 동의 폼의 hidden 필드로 왕복하므로
// 원문(scope 는 요청된 그대로)을 담고, 검증은 GET·POST 에서 같은 함수로 다시 한다.
type authzParams struct {
	responseType string
	clientID     string
	redirectURI  string
	scope        string
	state        string
	challenge    string
	method       string
	resource     string
}

// oauthErr 는 클라이언트로 되돌려 줄 수 있는 OAuth 오류다. 설명은 고정 문구만
// 쓰고 입력값을 되돌려 주지 않는다.
type oauthErr struct {
	code string
	desc string
}

const stateMaxBytes = 512

// authzCheck 는 인가 요청 검증 결과다. fatal 이 있으면 redirect_uri 를 믿을 수
// 없으므로 리다이렉트하지 않고 오류 페이지를 보여 준다.
type authzCheck struct {
	client  Client
	p       authzParams
	granted string
	fatal   string
	err     *oauthErr
}

// validateAuthz 는 GET 쿼리와 POST 폼에 공통으로 쓰는 검증이다.
//
// 순서가 보안상 중요하다: client_id 와 redirect_uri 를 먼저 확정한 뒤에야 다른
// 오류를 redirect_uri 로 돌려보낸다. 확정 전의 모든 오류는 오류 페이지다
// (open redirect 방지).
func (s *Server) validateAuthz(ctx context.Context, v url.Values) authzCheck {
	var c authzCheck
	get := func(name string) (string, bool) {
		vals := v[name]
		switch len(vals) {
		case 0:
			return "", true
		case 1:
			return vals[0], true
		}
		return "", false // 같은 파라미터가 여러 번: 모호하므로 거절
	}

	var dup []string
	field := func(name string) string {
		val, one := get(name)
		if !one {
			dup = append(dup, name)
		}
		return val
	}
	c.p.clientID = field("client_id")
	c.p.redirectURI = field("redirect_uri")
	c.p.responseType = field("response_type")
	c.p.scope = field("scope")
	c.p.state = field("state")
	c.p.challenge = field("code_challenge")
	c.p.method = field("code_challenge_method")
	c.p.resource = field("resource")

	for _, d := range dup {
		if d == "client_id" || d == "redirect_uri" {
			c.fatal = "중복된 파라미터가 있어 요청을 처리할 수 없습니다."
			return c
		}
	}
	client, ok := s.clients.Lookup(ctx, c.p.clientID)
	if !ok {
		c.fatal = "알 수 없는 클라이언트입니다."
		return c
	}
	if c.p.redirectURI == "" || !containsString(client.RedirectURIs, c.p.redirectURI) {
		c.fatal = "등록되지 않은 redirect_uri 입니다."
		return c
	}

	// 여기부터는 redirect_uri 가 허용 목록과 완전 일치함이 확인됐다.
	c.client = client
	if len(dup) > 0 {
		c.err = &oauthErr{"invalid_request", "duplicate parameter"}
		return c
	}
	if len(c.p.state) > stateMaxBytes {
		c.p.state = "" // 너무 긴 state 는 되돌려 주지 않는다
		c.err = &oauthErr{"invalid_request", "state too long"}
		return c
	}
	if c.p.responseType != "code" {
		c.err = &oauthErr{"unsupported_response_type", "only response_type=code is supported"}
		return c
	}
	if c.p.challenge == "" {
		c.err = &oauthErr{"invalid_request", "code_challenge is required (PKCE)"}
		return c
	}
	// plain 이나 생략(= plain 기본값)은 다운그레이드이므로 거절한다.
	if c.p.method != "S256" {
		c.err = &oauthErr{"invalid_request", "code_challenge_method must be S256"}
		return c
	}
	if !validS256Challenge(c.p.challenge) {
		c.err = &oauthErr{"invalid_request", "malformed code_challenge"}
		return c
	}
	if c.p.resource != "" && c.p.resource != s.cfg.Resource {
		c.err = &oauthErr{"invalid_target", "unknown resource"}
		return c
	}
	c.granted = grantedScope(c.p.scope)
	return c
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// validS256Challenge 는 SHA-256 출력(32바이트)의 base64url(패딩 없음) 여부다.
func validS256Challenge(s string) bool {
	if len(s) != 43 {
		return false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	return err == nil && len(b) == sha256.Size
}

// validVerifier 는 RFC 7636 §4.1 code_verifier 문법이다.
func validVerifier(s string) bool {
	if len(s) < 43 || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
		default:
			return false
		}
	}
	return true
}

// pkceS256 는 verifier 에 대한 S256 챌린지다.
func pkceS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// 동의 폼 토큰: 서명 + 만료 + 일회용 nonce + 요청 파라미터 결합
// ---------------------------------------------------------------------------

type formTokenPayload struct {
	Nonce   string `json:"n"`
	Expires int64  `json:"e"`
	Params  string `json:"h"`
}

// paramsDigest 는 파라미터를 길이 접두 방식으로 이어 붙여 해시한다. 구분자
// 모호성이 없어서 한 필드의 끝을 다음 필드로 옮기는 식의 위조가 안 된다.
func paramsDigest(p authzParams) string {
	h := sha256.New()
	for _, f := range []string{p.responseType, p.clientID, p.redirectURI, p.scope,
		p.state, p.challenge, p.method, p.resource} {
		h.Write([]byte(strconv.Itoa(len(f))))
		h.Write([]byte{':'})
		h.Write([]byte(f))
	}
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func (s *Server) formMAC(b64payload string) []byte {
	m := hmac.New(sha256.New, s.csrfKey)
	m.Write([]byte(b64payload))
	return m.Sum(nil)
}

func (s *Server) newFormToken(p authzParams) string {
	payload, _ := json.Marshal(formTokenPayload{
		Nonce:   randomToken(16),
		Expires: s.now().Add(formTokenTTL).Unix(),
		Params:  paramsDigest(p),
	})
	b := base64.RawURLEncoding.EncodeToString(payload)
	return b + "." + base64.RawURLEncoding.EncodeToString(s.formMAC(b))
}

// checkFormToken 은 토큰이 서버가 만든 것이고, 만료 전이며, 이 파라미터에 묶인 것인지
// 확인한다. nonce 는 소모하지 않는다: 소모는 속도 제한을 통과한 승인 시도에서만 한다
// (거부 폼을 대량으로 제출해 nonce 저장소를 채우는 것을 막는다).
func (s *Server) checkFormToken(tok string, p authzParams) (nonce string, exp time.Time, ok bool) {
	b, sigPart, found := strings.Cut(tok, ".")
	if !found || len(tok) > 1024 {
		return "", time.Time{}, false
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(sigPart)
	if err != nil || !hmac.Equal(sig, s.formMAC(b)) {
		return "", time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(b)
	if err != nil {
		return "", time.Time{}, false
	}
	var fp formTokenPayload
	if json.Unmarshal(raw, &fp) != nil || fp.Nonce == "" {
		return "", time.Time{}, false
	}
	exp = time.Unix(fp.Expires, 0)
	if !s.now().Before(exp) {
		return "", time.Time{}, false
	}
	want := paramsDigest(p)
	if subtle.ConstantTimeCompare([]byte(fp.Params), []byte(want)) != 1 {
		return "", time.Time{}, false
	}
	return fp.Nonce, exp, true
}

// ---------------------------------------------------------------------------
// 핸들러
// ---------------------------------------------------------------------------

func setPageHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	// form-action 은 일부러 지정하지 않는다: 일부 브라우저는 폼 제출 뒤 리다이렉트
	// 대상(ChatGPT 콜백)까지 form-action 으로 막는다.
	h.Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
}

// requestContext 는 요청 ctx 에 클라이언트 IP 키를 싣는다(CIMD 가져오기 속도 제한용).
func (s *Server) requestContext(r *http.Request) context.Context {
	return context.WithValue(r.Context(), clientIPKey{}, clientKey(r, s.cfg.TrustedProxies))
}

func (s *Server) handleAuthorizeGET(w http.ResponseWriter, r *http.Request) {
	c := s.validateAuthz(s.requestContext(r), r.URL.Query())
	if c.fatal != "" {
		s.renderError(w, http.StatusBadRequest, c.fatal)
		return
	}
	if c.err != nil {
		s.redirectError(w, r, c.p, c.err)
		return
	}
	s.renderConsent(w, http.StatusOK, c, "")
}

func (s *Server) handleAuthorizePOST(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, authorizeBodyMaxBytes)
	if err := r.ParseForm(); err != nil {
		s.renderError(w, http.StatusBadRequest, "요청을 읽을 수 없습니다.")
		return
	}
	// 본문 폼만 본다. URL 쿼리를 섞어 읽으면 폼 토큰이 묶어 둔 값과 다른 값이
	// 끼어들 수 있다.
	form := r.PostForm

	c := s.validateAuthz(s.requestContext(r), form)
	if c.fatal != "" {
		s.renderError(w, http.StatusBadRequest, c.fatal)
		return
	}
	if c.err != nil {
		s.redirectError(w, r, c.p, c.err)
		return
	}

	nonce, nonceExp, ok := s.checkFormToken(form.Get("form_token"), c.p)
	if !ok {
		s.renderConsent(w, http.StatusBadRequest, c, "폼이 만료되었거나 유효하지 않습니다. 다시 시도하세요.")
		return
	}

	// 거부는 키 추측이 아니고 되돌려 줄 것도 없다: 속도 제한도 nonce 소모도 하지 않는다.
	if form.Get("action") == "deny" {
		s.redirectError(w, r, c.p, &oauthErr{"access_denied", "the owner denied the request"})
		return
	}

	// 속도 제한을 nonce 소모보다 먼저 건다.
	ip := clientKey(r, s.cfg.TrustedProxies)
	release, ok, retry := s.limiter.reserve(ip)
	if !ok {
		s.log.Warn("mcp oauth: consent locked out", "client_ip", ip)
		secs := int(retry.Seconds()) + 1
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		s.renderError(w, http.StatusTooManyRequests,
			"실패한 시도가 너무 많아 잠겼습니다. 잠시 후 다시 시도하세요.")
		return
	}
	if !s.nonces.consume(nonce, nonceExp) {
		release() // 이미 쓴 폼: 키 추측이 아니므로 시도로 세지 않는다
		s.renderConsent(w, http.StatusBadRequest, c, "폼이 이미 사용되었습니다. 다시 시도하세요.")
		return
	}

	key := form.Get("owner_key")
	sum := sha256.Sum256([]byte(key))
	if len(key) == 0 || len(key) > 1024 ||
		subtle.ConstantTimeCompare(sum[:], s.cfg.OwnerKeySHA256[:]) != 1 {
		// 예약은 되돌리지 않는다 = 실패로 센다.
		s.log.Warn("mcp oauth: consent rejected (wrong owner key)", "client_ip", ip)
		s.renderConsent(w, http.StatusForbidden, c, "소유자 키가 올바르지 않습니다.")
		return
	}
	release()

	code := s.codes.issue(authCode{
		clientID:    c.p.clientID,
		redirectURI: c.p.redirectURI,
		challenge:   c.p.challenge,
		scope:       c.granted,
		resource:    s.cfg.Resource,
	}, s.cfg.CodeTTL)
	s.log.Info("mcp oauth: authorization granted", "client_id", c.p.clientID, "scope", c.granted)

	q := url.Values{}
	q.Set("code", code)
	if c.p.state != "" {
		q.Set("state", c.p.state)
	}
	s.redirect(w, r, c.p.redirectURI, q)
}

// redirectError 는 검증된 redirect_uri 로 오류를 돌려보낸다. RFC 9207 에 따라
// 오류 리다이렉트에도 iss 를 싣는다.
func (s *Server) redirectError(w http.ResponseWriter, r *http.Request, p authzParams, e *oauthErr) {
	q := url.Values{}
	q.Set("error", e.code)
	if e.desc != "" {
		q.Set("error_description", e.desc)
	}
	if p.state != "" {
		q.Set("state", p.state)
	}
	s.redirect(w, r, p.redirectURI, q)
}

// redirect 는 호출자가 허용 목록으로 검증한 redirectURI 에만 쓴다. 기존 쿼리는
// 보존하고 iss(RFC 9207)를 항상 덧붙인다.
func (s *Server) redirect(w http.ResponseWriter, r *http.Request, redirectURI string, extra url.Values) {
	u, err := url.Parse(redirectURI)
	if err != nil { // 설정 단계에서 검증되므로 도달하지 않는다
		s.renderError(w, http.StatusInternalServerError, "내부 오류입니다.")
		return
	}
	q := u.Query()
	for k, vs := range extra {
		for _, v := range vs {
			q.Set(k, v)
		}
	}
	q.Set("iss", s.cfg.Issuer)
	u.RawQuery = q.Encode()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// 화면
// ---------------------------------------------------------------------------

type hiddenField struct{ Name, Value string }

type consentView struct {
	AppName    string
	ClientID   string
	ClientName string
	Scope      string
	Action     string
	FormToken  string
	Error      string
	Hidden     []hiddenField
}

const pageStyle = `body{font-family:system-ui,sans-serif;max-width:30rem;margin:3rem auto;padding:0 1rem;line-height:1.5}` +
	`code{background:#f2f2f2;padding:.1rem .3rem;border-radius:3px}` +
	`input[type=password]{width:100%;padding:.5rem;box-sizing:border-box;margin:.5rem 0 1rem}` +
	`button{padding:.5rem 1.2rem;margin-right:.5rem}.err{color:#b00020}`

const consentHTML = `<!doctype html>
<html lang="ko"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex"><title>{{.AppName}} 접근 승인</title><style>` + pageStyle + `</style></head>
<body>
<h1>{{.AppName}} 접근 승인</h1>
<p>클라이언트 {{if .ClientName}}<strong>{{.ClientName}}</strong> {{end}}<code>{{.ClientID}}</code> 가 다음 권한을 요청합니다.</p>
<ul><li><code>{{.Scope}}</code> — 지식 베이스 검색·조회 (읽기 전용, 쓰기 불가)</li></ul>
{{if .Error}}<p class="err" role="alert">{{.Error}}</p>{{end}}
<form method="post" action="{{.Action}}" autocomplete="off">
{{range .Hidden}}<input type="hidden" name="{{.Name}}" value="{{.Value}}">
{{end}}<input type="hidden" name="form_token" value="{{.FormToken}}">
<label for="owner_key">소유자 키</label>
<input id="owner_key" type="password" name="owner_key" autocomplete="off" autofocus required>
<button type="submit" name="action" value="approve">승인</button>
<button type="submit" name="action" value="deny" formnovalidate>거부</button>
</form>
</body></html>`

const errorHTML = `<!doctype html>
<html lang="ko"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex"><title>요청 오류</title><style>` + pageStyle + `</style></head>
<body><h1>요청을 처리할 수 없습니다</h1><p>{{.}}</p></body></html>`

func (s *Server) parseTemplates() error {
	var err error
	if s.consent, err = template.New("consent").Parse(consentHTML); err != nil {
		return err
	}
	s.errPage, err = template.New("error").Parse(errorHTML)
	return err
}

func (s *Server) renderConsent(w http.ResponseWriter, status int, c authzCheck, msg string) {
	p := c.p
	view := consentView{
		AppName:    s.cfg.AppName,
		ClientID:   p.clientID,
		ClientName: c.client.Name,
		Scope:      c.granted,
		Action:     PathAuthorize,
		FormToken:  s.newFormToken(p),
		Error:      msg,
		Hidden: []hiddenField{
			{"response_type", p.responseType}, {"client_id", p.clientID},
			{"redirect_uri", p.redirectURI}, {"scope", p.scope}, {"state", p.state},
			{"code_challenge", p.challenge}, {"code_challenge_method", p.method},
			{"resource", p.resource},
		},
	}
	setPageHeaders(w)
	w.WriteHeader(status)
	if err := s.consent.Execute(w, view); err != nil {
		s.log.Warn("mcp oauth: render consent failed", logsafe.ErrAttrs(err)...)
	}
}

func (s *Server) renderError(w http.ResponseWriter, status int, msg string) {
	setPageHeaders(w)
	w.WriteHeader(status)
	if err := s.errPage.Execute(w, msg); err != nil {
		s.log.Warn("mcp oauth: render error page failed", logsafe.ErrAttrs(err)...)
	}
}
