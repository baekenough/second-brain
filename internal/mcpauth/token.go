package mcpauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/baekenough/second-brain/internal/logsafe"
)

// tokenResponse 는 RFC 6749 §5.1 성공 응답이다.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

func writeTokenJSON(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// tokenError 는 RFC 6749 §5.2 오류 응답이다. 설명은 고정 문구만 쓴다.
func tokenError(w http.ResponseWriter, status int, code, desc string) {
	body := map[string]string{"error": code}
	if desc != "" {
		body["error_description"] = desc
	}
	writeTokenJSON(w, status, body)
}

func hashToken(t string) []byte {
	h := sha256.Sum256([]byte(t))
	return h[:]
}

// handleToken 은 POST /token 이다(form-encoded 요청, JSON 응답).
//
// 클라이언트 인증은 none(공개 클라이언트)뿐이다: client_id 만 확인하고 Basic
// 헤더나 client_secret 은 인증 수단으로 쓰지 않는다. 보호는 PKCE 와 코드의 바인딩이
// 맡는다.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, tokenBodyMaxBytes)
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/x-www-form-urlencoded" {
		tokenError(w, http.StatusBadRequest, "invalid_request", "content type must be application/x-www-form-urlencoded")
		return
	}
	if err := r.ParseForm(); err != nil {
		tokenError(w, http.StatusBadRequest, "invalid_request", "malformed request body")
		return
	}
	form := r.PostForm // URL 쿼리의 파라미터는 무시한다
	for _, vals := range form {
		if len(vals) > 1 {
			tokenError(w, http.StatusBadRequest, "invalid_request", "duplicate parameter")
			return
		}
	}

	// 이 서버가 광고하는 클라이언트 인증 방식은 none 뿐이다. 클라이언트 어서션
	// (private_key_jwt)은 지원하지 않으므로 보내면 거절한다.
	if form.Get("client_assertion") != "" || form.Get("client_assertion_type") != "" {
		tokenError(w, http.StatusBadRequest, "invalid_client", "client authentication method not supported")
		return
	}

	// 여기서는 라이브 클라이언트 조회(CIMD 문서 가져오기)를 하지 않는다. 클라이언트의
	// 정당성은 인가 시점에 확인됐고 코드·리프레시 계보에 client_id 로 묶여 있다. 문서
	// 서버 장애나 캐시 오염이 갱신을 막지 못하게 한다. 형태만 확인한다.
	clientID := form.Get("client_id")
	if !s.clientPlausible(r.Context(), clientID) {
		tokenError(w, http.StatusBadRequest, "invalid_client", "unknown client")
		return
	}

	switch form.Get("grant_type") {
	case "authorization_code":
		s.grantAuthorizationCode(w, r, form, clientID)
	case "refresh_token":
		s.grantRefreshToken(w, r, form, clientID)
	default:
		tokenError(w, http.StatusBadRequest, "unsupported_grant_type", "")
	}
}

func (s *Server) grantAuthorizationCode(w http.ResponseWriter, r *http.Request, form url.Values, clientID string) {
	code := form.Get("code")
	verifier := form.Get("code_verifier")
	redirectURI := form.Get("redirect_uri")
	if code == "" || len(code) > 256 {
		tokenError(w, http.StatusBadRequest, "invalid_request", "code is required")
		return
	}

	// 코드는 이후 검증 결과와 상관없이 여기서 소모된다.
	rec, st := s.codes.consume(code)
	switch st {
	case codeReplayed:
		// 이미 쓴 코드를 다시 냈다: 탈취 가능성이 있으므로 그 코드로 발급한 토큰의
		// 계보를 폐기한다(RFC 6749 §4.1.2). 이미 나간 액세스 토큰은 만료까지 산다.
		if rec.familyID != "" {
			if err := s.store.RevokeFamily(r.Context(), rec.familyID, s.now()); err != nil {
				s.log.Warn("mcp oauth: revoke family after code replay failed", logsafe.ErrAttrs(err)...)
			}
		}
		s.log.Warn("mcp oauth: authorization code replay detected", "family_id", rec.familyID)
		tokenError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid")
		return
	case codeUnknown, codeExpired:
		tokenError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid")
		return
	}

	// 코드를 소모한 즉시 이 코드가 낳을 토큰 계보를 정한다. 발급(저장소 쓰기) 도중에
	// 같은 코드가 재사용되어도 계보를 폐기할 수 있도록 Create 보다 먼저 연결해 둔다.
	familyID := uuid.NewString()
	s.codes.attachFamily(code, familyID)

	// 바인딩 검증. 어느 것이 틀렸는지는 응답에서 구분하지 않는다.
	if rec.clientID != clientID || rec.redirectURI != redirectURI {
		tokenError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid")
		return
	}
	if res := form.Get("resource"); res != "" && res != rec.resource {
		tokenError(w, http.StatusBadRequest, "invalid_target", "unknown resource")
		return
	}
	if !validVerifier(verifier) ||
		subtle.ConstantTimeCompare([]byte(pkceS256(verifier)), []byte(rec.challenge)) != 1 {
		tokenError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid")
		return
	}

	grant := RefreshGrant{
		FamilyID: familyID,
		ClientID: rec.clientID,
		Scope:    rec.scope,
		Resource: rec.resource,
	}
	refresh := randomToken(32)
	now := s.now()
	if err := s.store.Create(r.Context(), grant, hashToken(refresh), now.Add(s.cfg.RefreshTokenTTL), now); err != nil {
		s.log.Warn("mcp oauth: store refresh token failed", logsafe.ErrAttrs(err)...)
		tokenError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	// 저장소 쓰기 도중에 같은 코드가 다시 제시됐다면(탈취 가능성) 방금 만든 계보를
	// 폐기하고 토큰을 내주지 않는다.
	if s.codes.wasReplayed(code) {
		if err := s.store.RevokeFamily(r.Context(), grant.FamilyID, s.now()); err != nil {
			s.log.Warn("mcp oauth: revoke family after code replay failed", logsafe.ErrAttrs(err)...)
		}
		s.log.Warn("mcp oauth: authorization code replay detected during issuance", "family_id", grant.FamilyID)
		tokenError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid")
		return
	}
	s.respondWithTokens(w, grant, refresh)
}

func (s *Server) grantRefreshToken(w http.ResponseWriter, r *http.Request, form url.Values, clientID string) {
	presented := form.Get("refresh_token")
	if presented == "" || len(presented) > 256 {
		tokenError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}
	// 회전 전에 확인한다: 회전은 되돌릴 수 없다.
	if res := form.Get("resource"); res != "" && res != s.cfg.Resource {
		tokenError(w, http.StatusBadRequest, "invalid_target", "unknown resource")
		return
	}
	// scope 파라미터는 원래 발급 범위 안에서만 의미가 있고, 이 서버는 범위를 하나
	// (brain:read)만 발급하므로 좁힐 수 있는 것이 없다. 무시하고 원래 범위를 쓴다.

	newRefresh := randomToken(32)
	now := s.now()
	grant, outcome, err := s.store.Rotate(r.Context(), RotateRequest{
		PresentedHash: hashToken(presented),
		ClientID:      clientID,
		NewHash:       hashToken(newRefresh),
		NewExpiresAt:  now.Add(s.cfg.RefreshTokenTTL),
		Now:           now,
	})
	if err != nil {
		s.log.Warn("mcp oauth: rotate refresh token failed", logsafe.ErrAttrs(err)...)
		tokenError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	switch outcome {
	case RotateOK:
		s.respondWithTokens(w, grant, newRefresh)
	case RotateReused:
		s.log.Warn("mcp oauth: refresh token reuse detected; token family revoked")
		tokenError(w, http.StatusBadRequest, "invalid_grant", "refresh token is invalid")
	default:
		tokenError(w, http.StatusBadRequest, "invalid_grant", "refresh token is invalid")
	}
}

func (s *Server) respondWithTokens(w http.ResponseWriter, g RefreshGrant, refresh string) {
	now := s.now()
	claims := Claims{
		Issuer:    s.cfg.Issuer,
		Audience:  s.cfg.Resource,
		Subject:   SubjectOwner,
		Scope:     g.Scope,
		ClientID:  g.ClientID,
		IssuedAt:  now.Unix(),
		NotBefore: now.Unix(),
		ExpiresAt: now.Add(s.cfg.AccessTokenTTL).Unix(),
		ID:        randomToken(16),
	}
	access, err := s.signer.sign(claims)
	if err != nil {
		s.log.Warn("mcp oauth: sign access token failed", logsafe.ErrAttrs(err)...)
		tokenError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	writeTokenJSON(w, http.StatusOK, tokenResponse{
		AccessToken:  access,
		TokenType:    "Bearer",
		ExpiresIn:    int64(s.cfg.AccessTokenTTL.Seconds()),
		RefreshToken: refresh,
		Scope:        strings.TrimSpace(g.Scope),
	})
}
