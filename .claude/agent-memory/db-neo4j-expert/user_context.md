---
name: user-context
description: 한국어로 지시하는 위임형 사용자 — 프로덕션은 macmini docker-compose, 백엔드 Go / 프런트 Next.js(bun), 개인 데이터·원격 LLM 정책이 엄격하다
metadata:
  type: user
---

- 사용자는 모든 파일 작업을 에이전트에 위임하고 한국어로 지시한다. 응답도 한국어, 파일 내용은 영어/한국어 혼용(이 프로젝트 문서는 한국어).
- 프로덕션은 **macmini의 `docker compose -f docker-compose.local.yml --env-file .env.local`** 이다. k8s 아님. `--env-file` 누락이 실제 장애를 낸 적이 있어 명령마다 붙여 쓴다.
- 스택: Go 백엔드(`cmd/server`, `cmd/collector`), Next.js 16 + React 19 프런트, 패키지 매니저는 **bun**(npm 쓰면 CI가 깨진다). PostgreSQL 16(pgvector, pg_bigm)이 진실의 원천.
- LLM은 **원격 API만**. ollama 등 로컬 추론은 정책상 금지 — Neo4j의 로컬 text2cypher 모델류를 제안하지 않는다.
- 웹은 Cloudflare Access JWT 뒤에 있고 `web/src/proxy.ts`가 인증을 처리한다. 새 인증 계층을 만들지 않는다.
- 개인 데이터(실명·전화번호·문서 원문)를 출력·로그·테스트 픽스처에 넣지 않는다. 예시가 필요하면 가공 더미(`더미갑` 등)를 쓴다.
- 문서 작성 전용 작업에서는 SSH 배포·git 커밋을 하지 않는다. 범위를 명시적으로 좁혀 지시하는 편이다.

관련: [[plan-size-and-context-budget]], [[neo4j-placement-macmini]]
