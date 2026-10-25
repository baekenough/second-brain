# MCP OAuth: OpenAI dots / ChatGPT 연결

MCP 서버(`cmd/mcp`, 포트 8090)에 OAuth 2.1 인가를 켜서 ChatGPT의 커스텀 MCP 서버로 읽기 전용 연결을 만드는 절차입니다. dots는 사용자가 ChatGPT에 설치한 플러그인과 그 권한을 그대로 쓰므로, ChatGPT에 커스텀 MCP 서버를 플러그인으로 만들어 설치하면 dots에서 쓸 수 있습니다. 구현은 `internal/mcpauth`, 리프레시 토큰 저장소는 `internal/store/mcp_oauth.go`(마이그레이션 `042`)입니다.

## 동작 요약

| 항목 | 내용 |
| --- | --- |
| 인증 주체 | 소유자 한 명. 동의 화면에 소유자 키를 한 번 붙여 넣는다. 계정·비밀번호 없음 |
| 클라이언트 등록 | **CIMD(권장)**: `client_id` 가 ChatGPT가 호스팅하는 메타데이터 문서 URL. **미리 등록된 클라이언트(대안)**: 고정 `client_id`, 비밀 없음, auth method `none`. DCR은 지원하지 않음 |
| 스코프 | `brain:read` 하나. `search`, `get_document`, `stats`만 허용. `add_note`는 OAuth 토큰을 거절하며 정적 `API_KEY`로만 동작(OAuth·인증 없음 세션의 `tools/list`에서는 숨김) |
| 액세스 토큰 | HS256 JWT, 1시간 |
| 리프레시 토큰 | 불투명 값, Postgres에 해시로 저장, 30일, 사용할 때마다 회전, 재사용 시 계보 전체 폐기 |
| 정적 `API_KEY` Bearer | 전권. hermes 에이전트는 2026-10-25 폐기됐고 Tailscale 바인딩도 제거됨. 같은 키가 `/api/v1/*`(폰 ingest, web)도 보호하고 OAuth 기동에도 필요해서 남아 있으며, 이번 폐기로 교체하지 않는다 |
| OAuth 꺼짐 | 환경 변수를 모두 비우면 지금과 동일. OAuth 엔드포인트도 열리지 않음 |

### 혼합 모드 (OAuth 또는 인증 없음)

ChatGPT의 등록 화면은 인증 방식으로 "OAuth", "인증 없음", "OAuth 또는 인증 없음"(혼합)을 제공하고, 이 서버는 OAuth와 혼합 둘 다에서 동작합니다.

| 요청 | 결과 |
| --- | --- |
| `Authorization` 헤더 없음, `initialize` / `tools/list` | 통과. 읽기 도구가 `securitySchemes = [{type:"oauth2", scopes:["brain:read"]}]` 와 함께 보임. 쓰기 도구는 숨김 |
| `Authorization` 헤더 없음, 도구 호출 | HTTP 200 + 도구 오류(`isError: true`) + `_meta["mcp/www_authenticate"] = ['Bearer resource_metadata="…", error="insufficient_scope", error_description="You need to login to continue", scope="brain:read"']`. ChatGPT가 이걸 보고 로그인을 시작함 |
| 유효한 OAuth 토큰 | 읽기 도구 사용 가능. 쓰기 도구 호출은 도구 오류(재인가로 해결되지 않으므로 도전 없음) |
| 정적 `API_KEY` | 전권(기존과 동일) |
| 헤더가 있는데 정적 키도 유효한 JWT도 아님(만료·위조·다른 aud 포함) | HTTP 401 + `WWW-Authenticate: Bearer resource_metadata="…", error="invalid_token"`. ChatGPT가 토큰을 갱신함 |

`securitySchemes` 는 공식 문서대로 도구 기술자의 **최상위 필드**(`inputSchema`·`annotations` 와 같은 층)입니다. mcp-go v0.54.1 은 이 필드를 직렬화할 수 없어서, 도구 필터가 `_meta.securitySchemes` 에 실어 둔 값을 HTTP 계층(`cmd/mcp/securityschemes.go`)이 `tools/list` 응답(`application/json` 본문과 `text/event-stream` 의 `data:` 줄 모두)에서 최상위로 복사합니다. `_meta` 사본은 호환용으로 남깁니다. 다른 요청·응답은 한 바이트도 바꾸지 않습니다.

MCP Events(프로토콜 2026-07-28의 이벤트 구독)는 구현하지 않습니다. 전송은 streamable HTTP(`POST /mcp`)와 SSE(`GET /mcp/sse`)입니다.

엔드포인트(모두 `MCP_PUBLIC_BASE_URL` 기준, 같은 포트):

| 경로 | 용도 |
| --- | --- |
| `GET /.well-known/oauth-protected-resource` , `…/mcp` | 보호 리소스 메타데이터(RFC 9728). `scopes_supported: ["brain:read"]` |
| `GET /.well-known/oauth-authorization-server` | 인가 서버 메타데이터(RFC 8414). `client_id_metadata_document_supported: true` |
| `GET /.well-known/openid-configuration` | 위와 같은 문서(OIDC 기능은 광고하지 않음) |
| `GET/POST /authorize` | 동의 화면(소유자 키 입력)과 코드 발급. 모든 응답 리다이렉트(오류 포함)에 `iss`(RFC 9207), 값은 메타데이터의 `issuer` 와 글자까지 같음 |
| `POST /token` | `authorization_code`, `refresh_token` |

`authorization_response_iss_parameter_supported: true` 를 광고하므로 ChatGPT는 안정적인 공용 콜백(`https://chatgpt.com/connector_platform_oauth_redirect`)을 씁니다. 이것이 `MCP_OAUTH_REDIRECT_URIS` 의 기본값입니다.

## 1. 키 만들기

소유자 키는 동의 화면에서 붙여 넣는 긴 무작위 비밀입니다. 서버에는 SHA-256만 저장합니다. 키 자체는 비밀번호 관리자에 보관하세요.

```bash
# 소유자 키(출력된 값을 비밀번호 관리자에 저장)
KEY=$(openssl rand -base64 32); echo "$KEY"

# 서버에 넣을 SHA-256 (hex 64자). 끝 개행이 들어가지 않게 printf 를 쓴다
printf %s "$KEY" | shasum -a 256 | cut -d' ' -f1

# JWT 서명 키(32바이트 이상)
openssl rand -base64 48
```

## 2. 환경 변수

`.env.local`(ubuntu1 의 compose 가 읽는 파일)에 설정합니다. 예시와 설명은 `.env.local.example` 에 있습니다.

| 변수 | 필수 | 설명 |
| --- | --- | --- |
| `MCP_PUBLIC_BASE_URL` | 예 | `https://sb.baekenough.com` (경로 없이 origin만) |
| `MCP_OAUTH_OWNER_KEY_SHA256` | 예 | 1단계의 SHA-256 hex |
| `MCP_OAUTH_SIGNING_KEY` | 예 | 1단계의 서명 키. 32바이트 이상. base64/hex 처럼 보이면 **디코딩한 길이**가 32바이트 이상이어야 함(32자 base64 는 24바이트라 거절). `openssl rand -base64 48` 로 만들 것. base64/hex 가 아니면 엔트로피를 확인할 수 없다는 경고를 로그에 남김 |
| `API_KEY` | 예 | 이미 있는 정적 키. OAuth를 켜면서 비어 있으면 기동을 거부함 |
| `MCP_OAUTH_REDIRECT_URIS` | 아니오 | 미리 등록된 클라이언트의 허용 콜백(쉼표 구분, 완전 일치). 기본 `https://chatgpt.com/connector_platform_oauth_redirect`. 값을 주면 기본값을 **대체**하므로 함께 쓰려면 목록에 같이 적음 |
| `MCP_OAUTH_CLIENT_ID` | 아니오 | 미리 등록된 클라이언트의 ID. 기본 `dots` |
| `MCP_OAUTH_CIMD_CLIENT_ID_PATTERNS` | 아니오 | 받아들일 CIMD `client_id` URL 패턴. 기본 `https://chatgpt.com/oauth/client.json`, `https://chatgpt.com/oauth/{id}/client.json`. `off` 면 CIMD 끔 |
| `MCP_OAUTH_CIMD_REDIRECT_HOSTS` | 아니오 | CIMD 문서의 `redirect_uris` 가 가질 수 있는 호스트(https만). 기본 `chatgpt.com` |
| `MCP_OAUTH_RESOURCE` | 아니오 | 기본 `MCP_PUBLIC_BASE_URL` + `/mcp` |
| `MCP_OAUTH_TRUSTED_PROXIES` | 아니오 | `CF-Connecting-IP` 를 믿을 대역. 기본 `127.0.0.0/8`, `::1`, `172.16.0.0/12` |

필수 변수 중 일부만 설정하면 `mcp` 바이너리가 어떤 변수가 빠졌는지 알려 주며 기동을 거부합니다(조용히 약한 인증으로 뜨지 않게 하려는 의도). 오류 문구에는 값이 담기지 않습니다.

적용(ubuntu1 운영): 이미지는 따로 빌드해 `second-brain-mcp:ubuntu1` 로 두고, 운영 compose 파일은 리포가 아니라 ubuntu1 에 있는 `docker-compose.ubuntu1.yml` 입니다(`docker-compose.local.yml --build` 가 아닙니다).

```bash
# 1) server 를 먼저 기동/배포한다. 마이그레이션 042(mcp_oauth_refresh_tokens)는 server 가
#    시작할 때 적용하고, mcp 는 마이그레이션을 돌리지 않는다. 테이블이 없으면 mcp 의 /token 이
#    server_error 를 돌려준다.
# 2) 그다음 mcp 를 다시 만든다.
docker compose --env-file .env.local -f docker-compose.ubuntu1.yml up -d mcp
docker compose --env-file .env.local -f docker-compose.ubuntu1.yml logs mcp | grep -E 'MCP OAuth enabled|startup failed|mcp oauth'
```

기동 로그에 `MCP OAuth enabled` 가 보여야 합니다(issuer·resource·client_id만 출력되고 비밀은 출력되지 않음). 서명 키 엔트로피 경고가 있으면 함께 출력됩니다. 확인:

```bash
curl -s https://sb.baekenough.com/.well-known/oauth-protected-resource/mcp
curl -s https://sb.baekenough.com/.well-known/oauth-authorization-server
# 잘못된 토큰은 401 + invalid_token
curl -si https://sb.baekenough.com/mcp -X POST -H 'authorization: Bearer x' -H 'content-type: application/json' -d '{}' | head -5
```

Cloudflare 쪽 설정(`sb.baekenough.com` 은 웹 UI 와 같은 호스트이고 Cloudflare Access 뒤에 있다):

1. 터널 ingress: `deploy/ubuntu1-stack/cloudflared/config-second-brain.yml` 처럼 `sb.baekenough.com` 의 `path` 규칙으로 `/mcp`, `/authorize`, `/token`, `/.well-known/oauth-*`, `/.well-known/openid-configuration` 만 `localhost:8090` 으로 보낸다(웹 규칙보다 위). 운영 파일은 `~/.cloudflared/config-second-brain.yml`, 적용은 `cloudflared tunnel --config <파일> ingress validate` 후 `sudo systemctl restart cloudflared-second-brain` (SIGHUP 금지 — 터널이 종료된다).
2. Cloudflare Access: Zero Trust → Access → Applications 에 self-hosted 앱을 하나 더 만들고 목적지로 `sb.baekenough.com/mcp`, `sb.baekenough.com/authorize`, `sb.baekenough.com/token`, `sb.baekenough.com/.well-known` 을 넣는다. 정책은 Action **Bypass**, Include **Everyone**. 더 구체적인 경로 앱이 호스트 전체 앱보다 우선하므로 나머지 웹 UI 는 계속 Google 로그인 뒤에 있다. ChatGPT 서버가 이 경로들을 직접 호출하므로 봇 차단(Bot Fight Mode 등)도 이 경로에는 걸리면 안 된다.
3. 서버가 `chatgpt.com` 의 CIMD 문서를 가져오므로 mcp 컨테이너에서 `chatgpt.com:443` 으로 나가는 연결이 열려 있어야 한다(ubuntu1 app 네트워크에서 200 확인됨).

네트워크 사실(ubuntu1): docker 네트워크는 app `172.24.0.0/16`, postgres `172.22.0.0/16` 이고 둘 다 기본 신뢰 프록시 대역 `172.16.0.0/12` 안입니다. 그래서 cloudflared → docker-proxy 경로의 연결 주소가 신뢰 대역으로 보이고 `CF-Connecting-IP` 가 클라이언트 IP로 쓰입니다. Tailnet 피어도 docker-proxy 를 거쳐 들어오므로 신뢰 프록시처럼 보입니다(= 그들이 `CF-Connecting-IP` 를 위조해 IP별 제한을 비껴갈 수 있음). Tailnet 기기는 모두 본인 소유라 허용 가능한 한계이며, 이 헤더는 속도 제한 키에만 쓰이고 인증에는 쓰이지 않습니다.

## 3. ChatGPT에서 연결 (dots 포함)

1. `chatgpt.com/plugins` 로 이동해 **+** → **Add custom MCP server** 를 누릅니다.
2. 아래 값을 입력합니다. 화면 라벨은 바뀔 수 있으니 의미가 같은 칸에 넣으세요.

공통:

| 칸 | 값 |
| --- | --- |
| 서버 URL | `https://sb.baekenough.com/mcp` |
| 인증 | **OAuth** (`OAuth 또는 인증 없음`도 동작하지만 이 서버는 인증 없이 쓸 수 있는 도구가 없으므로 OAuth를 권장) |

### 옵션 A. CIMD (권장)

클라이언트 등록 방식으로 **Client ID Metadata Document(CIMD)** 를 고릅니다. 서버 URL만 있으면 되고 콜백 URL, client_id, 시크릿을 입력하지 않습니다. 서버가 `client_id_metadata_document_supported: true` 를 광고하므로 이 선택지가 활성화됩니다. ChatGPT가 `https://chatgpt.com/oauth/…/client.json` 형태의 `client_id` 로 인가를 시작하고, 서버는 그 문서를 가져와(허용 패턴·공개 IP·HTTPS·리다이렉트 불가·크기 제한 검증) `redirect_uris` 를 얻습니다.

### 옵션 B. 미리 등록된 클라이언트 (대안)

정적 자격 증명을 입력하면 ChatGPT는 CIMD보다 이것을 우선 사용합니다. CIMD 선택지가 보이지 않을 때 씁니다.

| 칸 | 값 |
| --- | --- |
| Client ID | `dots` (`MCP_OAUTH_CLIENT_ID` 와 같은 값) |
| Client secret | 비움 |
| Token endpoint auth method | `none` |
| Authorization URL | `https://sb.baekenough.com/authorize` |
| Token URL | `https://sb.baekenough.com/token` |
| Scope | `brain:read` |

콜백 URL은 기본값 `https://chatgpt.com/connector_platform_oauth_redirect` 가 이미 허용되어 있습니다. 등록 화면이 다른 콜백(예: `https://chatgpt.com/connector/oauth/{callback_id}`)을 보여 주면 그 값을 `MCP_OAUTH_REDIRECT_URIS` 에 추가하고(기본값도 쓰려면 둘 다 쉼표로 적음) mcp 컨테이너를 다시 만듭니다. 와일드카드는 지원하지 않습니다.

### 연결 마무리

3. **Create as a plugin** 을 눌러 플러그인으로 만들고 **설치(install)** 합니다. ChatGPT 서버가 로그인을 시작해 이 서버의 동의 화면(`/authorize`)이 열립니다.
4. **내가 방금 시작한 연결에서만** 동의 화면을 엽니다. 소유자 키가 이 서버의 유일한 방어선이라, 누군가 보낸 링크나 예상치 못한 팝업의 동의 화면에는 절대 키를 입력하지 마세요(동의 화면의 URL이 `https://sb.baekenough.com/authorize` 인지 확인). 동의 화면에서 **소유자 키**(1단계에서 만든 키 원문)를 붙여 넣고 **승인**합니다. 요청 스코프는 `brain:read`(읽기 전용)로 표시됩니다. 키가 틀리면 다시 입력하고, 한도(아래)에 닿으면 잠시 잠깁니다.
5. 설치된 플러그인은 dots에서 사용할 수 있습니다(dots는 사용자가 설치한 플러그인과 그 권한을 그대로 씁니다).

스코프: ChatGPT는 기본으로 OIDC 스코프(`openid`, `email` 등)도 요청합니다. 이 서버는 OIDC를 구현하지 않으므로 `brain:read` 만 광고하고(PRM·AS 메타데이터의 `scopes_supported`), 요청에 섞인 알 수 없는 스코프는 거절하지 않고 조용히 버립니다. 그래서 연결이 `brain:read` 로 성공합니다. 남는 스코프가 없으면 `brain:read` 를 발급하며, 실제 발급 스코프는 동의 화면과 토큰 응답의 `scope` 에 나옵니다. 쓰기 스코프는 요청해도 얻을 수 없습니다.

## 4. 폐기

| 목적 | 방법 |
| --- | --- |
| 모든 리프레시 토큰 폐기(연결 끊기) | `UPDATE mcp_oauth_refresh_tokens SET revoked_at = now() WHERE revoked_at IS NULL;` |
| 한 계보만 폐기 | `UPDATE mcp_oauth_refresh_tokens SET revoked_at = now() WHERE family_id = '<uuid>' AND revoked_at IS NULL;` |
| 이미 발급된 액세스 토큰까지 즉시 무효화 | `MCP_OAUTH_SIGNING_KEY` 를 새 값으로 바꾸고 mcp 재생성(액세스 토큰은 서버에 저장되지 않으므로 이 방법뿐. 그렇지 않으면 최대 1시간 유효) |
| 소유자 키 교체 | 새 키의 SHA-256을 `MCP_OAUTH_OWNER_KEY_SHA256` 에 넣고 mcp 재생성. 기존 연결은 위의 폐기와 함께 정리 |
| CIMD 끄기 | `MCP_OAUTH_CIMD_CLIENT_ID_PATTERNS=off` |
| OAuth 전체 끄기 | `MCP_*` OAuth 변수를 모두 비우고 mcp 재생성. 정적 `API_KEY` 경로만 남음 |

`mcp_oauth_refresh_tokens` 는 `token_hash`(SHA-256)만 저장하므로 DB를 열어 봐도 토큰 원문은 알 수 없습니다. 만료된 지 하루가 지난 행은 새 토큰을 발급할 때마다 지워집니다.

## 5. 보안 메모

- **소유자 키 시도 제한**: 클라이언트 IP당 15분에 5회, 전체 합산 500회. 한도에 닿으면 올바른 키도 평가하지 않고 `429` 를 줍니다. 소유자 키는 무작위 32바이트라 무차별 대입은 IP별 제한만으로도 무의미합니다. 전체 상한은 여러 IP에서 오는 대규모 공격(비용·로그 폭주)만 막으려는 것이라 높게 두었습니다. 낮게(예: 20) 두면 IP 4개가 15분마다 5회씩만 실패해도 소유자가 계속 잠기는 DoS가 되므로, 이 상한은 일부러 높습니다(트레이드오프: 대규모 분산 공격은 500회까지 통과). cloudflared 뒤에서는 연결 주소가 신뢰 대역일 때만 `CF-Connecting-IP` 를 씁니다(`X-Forwarded-For` 는 보지 않음). 거부(deny)는 키 추측이 아니므로 제한·nonce를 쓰지 않습니다.
- **인증 없는 요청**: Authorization 헤더가 없는 요청(혼합 모드)과 OAuth 토큰 요청의 본문은 1MB로 제한됩니다(정적 키는 `add_note` 때문에 기존 상한 유지). 인증 없는 `initialize` 가 만드는 세션 상태는 30분 유휴 후 정리됩니다.
- **인가 코드**: 무작위, 일회용, 60초, `client_id`·`redirect_uri`·PKCE 챌린지·스코프·`resource` 에 묶임. 같은 코드를 다시 내면 그 코드로 발급한 토큰 계보를 폐기합니다.
- **PKCE**: S256만. `plain`/생략은 거절.
- **CIMD 안전장치**: `client_id` 는 허용 패턴과 글자 그대로 맞아야 가져옵니다. 연결 시점에 실제 접속 IP가 공개 주소인지 검사(루프백·사설·링크로컬·메타데이터 주소·CGNAT 등 차단, DNS 재바인딩 방지), 프록시 미사용, HTTPS만, 리다이렉트 불가, 5초 타임아웃, 16KB 상한, `application/json` 만 허용. 문서의 `client_id` 는 URL과 같아야 하고, `redirect_uris` 는 모두 허용 호스트의 https여야 합니다. 인증 방식은 `none` 만 씁니다. 실제 ChatGPT 문서는 `token_endpoint_auth_method: "private_key_jwt"` 에 `token_endpoint_auth_methods_supported: ["none","private_key_jwt"]` 를 함께 싣는데, ChatGPT 는 자기 목록과 서버가 광고한 방식(`none` 뿐)의 교집합에서 고르므로, 목록에 `none` 이 있거나 단수 필드가 `none`/생략이면 받아들이고 `jwks`·`jwks_uri`·`token_endpoint_auth_signing_alg` 는 무시합니다. `client_secret` 필드는 여전히 거절합니다. `/token` 에서는 `client_assertion` 을 보내면 `invalid_client` 로 거절합니다. `/token` 은 CIMD 문서를 다시 가져오지 않고(인가 시점에 확인한 `client_id` 가 코드·리프레시 계보에 묶여 있음) 문서 서버 장애가 갱신을 막지 않습니다. 가져오기는 IP당 분당 6회·전체 분당 30회로 제한되고, 부정 결과가 성공 캐시를 밀어내지 못하며, 실패 시 마지막 성공값을 24시간까지 씁니다. 성공은 10분, 실패는 1분 캐시하고, 문서 서버 장애 시 마지막 성공값을 24시간까지 씁니다.
- **로그**: 소유자 키, 코드, 토큰, 서명 키는 로그에 남지 않습니다(테스트로 검증).
- **소유자 키가 유일한 방어선**: 동의 화면은 키만 확인합니다. 공격자가 허용된 콜백(ChatGPT)으로 자기가 시작한 흐름의 링크를 건네 키를 입력하게 만들 수 있으므로, 직접 시작한 연결에서만 키를 입력하세요. 첫 연결이 끝난 뒤에는 `MCP_OAUTH_REDIRECT_URIS` 를 실제로 쓰인 콜백 하나로 고정하고, CIMD 를 안 쓰면 `MCP_OAUTH_CIMD_CLIENT_ID_PATTERNS=off` 로 꺼서 허용 범위를 좁히는 것을 권장합니다.
- **리프레시 계보 직렬화**: 회전과 폐기는 계보 단위 Postgres advisory lock 아래에서 실행됩니다. 재사용 탐지로 계보를 폐기하는 동시에 최신 토큰을 회전하는 공격자의 자식 토큰도 함께 폐기됩니다(READ COMMITTED 경합 방지). 인가 코드를 발급 도중 재사용하면 토큰을 내주지 않고 계보를 폐기합니다.
- **알려진 한계**: 같은 리프레시 토큰으로 동시에 두 번 갱신하거나 응답이 유실된 뒤 재시도하면 재사용으로 판정되어 계보가 폐기됩니다. 이 경우 ChatGPT에서 다시 연결하면 됩니다. 인증 없는 `GET`(SSE 스트림 열기)은 OAuth 도입 전과 같이 세션을 만들 수 있습니다(도구 호출은 인증 필요).
