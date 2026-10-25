package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/baekenough/second-brain/internal/mcpauth"
)

// ---------------------------------------------------------------------------
// OAuth 2.1 (internal/mcpauth) 배선
//
// MCP_PUBLIC_BASE_URL 등 환경 변수가 비어 있으면 이 파일의 코드는 하나도 실행되지
// 않고 기존 동작(정적 API_KEY, 인증 없는 initialize 허용)이 그대로다.
// ---------------------------------------------------------------------------

// toolScopes 는 도구별로 요구하는 스코프다. 새 도구를 등록하면 여기에도 넣어야
// 한다 — 없는 도구는 requiredScope 가 쓰기 스코프를 요구하게 해서 OAuth 토큰에
// 기본 거부(default deny)가 되도록 했다.
var toolScopes = map[string]string{
	"search":       mcpauth.ScopeRead,
	"get_document": mcpauth.ScopeRead,
	"stats":        mcpauth.ScopeRead,
	"add_note":     mcpauth.ScopeWrite,
}

func requiredScope(tool string) string {
	if s, ok := toolScopes[tool]; ok {
		return s
	}
	return mcpauth.ScopeWrite
}

// oauthPrereqError 는 OAuth 가 켜졌을 때의 선행 조건을 확인한다. API_KEY 가
// 비어 있으면 모든 요청이 전권으로 허용되는 개발 모드라 OAuth 를 켜도 보호가
// 없다. 의도치 않은 공개 노출을 막으려고 기동을 거절한다.
func oauthPrereqError(c *mcpauth.Config, apiKey string) error {
	if c != nil && apiKey == "" {
		return errors.New("mcp oauth config: API_KEY must be set when OAuth is enabled")
	}
	return nil
}

// oauthAuthContextFunc 는 HTTPContextFunc 다. 게이트와 같은 Authenticate 로 주체를
// 정해 ctx 에 싣고, 기존 도구 핸들러가 쓰는 mcpAuthKey 도 함께 채운다(유효한
// 자격이면 true). 스코프 판정은 도구 미들웨어가 한다.
func oauthAuthContextFunc(as *mcpauth.Server, apiKey string) server.HTTPContextFunc {
	base := as.AuthContextFunc(apiKey)
	return func(ctx context.Context, r *http.Request) context.Context {
		ctx = base(ctx, r)
		p, _ := mcpauth.PrincipalFrom(ctx)
		return context.WithValue(ctx, mcpAuthKey{}, p.Kind != mcpauth.PrincipalNone)
	}
}

// oauthToolMiddleware 는 tools/call 마다 호출자의 스코프가 도구의 요구를 채우는지
// 확인한다. 혼합 모드(OAuth 또는 인증 없음)라 인증 없는 initialize/tools/list 는 통과하고,
// 인증이 필요한 도구 호출이 여기서 거절된다. 주체가 ctx 에 없으면 인증 없음으로
// 취급해 거부한다(fail closed).
func oauthToolMiddleware(as *mcpauth.Server) server.ToolHandlerMiddleware {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			need := requiredScope(req.Params.Name)
			p, _ := mcpauth.PrincipalFrom(ctx)
			if !p.HasScope(need) {
				return scopeDeniedResult(as, p, need), nil
			}
			return next(ctx, req)
		}
	}
}

// scopeDeniedResult 는 인증 부족·스코프 부족을 도구 오류(isError: true)로 알린다.
//
// 요구 스코프를 이 서버가 발급할 수 있을 때만 _meta["mcp/www_authenticate"] 에
// 도전을 싣는다: 클라이언트가 그걸 보고 (재)인가를 시도하므로, 발급할 수 없는
// 스코프(쓰기)에 대해 싣으면 헛된 인가 루프만 만든다. 그 경우는 재인가로 해결되지
// 않으므로 평문 거절이다.
func scopeDeniedResult(as *mcpauth.Server, p mcpauth.Principal, need string) *mcp.CallToolResult {
	if !mcpauth.Supports(need) {
		return mcp.NewToolResultError("forbidden: this tool is not available to OAuth clients")
	}
	opts := mcpauth.ChallengeOpts{Error: "insufficient_scope", Scope: need}
	msg := "insufficient_scope: this tool requires scope " + need
	if p.Kind == mcpauth.PrincipalNone {
		opts.Description = "You need to login to continue"
		msg = "unauthorized: authentication required (OAuth)"
	} else {
		opts.Description = "The access token lacks the required scope"
	}
	res := mcp.NewToolResultError(msg)
	res.Meta = mcp.NewMetaFromMap(map[string]any{
		"mcp/www_authenticate": []string{as.Challenge(opts)},
	})
	return res
}

// oauthToolFilter 는 tools/list 를 호출자별로 다듬는다.
//   - 읽기 도구는 인증 없는 세션에도 보이고, _meta.securitySchemes 로 oauth2 + 스코프를
//     선언한다(mcp-go v0.54.1 의 Tool 에는 최상위 securitySchemes 필드가 없어 _meta 로만
//     실을 수 있다). ChatGPT 는 이걸 보고 도구 호출 전에 로그인을 안내한다.
//   - 쓰기 도구(add_note)는 해당 스코프가 없는 세션(OAuth·인증 없음)에서 목록에서
//     뺀다. 정적 API 키 세션에는 그대로 보인다.
func oauthToolFilter() server.ToolFilterFunc {
	return func(ctx context.Context, tools []mcp.Tool) []mcp.Tool {
		p, _ := mcpauth.PrincipalFrom(ctx)
		out := make([]mcp.Tool, 0, len(tools))
		for _, t := range tools {
			need := requiredScope(t.Name)
			if !mcpauth.Supports(need) {
				if !p.HasScope(need) {
					continue
				}
			} else {
				t = withOAuthScheme(t, need)
			}
			out = append(out, t)
		}
		return out
	}
}

// withOAuthScheme 는 원본 Tool 을 건드리지 않고 securitySchemes 를 더한 사본을
// 돌려준다(Meta 는 포인터라 공유 상태를 바꾸면 안 된다).
func withOAuthScheme(t mcp.Tool, scope string) mcp.Tool {
	fields := map[string]any{}
	var progress mcp.ProgressToken
	if t.Meta != nil {
		for k, v := range t.Meta.AdditionalFields {
			fields[k] = v
		}
		progress = t.Meta.ProgressToken
	}
	fields["securitySchemes"] = []map[string]any{
		{"type": "oauth2", "scopes": []string{scope}},
	}
	t.Meta = &mcp.Meta{ProgressToken: progress, AdditionalFields: fields}
	return t
}

// oauthServerOptions 는 OAuth 가 켜졌을 때 MCPServer 에 더하는 옵션이다.
func oauthServerOptions(as *mcpauth.Server) []server.ServerOption {
	return []server.ServerOption{
		server.WithToolHandlerMiddleware(oauthToolMiddleware(as)),
		server.WithToolFilter(oauthToolFilter()),
	}
}

// buildMCPHTTP 는 MCP HTTP 핸들러를 조립한다. main 과 테스트가 같은 함수를 써서
// 배선이 어긋나면 테스트가 잡는다.
//
// as == nil(OAuth 꺼짐)이면 기존 조립과 같다: 정적 키 컨텍스트 함수 + 본문 상한,
// 게이트 없음. as != nil 이면 혼합 모드다: Authorization 헤더가 없는 요청은 통과하고
// (인증이 필요한 도구 호출은 도구 계층이 거절), 헤더가 있는데 유효하지 않으면 본문을
// 읽기 전에 401 invalid_token 으로 막는다. OAuth 엔드포인트·메타데이터도 같은 포트에서 서빙한다.
func buildMCPHTTP(s *server.MCPServer, apiKey string, as *mcpauth.Server, httpSrv *http.Server) (*server.StreamableHTTPServer, http.Handler) {
	return buildMCPHTTPWithTTL(s, apiKey, as, httpSrv, oauthSessionIdleTTL)
}

// oauthSessionIdleTTL 은 OAuth 가 켜졌을 때 유휴 세션을 정리하는 시한이다. 혼합 모드는
// 인증 없는 initialize 를 허용하므로 세션 상태(도구·로그 수준 저장소, 등록된 세션)가
// 무한히 쌓일 수 있다. mcp-go 의 sweeper 가 이 시간 동안 요청이 없는 세션의 전송 상태를
// 지운다. 기본 세션 ID 관리자는 ID 를 검증하지 않는 stateless-generating 방식이라(요청마다
// 세션 ID 를 다시 받아들이므로) 정리된 세션의 클라이언트는 다음 요청에서 새 상태로 이어진다.
const oauthSessionIdleTTL = 30 * time.Minute

func buildMCPHTTPWithTTL(s *server.MCPServer, apiKey string, as *mcpauth.Server, httpSrv *http.Server, idleTTL time.Duration) (*server.StreamableHTTPServer, http.Handler) {
	ctxFunc := mcpAuthContextFunc(apiKey)
	opts := []server.StreamableHTTPOption{
		server.WithStreamableHTTPServer(httpSrv),
	}
	if as != nil {
		ctxFunc = oauthAuthContextFunc(as, apiKey)
		opts = append(opts, server.WithSessionIdleTTL(idleTTL))
	}
	opts = append(opts, server.WithHTTPContextFunc(ctxFunc))
	mcpHTTP := server.NewStreamableHTTPServer(s, opts...)

	var inner http.Handler = mcpHTTP
	if as != nil {
		// 본문 상한(newMCPHTTPHandler) 안쪽에서 tools/list 응답을 다듬는다.
		inner = topLevelSecuritySchemes(inner)
	}
	handler := newMCPHTTPHandler(inner)
	if as != nil {
		handler = as.Handler(apiKey, handler)
	}
	return mcpHTTP, handler
}
