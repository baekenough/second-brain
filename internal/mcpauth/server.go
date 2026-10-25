package mcpauth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"
)

// 엔드포인트 경로. 메타데이터가 이 값을 그대로 광고한다. MCP 인가 사양의
// 폴백 경로(/authorize, /token)와 같아서, 메타데이터를 읽지 않는 클라이언트도
// 맞는 곳에 도달한다.
const (
	PathAuthorize         = "/authorize"
	PathToken             = "/token"
	PathASMetadata        = "/.well-known/oauth-authorization-server"
	PathOpenIDConfig      = "/.well-known/openid-configuration"
	pathPRMBase           = mcpserver.WellKnownProtectedResourcePath
	formTokenTTL          = 10 * time.Minute
	authorizeBodyMaxBytes = 8 << 10
	tokenBodyMaxBytes     = 16 << 10
)

// Client 는 등록된 OAuth 클라이언트다.
type Client struct {
	ID string
	// Name 은 동의 화면에 보이는 표시 이름이다(CIMD 문서의 client_name, 선택).
	Name         string
	RedirectURIs []string
}

// ClientRegistry 는 client_id 로 클라이언트를 찾는다. 지금은 미리 등록된 공개
// 클라이언트 하나뿐이지만, DCR(RFC 7591)이나 Client ID Metadata Document 는
// 이 인터페이스의 다른 구현으로 붙이고 인가·토큰 핸들러는 건드리지 않는다.
// 모든 클라이언트는 공개 클라이언트(auth method none)로 취급한다. CIMD(URL 형태의
// client_id)는 cimd.go 의 레지스트리가 같은 인터페이스로 제공한다.
type ClientRegistry interface {
	Lookup(ctx context.Context, clientID string) (Client, bool)
}

type staticRegistry struct{ client Client }

func (r staticRegistry) Lookup(_ context.Context, id string) (Client, bool) {
	// client_id 는 비밀이 아니므로 일반 비교로 충분하다.
	if id != "" && id == r.client.ID {
		return r.client, true
	}
	return Client{}, false
}

// Server 는 인가 서버(AS)와 리소스 서버 쪽 토큰 검증을 함께 가진다.
type Server struct {
	cfg     Config
	store   RefreshStore
	clients ClientRegistry
	signer  *tokenSigner
	csrfKey []byte

	codes   *codeStore
	nonces  *nonceStore
	limiter *attemptLimiter
	now     func() time.Time
	log     *slog.Logger

	cimdFetch  cimdFetchFunc
	prmURL     string
	prmHandler http.Handler
	asDoc      []byte
	consent    *template.Template
	errPage    *template.Template
}

type clientIPKey struct{}

// clientPlausible 는 client_id 가 알려진 클라이언트일 수 있는 모양인지 본다(네트워크
// 접근 없음). 레지스트리가 Plausible 을 구현하지 않으면 Lookup 으로 대체한다.
func (s *Server) clientPlausible(ctx context.Context, id string) bool {
	if p, ok := s.clients.(interface{ Plausible(string) bool }); ok {
		return p.Plausible(id)
	}
	_, ok := s.clients.Lookup(ctx, id)
	return ok
}

// Option 은 NewServer 의 선택 설정이다.
type Option func(*Server)

// WithClock 은 시계를 바꾼다(테스트용).
func WithClock(now func() time.Time) Option { return func(s *Server) { s.now = now } }

// WithLogger 는 로거를 바꾼다. 이 패키지는 키·코드·토큰을 로그에 넘기지 않으며,
// 넘기는 값은 고정 문구·ID(계보·클라이언트)·IP·오류 모양뿐이다.
func WithLogger(l *slog.Logger) Option { return func(s *Server) { s.log = l } }

// WithCIMDFetcher 는 CIMD 문서를 가져오는 함수를 바꾼다(테스트용). 운영은 공개 IP 만
// 접속하는 secureFetcher 를 쓴다.
func WithCIMDFetcher(f cimdFetchFunc) Option { return func(s *Server) { s.cimdFetch = f } }

// WithClientRegistry 는 클라이언트 조회 구현을 바꾼다.
func WithClientRegistry(r ClientRegistry) Option { return func(s *Server) { s.clients = r } }

// NewServer 는 검증된 Config 와 리프레시 토큰 저장소로 서버를 만든다.
func NewServer(cfg Config, store RefreshStore, opts ...Option) (*Server, error) {
	if store == nil {
		return nil, errors.New("mcpauth: refresh store is required")
	}
	if cfg.Issuer == "" || cfg.Resource == "" || cfg.ClientID == "" || len(cfg.RedirectURIs) == 0 {
		return nil, errors.New("mcpauth: incomplete config")
	}
	if len(cfg.SigningKey) < MinSigningKeyBytes {
		return nil, fmt.Errorf("mcpauth: signing key must be at least %d bytes", MinSigningKeyBytes)
	}
	if cfg.AccessTokenTTL <= 0 {
		cfg.AccessTokenTTL = DefaultAccessTokenTTL
	}
	if cfg.RefreshTokenTTL <= 0 {
		cfg.RefreshTokenTTL = DefaultRefreshTokenTTL
	}
	if cfg.CodeTTL <= 0 {
		cfg.CodeTTL = DefaultCodeTTL
	}
	if cfg.AppName == "" {
		cfg.AppName = "second-brain"
	}

	s := &Server{
		cfg:    cfg,
		store:  store,
		now:    time.Now,
		log:    slog.Default(),
		signer: &tokenSigner{key: cfg.SigningKey, issuer: cfg.Issuer, resource: cfg.Resource},
	}
	for _, o := range opts {
		o(s)
	}
	if s.clients == nil {
		reg := multiRegistry{static: staticRegistry{client: Client{ID: cfg.ClientID, RedirectURIs: cfg.RedirectURIs}}}
		if len(cfg.CIMDClientIDPatterns) > 0 {
			fetch := s.cimdFetch
			if fetch == nil {
				fetch = newSecureFetcher(nil, nil)
			}
			c, err := newCIMDRegistry(cfg.CIMDClientIDPatterns, cfg.CIMDRedirectHosts, fetch,
				func() time.Time { return s.now() }, s.log)
			if err != nil {
				return nil, err
			}
			reg.cimd = c
		}
		s.clients = reg
	}
	s.codes = newCodeStore(func() time.Time { return s.now() })
	s.nonces = newNonceStore(func() time.Time { return s.now() })
	s.limiter = newAttemptLimiter(func() time.Time { return s.now() }, attemptWindow, attemptsPerIP, attemptsGlobally)

	// 폼 토큰 키는 JWT 서명 키에서 용도 라벨로 파생한다. 같은 키를 두 용도에
	// 그대로 쓰면 한쪽에서 만든 MAC 이 다른 쪽에서 통용될 여지가 생긴다.
	kd := hmac.New(sha256.New, cfg.SigningKey)
	kd.Write([]byte("second-brain/mcpauth/consent-form/v1"))
	s.csrfKey = kd.Sum(nil)

	resURL, err := url.Parse(cfg.Resource)
	if err != nil {
		return nil, fmt.Errorf("mcpauth: resource: %w", err)
	}
	s.prmURL = resURL.Scheme + "://" + resURL.Host + mcpserver.ProtectedResourceMetadataPath(cfg.Resource)

	s.prmHandler = mcpserver.NewProtectedResourceMetadataHandler(mcpserver.ProtectedResourceMetadataConfig{
		Resource:               cfg.Resource,
		AuthorizationServers:   []string{cfg.Issuer},
		ScopesSupported:        supportedScopes,
		BearerMethodsSupported: []string{"header"},
		ResourceName:           cfg.AppName,
	})

	doc, err := json.Marshal(asMetadata{
		Issuer:                            cfg.Issuer,
		AuthorizationEndpoint:             cfg.Issuer + PathAuthorize,
		TokenEndpoint:                     cfg.Issuer + PathToken,
		ScopesSupported:                   supportedScopes,
		ResponseTypesSupported:            []string{"code"},
		GrantTypesSupported:               []string{"authorization_code", "refresh_token"},
		CodeChallengeMethodsSupported:     []string{"S256"},
		TokenEndpointAuthMethodsSupported: []string{"none"},
		IssParameterSupported:             true,
		CIMDSupported:                     len(cfg.CIMDClientIDPatterns) > 0,
	})
	if err != nil {
		return nil, err
	}
	s.asDoc = doc

	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	return s, nil
}

// asMetadata 는 RFC 8414 인가 서버 메타데이터다. OIDC 전용 필드(jwks_uri,
// userinfo_endpoint, id_token_signing_alg_values_supported 등)는 구현하지 않으므로
// 일부러 넣지 않는다 — openid-configuration 경로로 같은 문서를 줄 때도 마찬가지다.
type asMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	ScopesSupported                   []string `json:"scopes_supported"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	IssParameterSupported             bool     `json:"authorization_response_iss_parameter_supported"`
	// CIMDSupported 는 URL 형태 client_id(Client ID Metadata Document)를 받는다는
	// 뜻이다. 꺼져 있으면 필드를 아예 싣지 않는다.
	CIMDSupported bool `json:"client_id_metadata_document_supported,omitempty"`
}

// Handler 는 OAuth 엔드포인트와 메타데이터를 서빙하고, 나머지 모든 경로는
// 리소스 게이트를 거쳐 protected(MCP 핸들러)로 넘긴다. staticKey 는 기존 정적
// API_KEY 다(비어 있으면 정적 키 경로는 없다).
func (s *Server) Handler(staticKey string, protected http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(pathPRMBase, s.prmHandler)
	if p := mcpserver.ProtectedResourceMetadataPath(s.cfg.Resource); p != pathPRMBase {
		mux.Handle(p, s.prmHandler)
	}
	mux.HandleFunc(PathASMetadata, s.serveASMetadata)
	mux.HandleFunc(PathOpenIDConfig, s.serveASMetadata)
	mux.HandleFunc("GET "+PathAuthorize, s.handleAuthorizeGET)
	mux.HandleFunc("POST "+PathAuthorize, s.handleAuthorizePOST)
	mux.HandleFunc("POST "+PathToken, s.handleToken)
	// 다른 메서드는 MCP 게이트(401)로 흘리지 않고 405 로 답한다.
	mux.HandleFunc(PathAuthorize, methodNotAllowed("GET, POST"))
	mux.HandleFunc(PathToken, methodNotAllowed("POST"))
	mux.Handle("/", s.Gate(staticKey, protected))
	return mux
}

func methodNotAllowed(allow string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", allow)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) serveASMetadata(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet, http.MethodHead:
	default:
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(s.asDoc)
	}
}

// ---------------------------------------------------------------------------
// 리소스 서버 쪽: 자격 판정과 401 게이트
// ---------------------------------------------------------------------------

// ErrNoCredentials 는 Authorization Bearer 가 없다는 뜻이다.
var ErrNoCredentials = errors.New("no bearer credentials")

// Authenticate 는 요청의 Bearer 토큰을 판정한다. 정적 API 키와 일치하면
// PrincipalStatic, 유효한 액세스 토큰이면 PrincipalOAuth 다. 둘 다 아니면
// PrincipalNone 과 오류를 돌려준다.
func (s *Server) Authenticate(r *http.Request, staticKey string) (Principal, error) {
	tok, ok := bearerToken(r)
	if !ok {
		return Principal{}, ErrNoCredentials
	}
	if staticKey != "" && constantTimeEqual(tok, staticKey) {
		return Principal{Kind: PrincipalStatic}, nil
	}
	c, err := s.signer.verify(tok, s.now())
	if err != nil {
		return Principal{}, err
	}
	return Principal{
		Kind:     PrincipalOAuth,
		Scopes:   splitScope(c.Scope),
		ClientID: c.ClientID,
		TokenID:  c.ID,
	}, nil
}

// bearerToken 은 RFC 6750 대로 스킴 이름의 대소문자를 구분하지 않는다.
func bearerToken(r *http.Request) (string, bool) {
	const prefix = "bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return h[len(prefix):], true
}

// constantTimeEqual 은 길이까지 드러내지 않도록 다이제스트로 비교한다.
func constantTimeEqual(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

// ChallengeOpts 는 WWW-Authenticate Bearer 도전의 선택 필드다.
type ChallengeOpts struct {
	Error       string
	Description string
	Scope       string
}

// Challenge 는 `Bearer resource_metadata="…"[, error][, error_description][, scope]`
// 형태의 값을 만든다. resource_metadata 는 항상 첫 파라미터다(ChatGPT 가 이 URL 로
// 인가 서버를 찾는다).
func (s *Server) Challenge(o ChallengeOpts) string {
	var b strings.Builder
	b.WriteString(`Bearer resource_metadata="` + s.prmURL + `"`)
	if o.Error != "" {
		b.WriteString(`, error="` + o.Error + `"`)
	}
	if o.Description != "" {
		b.WriteString(`, error_description="` + o.Description + `"`)
	}
	if o.Scope != "" {
		b.WriteString(`, scope="` + o.Scope + `"`)
	}
	return b.String()
}

// ResourceMetadataURL 은 도전에 싣는 PRM URL 이다.
func (s *Server) ResourceMetadataURL() string { return s.prmURL }

// Gate 는 "혼합 모드"(OAuth 또는 인증 없음) 게이트다.
//
//   - Authorization 헤더가 없으면 그대로 통과시킨다. ChatGPT 는 혼합 모드에서
//     initialize 와 tools/list 를 인증 없이 보내고, 도구마다 securitySchemes 로
//     인증 여부를 판단한다. 인증이 필요한 도구 호출은 도구 계층(cmd/mcp)이
//     `_meta["mcp/www_authenticate"]` 가 담긴 도구 오류로 거절한다.
//   - 헤더가 있는데 정적 API 키도 유효한 JWT 도 아니면 HTTP 401 +
//     `WWW-Authenticate: Bearer resource_metadata="…", error="invalid_token"`.
//     ChatGPT 는 이 응답으로 토큰을 갱신한다. 헤더 판정은 본문을 읽기 전에 한다.
func (s *Server) Gate(staticKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hasAuthorization(r) {
			capBody(w, r, Principal{})
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), Principal{})))
			return
		}
		p, err := s.Authenticate(r, staticKey)
		if err != nil {
			// 사유는 응답에 싣지 않는다. 로그에는 형식 오류 사유만 남기고 토큰은 남기지 않는다.
			s.log.Debug("mcp oauth: bearer rejected", "reason", err.Error())
			w.Header().Set("WWW-Authenticate", s.Challenge(ChallengeOpts{Error: "invalid_token"}))
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
			return
		}
		capBody(w, r, p)
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// NonStaticBodyMaxBytes 는 정적 API 키가 아닌 주체(인증 없음, OAuth)의 요청 본문 상한이다.
// 이들이 부르는 도구는 읽기뿐이라 큰 본문이 필요 없다. 정적 키는 add_note(10MB) 때문에
// 더 큰 상한(cmd/mcp 의 mcpRequestMaxBytes)을 그대로 쓴다. 인증 없는 요청이 서버 메모리를
// 크게 쓰게 하지 못하도록 이 상한을 게이트에서 먼저 건다.
const NonStaticBodyMaxBytes = 1 << 20

func capBody(w http.ResponseWriter, r *http.Request, p Principal) {
	if p.Kind != PrincipalStatic && r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, NonStaticBodyMaxBytes)
	}
}

func hasAuthorization(r *http.Request) bool {
	for _, v := range r.Header.Values("Authorization") {
		if v != "" {
			return true
		}
	}
	return false
}

// AuthContextFunc 는 mcp-go 의 HTTPContextFunc 로 쓸 함수를 돌려준다. 게이트와
// 같은 Authenticate 를 다시 호출해 두 곳의 판정이 어긋나지 않게 한다.
func (s *Server) AuthContextFunc(staticKey string) func(context.Context, *http.Request) context.Context {
	return func(ctx context.Context, r *http.Request) context.Context {
		p, _ := s.Authenticate(r, staticKey)
		return WithPrincipal(ctx, p)
	}
}
