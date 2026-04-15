---
name: second-brain-architecture-direction
description: second-brain 프로젝트 핵심 아키텍처 방향 결정 — CLI Proxy API = curation layer, Backend = storage warehouse, 사용자 = AI agent, REST=primary/GraphQL=secondary API 전략
type: project
---

second-brain 프로젝트의 컴포넌트별 역할과 전체 아키텍처 철학에 대한 확정된 방향.

**Why:** 컴포넌트 역할이 모호해지면 기능이 잘못된 레이어에 구현되는 경향이 있었음. 이 결정은 코드 배치, API 설계, 기능 확장 시 "어디에 넣어야 하나"를 명확히 하기 위한 기준선.

**How to apply:** 새 기능을 설계하거나 기존 코드 구조를 결정할 때, 아래 역할 정의에 맞는 레이어에 배치. 경계를 넘는 구현(예: backend에 curation 로직 추가)은 명시적 논의 없이는 금지.

## 아키텍처 철학

**LLM-curated private search engine** — LLM이 큐레이션하는 프라이빗 검색 엔진.

사용자는 일반 인간이 아닌 **AI Agent**. API를 통해 큐레이션된 데이터에 접근하는 소비자.

## 컴포넌트 역할

### CLI Proxy API (curation layer)

- **역할**: 지식을 큐레이션·정리·서빙하는 레이어
- **하는 일**: 데이터 큐레이션, 정보 조직화, 검색 결과 서빙, 컨텍스트 조합
- **하지 않는 일**: 봇 기능 없음 (Discord/Slack 명령 처리 금지), 원시 데이터 저장 금지
- **API 스타일**: REST 선호 (GraphQL보다 단순성 우선)

### Backend (storage/logistics warehouse)

- **역할**: 저장 및 물류 창고
- **하는 일**: 인덱싱, 저장, CRUD 오퍼레이션
- **하지 않는 일**: 큐레이션 로직 없음, 비즈니스 결정 없음

## API 설계 원칙

- AI agent가 소비하는 API — 인간 친화적 UI보다 기계 친화적 스키마 우선
- 엔드포인트 시맨틱스: 큐레이션된 결과를 반환하는 검색 API가 핵심

## API 전략 결정 (2026-04-15)

**REST = primary interface, GraphQL = secondary/sub interface.**

| 인터페이스 | 역할 | 사용 시나리오 |
|-----------|------|--------------|
| REST | 기본 API | 단순 요청, 캐시 가능, AI agent 친화적 |
| GraphQL | 보조 API | 복잡한 쿼리 — 관련 문서 + 메타데이터 한 번에 조회, 필드 선택 필요 시 |

**핵심 원칙**: Backend service layer는 REST/GraphQL 양쪽이 공유. 차이는 routing layer만.

**Why:** REST는 단순성·캐시 가능성·AI agent 친화성에서 우위. GraphQL은 "관련 문서 + 메타데이터"처럼 필드 선택이 필요한 복합 쿼리에서만 추가 가치가 있음. service layer 공유로 중복 구현 없이 두 인터페이스 제공 가능.

**How to apply:** 새 엔드포인트 설계 시 REST로 시작. 클라이언트가 동일 요청에서 여러 관련 리소스와 특정 필드만 조합해야 할 때 GraphQL 추가 검토. service layer에 비즈니스 로직 집중, REST/GraphQL router는 얇게 유지.

## Curation API 리팩터 구현 현황 (2026-04-15)

| 항목 | 상태 | 비고 |
|------|------|------|
| Binary separation: `cmd/server` (API) + `cmd/collector` (daemon) | **DONE** | 단일 바이너리에서 분리 완료 |
| LLM curation layer (`internal/curation/`) | **DONE** | 큐레이션 로직 독립 패키지 |
| pg_bigm Korean 2-gram search | **DONE** | PostgreSQL 한국어 전문 검색 |
| `GET /api/v1/search` endpoint | **DONE** | REST primary API 핵심 엔드포인트 |
| Docker multi-target build | **DONE** | server/collector 각각 이미지 분리 |
| Discord bot response 제거 (server에서) | **DONE** | bot 기능은 collector 쪽으로 격리 |
| Default port 변경 9200 → 8080 | **DONE** | |
| GraphQL secondary API | **TODO** | 미구현, 설계만 확정 |
| Telegram/Notion collectors | **TODO** | stub만 존재 |
