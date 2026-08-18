# 지식 그래프 투영·탐색 화면 구현 계획 (Part B)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** PostgreSQL의 `entity_relations`/`document_entities`를 Neo4j로 멱등 투영하고, 진입점 선택 → 1홉 확장 방식의 그래프 탐색 화면을 웹에 붙인다.

**Architecture:** PostgreSQL이 진실의 원천이고 Neo4j는 언제든 버리고 다시 만들 수 있는 파생 투영이다. collector 안의 `GraphProjectionWorker`가 기존 `ExtractionWorker`와 같은 패턴(ticker + 직렬 배치 + env 플래그 게이팅)으로 Postgres를 읽어 Cypher `MERGE`로 반영한다. server는 **코드에 박힌 파라미터화 고정 쿼리 카탈로그**만 노출하고, 프런트는 그 4개 엔드포인트만 호출한다. 투영 워커가 Neo4j의 유일한 writer다.

**Tech Stack:** Go 1.26 / `neo4j-go-driver/v5` / Neo4j 5.26 Community (docker-compose) / PostgreSQL 16 / Next.js 16 + React 19 + Tailwind 4 / bun

**Spec:** `docs/superpowers/specs/2026-08-17-knowledge-graph-actions-feedback-design.md` — §6(Part B)가 대상이고 §5.3 닫힌 어휘 8종, §5.4 스키마, §11 프라이버시가 전제다.

---

## 설계 문서 대비 변경 사항

### 변경 1 — Neo4j는 ubuntu1이 아니라 macmini에 배치한다

설계 문서 §6.1은 "ubuntu1 호스트에 배치"라고 적었고 §10-1이 그 전제로 "PostgreSQL → ubuntu1 이전 선행"을 들었다. **그 선행 조건이 아직 실행되지 않았다** — `docs/superpowers/plans/2026-08-18-postgres-migration-to-ubuntu1.md`가 존재하지만 그 문서 스스로 "planning-only, 어떤 작업도 실행되지 않았음"을 명시하고 실행 전 별도 go/no-go를 요구한다. 즉 Part B는 postgres가 macmini에 있는 상태를 전제로 착수해야 하고, 그 상태에서는 다음 실측 때문에 ubuntu1 배치가 성립하지 않는다.

| 경로 | 결과 |
|---|---|
| macmini **호스트** → ubuntu1 (100.68.237.99) | 도달 가능 |
| macmini **컨테이너** → ubuntu1 | **도달 불가** — macOS Docker Desktop VM에 호스트 Tailscale 라우트가 없다 |
| ubuntu1 → macmini의 5432 / 8080 / 3000 | **전부 closed** — 서비스가 127.0.0.1 바인딩이다 |

양방향이 막혀 있다. ubuntu1에 두려면 Tailscale 사이드카나 SSH 포워딩 배관이 필요한데, **사용자는 postgres 이전 논의에서 동일한 배관을 SPOF라는 이유로 이미 거부했다.** 투영 워커(collector)와 읽기 API(server)가 둘 다 macmini 컨테이너이므로 그 배관은 그래프 기능 전체의 단일 실패점이 된다. 반대 방향의 비용은 없다 — 문서 1,213건에서 나오는 관계 수천 개 규모라 heap 512MB / pagecache 256MB면 충분하다. **결정: macmini의 `docker-compose.local.yml`에 서비스로 추가한다.**

### 변경 2 — 설계 문서가 구현 단계로 미룬 항목(§13) 확정

| 항목 | 확정값 | 근거 |
|---|---|---|
| 렌더링 라이브러리 | `react-force-graph-2d` + `next/dynamic({ssr:false})` | 우산 패키지 `react-force-graph`는 three.js를 끌어온다. Task 11에 **실측 게이트** — `/graph` First Load JS 증가분이 250KB(gzip)를 넘으면 d3-force + SVG로 되돌린다 |
| 진입점 N | 기본 **20**, 최대 50 | 라벨 붙은 force graph가 한 화면에 읽히는 상한. 그 이상은 hairball이고 설계 의도(진입점 → 1홉 확장)에 어긋난다 |
| `confidence` 스케일 / 화면 기본 하한 | 0–1 연속값 / **0.5** | 마이그레이션 021이 `NUMERIC(3,2) CHECK 0..1`로 이미 확정. 0.5는 슬라이더로 0까지 내릴 수 있다 |

### 변경 3 — 설계 문서에 없던 결정 3가지

1. **투영 워터마크를 Postgres가 아니라 Neo4j 안(`(:ProjectionState {id:'singleton'})`)에 둔다.** Postgres에 두면 Neo4j 볼륨만 지웠을 때 워터마크가 살아남아 재구축이 조용히 깨진다. **이 계획은 마이그레이션 파일을 하나도 추가하지 않는다** — 진실의 원천에 되돌릴 것이 남지 않아야 롤백이 §롤백대로 끝난다.
2. **엔티티 노드를 별도 패스로 투영하지 않는다.** 관계 쿼리가 양 끝 엔티티를 JOIN해 가져오고 Cypher가 `MERGE`로 함께 만든다. 별도 패스는 "관계는 왔는데 노드가 아직 없음" 순서 의존을 만들고, 그 관계를 건너뛴 채 워터마크가 전진하면 영구 유실이 된다. 어떤 관계에도 참여하지 않는 고립 엔티티는 화면에서 의미가 없다.
3. **`server`/`collector`에 `depends_on: neo4j`를 걸지 않는다.** Neo4j가 죽어도 수집·검색·`/ask`는 돌아야 한다. 워커는 다음 tick에 재시도하고 API는 503을 반환한다. 이 결정이 §롤백의 "컨테이너 제거만으로 끝"을 성립시킨다.

---

## Global Constraints

- **프로덕션은 macmini의 `docker compose -f docker-compose.local.yml --env-file .env.local ...`이다.** k8s가 아니다. `--env-file` 생략 불가 — 생략하면 `${VAR}` 치환이 기본값으로 떨어져 마운트가 어긋난 전례가 있다.
- **PostgreSQL이 진실의 원천, Neo4j는 재구축 가능한 파생 투영이다.** Neo4j 백업은 요구사항이 아니다. 애플리케이션 경로에서 Neo4j에 직접 쓰지 않는다.
- **관계 타입은 닫힌 어휘 8종**(`communicated_with`, `requested_of`, `committed_to`, `mentions`, `belongs_to`, `scheduled_with`, `about_topic`, `related_to`) — `internal/model/relation.go`에 상수로 존재하며 어휘 밖 값은 `related_to`로 강등된다.
- **엔티티 타입은 4종**: `PERSON`, `ORG`, `CONCEPT`, `OTHER` (`internal/model/entity.go`).
- **LLM은 원격 API만 사용한다(로컬 추론 금지). 이 범위는 LLM을 전혀 호출하지 않는다.** 자연어→Cypher는 범위 밖이다(§프라이버시 6).
- **웹 인증은 `web/src/proxy.ts`의 Cloudflare Access JWT 검증이 처리한다.** 새 인증을 만들지 않는다.
- **웹 패키지 매니저는 bun.** `npm install`은 `package-lock.json`을 만들어 CI(`bun --frozen-lockfile`)를 깬다.
- **Go 1.26.6**, 모듈 `github.com/baekenough/second-brain`. 커밋은 Conventional Commits, 브랜치는 `main`에서 딴 `feature/*`.
- **개인정보**: 테스트 픽스처·커밋 메시지·로그에 실제 개인 데이터를 넣지 않는다. 가공된 더미(`더미갑`, `더미을`)만 쓴다.

---

## 파일 구조

**신규 (Go)**: `internal/graph/client.go`(드라이버 수명주기·세션 헬퍼) · `schema.go`(제약·인덱스 보증) · `reltypes.go`(Cypher 타입 리터럴 화이트리스트) · `projector.go`(MERGE 배치·워터마크·wipe) · `queries.go`(읽기 전용 고정 쿼리 + 상한) / `internal/store/graph_source.go`(투영용 Postgres 읽기) / `internal/model/projection.go`(투영 행 타입) / `internal/worker/graph_projection_worker.go` / `internal/api/graph.go`

**수정 (Go)**: `cmd/collector/main.go`(312행 근처, 워커 배선 + 플래그) · `cmd/server/main.go`(`WithGraph` 호출) · `internal/api/router.go`(옵셔널 필드 + 라우트) · `docker-compose.local.yml` · `.env.local.example` · `go.mod`/`go.sum`

**신규 (웹)**: `web/src/app/graph/page.tsx`(필터 + 진입점) · `GraphCanvas.tsx`(force-graph 래퍼, client-only) · `EvidencePanel.tsx` · `mask.ts` / `web/src/app/api/graph/{entry,expand,evidence,entities}/route.ts`(프록시 4개)

**수정 (웹)**: `web/src/lib/api.ts`, `web/src/lib/types.ts`, `web/package.json`

**마이그레이션 파일 없음.** 의도된 제약이다(변경 3-1).

---

## 그래프 데이터 모델

### 노드

```
(:Entity:Person)  (:Entity:Org)  (:Entity:Topic)  (:Entity:Other)   (:Document)   (:ProjectionState)
```

`entities.type`을 **보조 라벨**로 매핑한다(`CONCEPT` → `Topic`). 설계 문서 §6.2의 근거대로 라벨은 플래너가 직접 쓰는 1급 시민이라 속성 필터보다 저렴하고, 타입이 유한한 닫힌 집합이라 라벨 폭증 위험이 없다. `type` 속성도 **함께** 저장한다 — API 응답에서 라벨 목록을 역산하는 것보다 속성 1개 읽기가 단순하고, 두 값은 같은 소스에서 동시에 쓰여 어긋날 수 없다.

| 노드 | 속성 |
|---|---|
| `:Entity` | `pgId`(int, **UNIQUE**), `name`, `normalizedName`(인덱스), `type` |
| `:Document` | `pgId`(string UUID, **UNIQUE**), `sourceType`, `occurredAt`(nullable). **`title`·`content` 저장 금지** (§프라이버시 1) |
| `:ProjectionState` | `id`(항상 `'singleton'`, **UNIQUE**), `lastRelationId`, `resetToken`, `updatedAt` |

문서를 **노드**로 두는 이유: 한 문서가 여러 관계의 근거이므로(N:M) 관계 속성으로 복제하면 같은 메타데이터가 수천 번 중복된다. 반대로 엔티티 이름은 **속성**으로 둔다 — 이름으로 조인·필터하는 질의가 없고(항상 `pgId`로 진입한다) 노드로 승격하면 홉만 늘어난다.

### 관계

8종을 대문자 스네이크로 매핑하고 `document_entities` 투영인 `MENTIONED_IN` 하나를 더해 9종이다.

```
(:Entity)-[:COMMUNICATED_WITH|REQUESTED_OF|COMMITTED_TO|MENTIONS|BELONGS_TO|SCHEDULED_WITH|ABOUT_TOPIC|RELATED_TO]->(:Entity)
(:Entity)-[:MENTIONED_IN]->(:Document)
```

엔티티 간 8종은 `pgId`(= `entity_relations.id`), `evidencePgId`, `confidence`, `observedAt`을 갖고, `MENTIONED_IN`은 `evidencePgId`만 갖는다.

**Postgres 1행 = 관계 1개.** 같은 쌍·같은 타입이 근거 문서 3개에서 관찰되면 병렬 엣지 3개다. 집계("12번 연락했다")는 질의 시점에 `count(r)`로 한다. 대안(엣지 1개 + 카운터 증가)을 버린 이유는 (1) 카운터 증가는 멱등하지 않아 재실행 시 값이 부풀고, (2) 엣지 1개가 Postgres 1행에 1:1 대응해야 "모든 노드·관계가 Postgres 행으로 역추적된다"는 재구축 보증이 성립하기 때문이다. 병렬 엣지는 이 규모에서 비용이 아니다.

**방향**: 저장은 `from_entity_id → to_entity_id`를 그대로 따라 단방향이다. 탐색은 방향 무관(`-[r]-`)으로 매치하고 응답에 `direction`을 실어 UI가 화살표를 그린다. 양방향 이중 저장을 하지 않는다.

### 제약과 인덱스

```cypher
CREATE CONSTRAINT entity_pg_id       IF NOT EXISTS FOR (e:Entity)          REQUIRE e.pgId IS UNIQUE;
CREATE CONSTRAINT document_pg_id     IF NOT EXISTS FOR (d:Document)        REQUIRE d.pgId IS UNIQUE;
CREATE CONSTRAINT projection_state_id IF NOT EXISTS FOR (p:ProjectionState) REQUIRE p.id  IS UNIQUE;
CREATE INDEX entity_normalized_name  IF NOT EXISTS FOR (e:Entity)          ON (e.normalizedName);
```

유니크 제약은 **`MERGE`의 안전판**이다 — 없으면 동시 `MERGE`가 중복 노드를 만든다. 제약이 백킹 인덱스를 만들므로 `pgId`에 별도 인덱스를 만들지 않는다.

**관계 속성 인덱스는 만들지 않는다.** (1) Community 에디션은 **관계 유니크/존재 제약을 지원하지 않으므로**(Enterprise 기능) 관계 중복 방지를 제약이 아니라 "투영 워커가 유일한 직렬 writer"라는 구조로 보장한다 — 동시 writer가 없으면 `MERGE` 경합 자체가 없다. (2) 모든 조회가 `pgId`로 노드에 진입한 뒤 1홉 확장이라 관계를 전역 스캔하는 질의가 없다. 진입점 쿼리만 예외인데, 관계 수천 개에서는 인덱스보다 스캔이 빠르다. **임계**: 관계 50만 개 초과 또는 기간 전역 질의 도입 시 `observedAt` 관계 인덱스를 추가한다(§미해결 위험).

**슈퍼노드 주의**: 사용자 본인 엔티티와 상용 토픽은 팬아웃이 커진다. 현 규모에서는 문제가 아니지만 **확장 질의는 반드시 `ORDER BY` + `LIMIT`으로 자른다**. 무제한 확장 질의를 만들지 않는다. 가변 길이 경로(`*`)도 쓰지 않는다.

---

## 프라이버시 처리 방침

Part B는 LLM을 호출하지 않으므로 **이 범위에서 개인 데이터가 원격으로 나가는 경로는 없다.**

1. **Neo4j에 원문을 넣지 않는다.** `:Document`는 `pgId`+`sourceType`+`occurredAt`만 갖는다. 근거 열람은 UI가 기존 `/documents/{id}`로 링크하며 그 페이지는 이미 Access + Bearer 인증 뒤에 있다. 결과적으로 **Neo4j 볼륨이 통째로 유출되어도 노출되는 것은 이름과 관계 구조이지 대화 원문이 아니다.**
2. **포트를 공개하지 않는다.** bolt(7687)·browser(7474) 모두 `127.0.0.1` 바인딩. Browser는 인증만 통과하면 그래프 전체를 조회하므로 **cloudflared ingress에 7474 호스트명을 추가하지 않는다.** 필요하면 SSH 로컬 포워딩으로만 접근한다.
3. **화면 표시.** `/graph`는 `proxy.ts`의 Access 검증 뒤에 놓인다(새 인증 코드 없음). 노드 라벨에 실명이 그대로 나오는 것이 이 화면의 목적이다. 다만 화면 공유용 **마스킹 토글**을 둔다 — 켜면 클라이언트에서만 `김민수` → `김**`로 렌더링한다. 서버 응답은 바뀌지 않는다(서버 측 마스킹은 근거 대조를 불가능하게 만든다). 기본 off, 선택은 `localStorage`에 저장.
4. **로그.** 엔티티 `name`/`normalizedName`, 문서 제목·본문, Cypher 파라미터 맵 전체를 **절대 로그에 남기지 않는다.** 허용: `pgId`, 건수, 소요시간, 오류 종류. Task 12에 강제 가드 테스트를 둔다.
5. **Neo4j 자체 로그.** `NEO4J_server_logs_query_enabled: "OFF"` — 켜져 있으면 파라미터의 실명이 `/logs/query.log`에 남는다. Task 1에서 더미 이름으로 실증한다. `/logs`는 볼륨으로 내보내지 않아 컨테이너와 함께 사라진다.
6. **자연어→Cypher는 범위 밖이다.** 향후 붙인다면 읽기 전용 세션, 파라미터화 강제(리터럴 삽입 거부), 실행 전 `EXPLAIN` 게이트, 파괴적 절(`CREATE`/`MERGE`/`SET`/`DELETE`/`REMOVE`/`CALL {...} IN TRANSACTIONS`/`LOAD CSV`/`apoc.*` 쓰기) 정적 차단, 경로 상한·기본 `LIMIT`·트랜잭션 타임아웃이 **전부** 필요하다. 덧붙여 **Community에는 RBAC이 없어 "reader 롤 전용 사용자"라는 방어선을 쓸 수 없다** — 그때는 Enterprise 전환이나 읽기 복제본을 함께 판단해야 한다. 지금은 이 문제를 만들지 않는 쪽을 택했다.

---

## 작업 목록

작업 1–8은 백엔드, 9–12는 프런트다. 각 작업은 독립 검증·리뷰·커밋 가능하다. 배포는 §배포에서 따로 다룬다.

### Task 1: Neo4j 컨테이너 정의

**Files:** Modify `docker-compose.local.yml`(services 마지막 블록 뒤), `.env.local.example`
**Produces:** compose 네트워크 내부 엔드포인트 `bolt://neo4j:7687`, 사용자 `neo4j`, 비밀번호는 `NEO4J_PASSWORD`

- [ ] **Step 1: compose에 서비스와 볼륨 추가**

```yaml
  # neo4j — 지식 그래프 파생 투영. PostgreSQL이 진실의 원천이므로 이 볼륨은
  #   버려도 된다(백업 요구사항 없음, 재구축은 §배포 D6).
  #   포트는 127.0.0.1 에만 바인딩한다 — Browser(7474)는 인증만 통과하면
  #   그래프 전체를 조회하므로 cloudflared ingress에 호스트명을 두지 않는다.
  neo4j:
    image: neo4j:5.26-community
    ports:
      - "127.0.0.1:7687:7687"
      - "127.0.0.1:7474:7474"
    environment:
      # ${VAR} 치환은 env_file이 아니라 --env-file/셸에서 온다. 그래서
      # --env-file 없이 기동하면 아래 :? 가 조용한 오작동 대신 즉시 실패시킨다.
      NEO4J_AUTH: "neo4j/${NEO4J_PASSWORD:?NEO4J_PASSWORD is required}"
      NEO4J_PASSWORD: "${NEO4J_PASSWORD:?NEO4J_PASSWORD is required}"
      NEO4J_server_memory_heap_initial__size: "512m"   # 관계 수천 개 규모.
      NEO4J_server_memory_heap_max__size: "512m"       # 과할당은 다른 컨테이너를
      NEO4J_server_memory_pagecache_size: "256m"       # 밀어낼 뿐이다.
      NEO4J_server_logs_query_enabled: "OFF"           # 파라미터의 실명 유출 방지
    volumes:
      - neo4jdata:/data                                # /logs 는 내보내지 않는다
    healthcheck:
      test: ["CMD-SHELL", "cypher-shell -u neo4j -p \"$${NEO4J_PASSWORD}\" 'RETURN 1' >/dev/null 2>&1 || exit 1"]
      interval: 10s
      timeout: 10s
      retries: 12
      start_period: 40s
    restart: unless-stopped
```

`volumes:` 블록에 `neo4jdata:` 추가(주석: 파생 투영 전용, 삭제해도 재구축된다).

- [ ] **Step 2: `.env.local.example`에 변수 추가** — 값은 비운다.

```
NEO4J_PASSWORD=
NEO4J_URI=bolt://neo4j:7687
NEO4J_USERNAME=neo4j
GRAPH_PROJECTION_ENABLED=false
GRAPH_PROJECTION_INTERVAL=5m
GRAPH_PROJECTION_BATCH_SIZE=500
GRAPH_PROJECTION_RESET_TOKEN=
```

- [ ] **Step 3: compose 문법 검증**
Run: `docker compose -f docker-compose.local.yml --env-file .env.local config >/dev/null && echo OK` → `OK`.
`--env-file`을 뺀 동일 명령이 `NEO4J_PASSWORD is required`로 실패하는 것도 확인한다.

- [ ] **Step 4: 기동과 헬스체크**
Run: `docker compose -f docker-compose.local.yml --env-file .env.local up -d neo4j` → `... ps neo4j`가 60초 내 `healthy`.

- [ ] **Step 5: 포트 바인딩 확인**
Run: `... port neo4j 7687` → `127.0.0.1:7687`. `0.0.0.0`이면 실패로 간주하고 compose를 고친다.

- [ ] **Step 6: 쿼리 로그에 파라미터가 남지 않음을 실증** (더미 이름 사용)

```bash
docker compose -f docker-compose.local.yml --env-file .env.local exec neo4j \
  cypher-shell -u neo4j -p "$NEO4J_PASSWORD" 'CREATE (:Probe {name: $n})' -P 'n => "ZZTESTNAME"'
docker compose -f docker-compose.local.yml --env-file .env.local exec neo4j \
  sh -c 'grep -rl ZZTESTNAME /logs || echo NO_MATCH'   # Expected: NO_MATCH
```

- [ ] **Step 7: 프로브 삭제 후 커밋**
`... exec neo4j cypher-shell -u neo4j -p "$NEO4J_PASSWORD" 'MATCH (p:Probe) DETACH DELETE p'` 실행 후
`git add docker-compose.local.yml .env.local.example && git commit -m "feat(graph): add neo4j container for derived graph projection"`

### Task 2: Neo4j 클라이언트와 스키마 보증

**Files:** Create `internal/graph/client.go`, `internal/graph/schema.go`, `internal/graph/client_test.go`, `internal/graph/schema_integration_test.go`; Modify `go.mod`/`go.sum`
**Produces:**

```go
type Config struct { URI, Username, Password string; MaxPoolSize int; Timeout time.Duration }
func New(ctx context.Context, cfg Config) (*Client, error)
func (c *Client) Close(ctx context.Context) error
func (c *Client) EnsureSchema(ctx context.Context) error
func (c *Client) Read(ctx context.Context, work func(tx neo4j.ManagedTransaction) (any, error)) (any, error)
func (c *Client) Write(ctx context.Context, work func(tx neo4j.ManagedTransaction) (any, error)) (any, error)
```

- [ ] **Step 1: 의존성 추가** — `go get github.com/neo4j/neo4j-go-driver/v5@latest && go mod tidy`

- [ ] **Step 2: 실패하는 테스트 작성** (`client_test.go`)

```go
func TestNew_RejectsEmptyURI(t *testing.T) {
	if _, err := New(context.Background(), Config{Username: "neo4j", Password: "x"}); err == nil {
		t.Fatal("New with empty URI: want error, got nil")
	}
}
func TestNew_RejectsEmptyPassword(t *testing.T) {
	if _, err := New(context.Background(), Config{URI: "bolt://localhost:7687", Username: "neo4j"}); err == nil {
		t.Fatal("New with empty password: want error, got nil")
	}
}
```
Run: `go test ./internal/graph/ -run TestNew -v` → FAIL (`undefined: New`)

- [ ] **Step 3: 클라이언트 구현**

프로세스당 드라이버 1개, 작업 단위마다 짧은 세션, **관리 트랜잭션만** 사용한다(리더 전환·교착 재시도를 드라이버가 처리한다). 세션을 고루틴 간 공유하지 않는다.

```go
const (defaultGraphTimeout = 10 * time.Second; defaultMaxPoolSize = 10; defaultTxTimeout = 5 * time.Second)

func New(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.URI == "" { return nil, errors.New("graph: URI must not be empty") }
	if cfg.Password == "" { return nil, errors.New("graph: password must not be empty") }
	cfg = withDefaults(cfg) // 빈 Username→"neo4j", Timeout<=0→10s, MaxPoolSize<=0→10
	d, err := neo4j.NewDriverWithContext(cfg.URI, neo4j.BasicAuth(cfg.Username, cfg.Password, ""),
		func(c *neo4j.Config) { c.MaxConnectionPoolSize = cfg.MaxPoolSize; c.ConnectionAcquisitionTimeout = cfg.Timeout })
	if err != nil { return nil, fmt.Errorf("graph: driver: %w", err) }
	vctx, cancel := context.WithTimeout(ctx, cfg.Timeout); defer cancel()
	if err := d.VerifyConnectivity(vctx); err != nil {   // 실패 시 드라이버를 닫고 에러 반환
		_ = d.Close(ctx); return nil, fmt.Errorf("graph: connect: %w", err)
	}
	return &Client{driver: d, timeout: cfg.Timeout}, nil
}
```

`Read`/`Write`는 각각 `AccessModeRead`/`AccessModeWrite` 세션을 열고 `neo4j.WithTxTimeout(defaultTxTimeout)`으로 `ExecuteRead`/`ExecuteWrite`를 호출한 뒤 `defer session.Close(ctx)` 한다. **`Read`가 쓰기 모드 세션을 절대 쓰지 않는 것**이 §프라이버시 6의 방어선 중 하나다.

- [ ] **Step 4: 스키마 보증 구현** (`schema.go`)
§그래프 데이터 모델의 DDL 4문을 상수 슬라이스에 담고 `EnsureSchema`가 `Write`로 하나씩 실행한다. 전부 `IF NOT EXISTS`라 반복 실행이 안전하다. Neo4j는 한 트랜잭션에 스키마 변경과 데이터 변경을 섞지 못하므로 **DDL마다 별도 트랜잭션**이다.

- [ ] **Step 5: 단위 테스트 통과 확인** — `go test ./internal/graph/ -run TestNew -v` → PASS

- [ ] **Step 6: 통합 테스트 작성·실행**
`NEO4J_TEST_URI`가 없으면 `t.Skip`한다(CI는 Neo4j 없이 돈다). 있으면 `EnsureSchema`를 **두 번** 호출해 두 번째도 무오류인지, `SHOW CONSTRAINTS`가 3건인지 확인한다.
Run: `NEO4J_TEST_URI=bolt://localhost:7687 NEO4J_TEST_PASSWORD="$NEO4J_PASSWORD" go test ./internal/graph/ -run Integration -v` → PASS. 환경변수 없이는 SKIP.

- [ ] **Step 7: 커밋** — `git add go.mod go.sum internal/graph/ && git commit -m "feat(graph): add neo4j client and schema bootstrap"`

### Task 3: 관계 타입 화이트리스트

**Files:** Create `internal/graph/reltypes.go`, `internal/graph/reltypes_test.go`
**Consumes:** `model.RelationType` 상수 8종
**Produces:** `func CypherRelType(t model.RelationType) (string, bool)`, `func AllCypherRelTypes() []string`(8종), `const MentionedInRelType = "MENTIONED_IN"`

**이 파일이 따로 존재하는 이유**: Cypher에서 관계 타입은 **파라미터화할 수 없다**(`-[r:$type]-`은 문법 오류). 즉 타입만은 쿼리 문자열에 리터럴로 박히고, 그 값이 DB에서 읽은 문자열이면 그 지점이 유일한 Cypher 인젝션 경로가 된다. 이 파일이 "DB 문자열 → 코드 상수" 매핑을 강제해 그 경로를 닫는다.

- [ ] **Step 1: 실패하는 테스트 작성**

```go
func TestCypherRelType(t *testing.T) {
	cases := map[model.RelationType]string{
		model.RelationCommunicatedWith: "COMMUNICATED_WITH", model.RelationRequestedOf: "REQUESTED_OF",
		model.RelationCommittedTo: "COMMITTED_TO", model.RelationMentions: "MENTIONS", model.RelationBelongsTo: "BELONGS_TO",
		model.RelationScheduledWith: "SCHEDULED_WITH", model.RelationAboutTopic: "ABOUT_TOPIC", model.RelationRelatedTo: "RELATED_TO"}
	for in, want := range cases {
		if got, ok := CypherRelType(in); !ok || got != want {
			t.Errorf("CypherRelType(%q) = %q,%v; want %q,true", in, got, ok, want)
		}
	}
}
func TestCypherRelType_RejectsUnknown(t *testing.T) {
	for _, raw := range []string{"", "drop_all", "RELATED_TO", "MENTIONS`]->() DETACH DELETE n //"} {
		if got, ok := CypherRelType(model.RelationType(raw)); ok {
			t.Errorf("CypherRelType(%q) = %q,true; want ok=false", raw, got)
		}
	}
}
func TestAllCypherRelTypes_HasEightEntries(t *testing.T) {
	if n := len(AllCypherRelTypes()); n != 8 { t.Errorf("length = %d, want 8", n) }
}
```

대문자 `"RELATED_TO"`가 거부되어야 하는 이유: 입력은 항상 Postgres에 저장된 소문자 어휘이므로, 대문자가 들어왔다는 것은 어딘가에서 이미 변환된 값이라는 뜻이고 그 경로를 통과시키면 안 된다.

- [ ] **Step 2: 테스트 실패 확인** — `go test ./internal/graph/ -run TestCypherRelType -v` → FAIL

- [ ] **Step 3: 구현** — `map[model.RelationType]string` 리터럴 하나, 조회 함수, 정렬된 값 슬라이스 반환 함수. 맵 밖의 어떤 문자열도 통과시키지 않는다.

- [ ] **Step 4: 통과 확인 후 커밋** — `go test ./internal/graph/ -v` → PASS. `git add internal/graph/reltypes*.go && git commit -m "feat(graph): whitelist cypher relationship type literals"`

### Task 4: 투영용 Postgres 읽기 쿼리

**Files:** Create `internal/model/projection.go`, `internal/store/graph_source.go`, `internal/store/graph_source_test.go`
**Produces:**

```go
// internal/model/projection.go
type ProjectionRelation struct { ID int64; FromEntityID int64; FromName, FromType string
	ToEntityID int64; ToName, ToType string; Type RelationType
	EvidenceDocumentID string /* UUID 문자열 */; Confidence float64; ObservedAt time.Time }
type ProjectionMention struct { DocumentID, SourceType string; OccurredAt *time.Time
	EntityID int64; EntityName, EntityType string }
type GraphProjectionState struct { LastRelationID int64; ResetToken string }

// internal/store/graph_source.go
func NewGraphSource(pg *Postgres) *GraphSource
func (s *GraphSource) ListRelationsAfter(ctx context.Context, afterID int64, limit int) ([]model.ProjectionRelation, error)
func (s *GraphSource) ListMentionsAfter(ctx context.Context, afterDocumentID string, afterEntityID int64, limit int) ([]model.ProjectionMention, error)
```

- [ ] **Step 1: 실패하는 단위 테스트 작성**

이 저장소의 `internal/store` 테스트는 순수 함수만 다룬다(DB를 띄우지 않는다). 그래서 여기서는 인자 검증과 커서 절 구성만 단위 테스트하고, SQL 자체는 Step 4에서 실제 DB로 검증한다.

```go
func TestListRelationsAfter_RejectsNonPositiveLimit(t *testing.T) {
	if _, err := (&GraphSource{}).ListRelationsAfter(context.Background(), 0, 0); err == nil {
		t.Fatal("limit=0: want error, got nil")
	}
}
func TestMentionCursorClause(t *testing.T) {
	if got := mentionCursorClause(""); got != "" { t.Errorf("empty cursor = %q, want %q", got, "") }
	if got := mentionCursorClause("some-uuid"); got == "" { t.Error("non-empty cursor: want WHERE clause") }
}
```
Run: `go test ./internal/store/ -run "TestListRelationsAfter|TestMentionCursor" -v` → FAIL

- [ ] **Step 2: 관계 쿼리 구현** — 관계와 양 끝 엔티티를 **한 번에** 가져온다(변경 3-2).

```sql
SELECT r.id, fe.id, fe.name, fe.type, te.id, te.name, te.type,
       r.type, r.evidence_document_id, r.confidence, r.observed_at
  FROM entity_relations r
  JOIN entities fe ON fe.id = r.from_entity_id
  JOIN entities te ON te.id = r.to_entity_id
 WHERE r.id > $1 ORDER BY r.id LIMIT $2
```

`entity_relations`는 `ON CONFLICT DO NOTHING`으로만 삽입되는 **append-only** 테이블이라(`internal/store/relations.go`) `id` 워터마크가 정확히 맞는다. 단 `BIGSERIAL`은 커밋 순서와 id 순서가 어긋날 수 있어(A가 100을 잡고 B의 101보다 늦게 커밋) 워터마크가 앞질러 나갈 수 있다. **투영이 멱등하므로 겹쳐 읽는 비용이 없다** — 워커가 매번 `watermark - overlap`부터 다시 읽어 이 틈을 메운다(Task 6).

- [ ] **Step 3: 멘션 쿼리 구현**

```sql
SELECT d.id, d.source_type, d.occurred_at, e.id, e.name, e.type
  FROM document_entities de
  JOIN documents d ON d.id = de.document_id
  JOIN entities  e ON e.id = de.entity_id
 WHERE d.status = 'active'
   AND ($1 = '' OR (de.document_id, de.entity_id) > ($1::uuid, $2))
 ORDER BY de.document_id, de.entity_id LIMIT $3
```

`document_entities`에는 단조 증가 키가 없다(PK가 복합이고 문서 id는 랜덤 UUID다). keyset 커서로 페이지를 넘기되 **매 사이클 커서 0부터 다시 시작하는 전량 스윕**을 한다 — 랜덤 UUID 특성상 커서보다 "작은" 위치에 새 행이 끼어들어 증분 커서만으로는 누락이 생기기 때문이다. 현재 1,402행이고 이를 만드는 EntityWorker는 사실상 휴면(46,539건 중 142건 처리)이라 전량 스윕이 저렴하다. **임계**: 10만 행 초과 시 `created_at` 컬럼 추가 + 시간 워터마크로 전환(§미해결 위험).

- [ ] **Step 4: 실제 DB로 SQL 검증(읽기 전용)** — 단위 테스트는 SQL 유효성을 증명하지 못한다. **프로덕션(macmini)에 붙지 않는다.**

```bash
docker compose -f docker-compose.local.yml --env-file .env.local up -d postgres server
docker compose -f docker-compose.local.yml --env-file .env.local exec postgres psql -U brain -d second_brain \
  -c "EXPLAIN SELECT r.id, fe.id, fe.name, fe.type, te.id, te.name, te.type, r.type, r.evidence_document_id, r.confidence, r.observed_at FROM entity_relations r JOIN entities fe ON fe.id=r.from_entity_id JOIN entities te ON te.id=r.to_entity_id WHERE r.id > 0 ORDER BY r.id LIMIT 10;"
```
Expected: 계획이 출력되고 에러가 없다. 멘션 쿼리도 동일하게 `EXPLAIN`한다. 두 쿼리 모두 PK/인덱스를 타는지 확인한다.

- [ ] **Step 5: 통과 확인 후 커밋** — `go test ./internal/store/ ./internal/model/ -v` → PASS.
`git add internal/model/projection.go internal/store/graph_source*.go && git commit -m "feat(graph): add postgres read queries for graph projection"`

### Task 5: 투영기 — 멱등 MERGE, 워터마크, 전량 재구축

**Files:** Create `internal/graph/projector.go`, `internal/graph/projector_integration_test.go`
**Consumes:** Task 2의 `*Client`, Task 3의 `CypherRelType`, Task 4의 model 타입
**Produces:** `NewProjector(c *Client) *Projector` 와 메서드 `EnsureSchema` / `State(ctx) (model.GraphProjectionState, error)` / `UpsertRelations(ctx, []model.ProjectionRelation) error` / `UpsertMentions(ctx, []model.ProjectionMention) error` / `SetLastRelationID(ctx, int64) error` / `SetResetToken(ctx, string) error` / `Wipe(ctx) error`

- [ ] **Step 1: 실패하는 통합 테스트 작성** (`NEO4J_TEST_URI` 없으면 skip) — 멱등성이 이 작업의 전부다.

```go
func TestProjector_UpsertRelations_Idempotent(t *testing.T) {
	p := newTestProjector(t) // NEO4J_TEST_URI 없으면 t.Skip
	ctx := context.Background()
	if err := p.Wipe(ctx); err != nil { t.Fatal(err) }
	if err := p.EnsureSchema(ctx); err != nil { t.Fatal(err) }
	rows := []model.ProjectionRelation{{
		ID: 1, FromEntityID: 11, FromName: "더미갑", FromType: "PERSON",
		ToEntityID: 22, ToName: "더미을", ToType: "PERSON",
		Type: model.RelationCommunicatedWith,
		EvidenceDocumentID: "00000000-0000-0000-0000-0000000000aa",
		Confidence: 0.9, ObservedAt: time.Now().UTC(),
	}}
	for i := 0; i < 2; i++ {
		if err := p.UpsertRelations(ctx, rows); err != nil { t.Fatal(err) }
	}
	if got := countRels(t, p); got != 1 { t.Fatalf("rel count after 2 identical upserts = %d, want 1", got) }
	if got := countNodes(t, p, "Entity"); got != 2 { t.Fatalf("Entity count = %d, want 2", got) }
}

func TestProjector_State_RoundTrip(t *testing.T) {
	p := newTestProjector(t); ctx := context.Background()
	if err := p.Wipe(ctx); err != nil { t.Fatal(err) }
	if err := p.SetLastRelationID(ctx, 42); err != nil { t.Fatal(err) }
	st, err := p.State(ctx); if err != nil { t.Fatal(err) }
	if st.LastRelationID != 42 { t.Fatalf("LastRelationID = %d, want 42", st.LastRelationID) }
}
```

픽스처 이름은 반드시 가공된 더미다.
Run: `NEO4J_TEST_URI=... NEO4J_TEST_PASSWORD=... go test ./internal/graph/ -run TestProjector -v` → FAIL

- [ ] **Step 2: 관계 업서트 구현** — 행을 **관계 타입별로 묶고**, 묶음마다 `CypherRelType`으로 얻은 리터럴로 `UNWIND` 문 하나를 만든다. 화이트리스트 밖 타입은 건너뛰고 카운트만 로그한다.

```cypher
UNWIND $rows AS row   // row.from/row.to = {name, normalizedName, type} 맵
MERGE (a:Entity {pgId: row.fromId}) ON CREATE SET a += row.from ON MATCH SET a += row.from
MERGE (b:Entity {pgId: row.toId})   ON CREATE SET b += row.to   ON MATCH SET b += row.to
MERGE (a)-[r:COMMUNICATED_WITH {pgId: row.pgId}]->(b)
  ON CREATE SET r.evidencePgId=row.evidencePgId, r.confidence=row.confidence, r.observedAt=datetime(row.observedAt)
```

`COMMUNICATED_WITH` 자리에만 화이트리스트 리터럴이 들어가고 **나머지는 전부 `$rows` 파라미터**다. 관계 `MERGE` 키가 `pgId`이므로 몇 번을 돌려도 엣지가 하나다. 관계에 `ON MATCH`를 걸지 않는 이유는 `entity_relations`가 append-only라 기존 행 속성이 바뀌지 않기 때문이고, 엔티티 이름은 정규화 규칙 변화에 따라 갱신되어야 하므로 `ON MATCH SET`을 건다.

보조 라벨(`:Person`/`:Org`/`:Topic`/`:Other`)은 타입별로 행을 한 번 더 묶어 `MATCH (e:Entity {pgId: row.pgId}) SET e:Person` 형태의 **별도 문**으로 붙인다 — 라벨도 파라미터화가 불가능하므로 엔티티 타입 4종 역시 코드 상수 매핑을 거친다.

배치 하나(기본 500행)가 트랜잭션 하나다. `Eager` 연산자를 피하려고 한 문장에 읽기와 쓰기를 섞지 않는다.

- [ ] **Step 3: 멘션 업서트 구현**

```cypher
UNWIND $rows AS row   // row.doc = {sourceType, occurredAt}, row.entity = {name, normalizedName, type}
MERGE (d:Document {pgId: row.documentId}) ON CREATE SET d += row.doc ON MATCH SET d += row.doc
MERGE (e:Entity {pgId: row.entityId})     ON CREATE SET e += row.entity
MERGE (e)-[m:MENTIONED_IN]->(d) ON CREATE SET m.evidencePgId=row.documentId
```

`occurredAt`이 null일 수 있으므로 Go에서 `nil`을 그대로 넘긴다. **`:Document`에 제목·본문을 절대 넣지 않는다**(§프라이버시 1).

- [ ] **Step 4: 상태·와이프 구현**

```cypher
MERGE (p:ProjectionState {id: 'singleton'})
  ON CREATE SET p.lastRelationId=0, p.resetToken='', p.updatedAt=datetime()
RETURN p.lastRelationId AS lastRelationId, coalesce(p.resetToken,'') AS resetToken
```

`Wipe`는 반환 카운트가 0이 될 때까지 `MATCH (n) WITH n LIMIT 10000 DETACH DELETE n RETURN count(*)`를 반복한다. `CALL {...} IN TRANSACTIONS`는 관리 트랜잭션 안에서 쓸 수 없으므로 사용하지 않는다. `Wipe`는 `:ProjectionState`까지 지운다 — 그래야 다음 사이클이 워터마크 0에서 시작한다.

- [ ] **Step 5: 통합 테스트 통과 확인** — 위 Run 명령 → PASS(관계 1개, 엔티티 2개, 워터마크 42)

- [ ] **Step 6: 쿼리 계획 확인**
```bash
docker compose -f docker-compose.local.yml --env-file .env.local exec neo4j cypher-shell -u neo4j -p "$NEO4J_PASSWORD" \
  "PROFILE MATCH (a:Entity {pgId: 11})-[r]-(b:Entity) RETURN count(r)"
```
Expected: 계획에 `NodeUniqueIndexSeek`가 있고 `AllNodesScan`이 없다.

- [ ] **Step 7: 커밋** — `git add internal/graph/projector*.go && git commit -m "feat(graph): add idempotent projector with watermark and rebuild"`

### Task 6: 투영 워커와 collector 배선

**Files:** Create `internal/worker/graph_projection_worker.go`, `internal/worker/graph_projection_worker_test.go`; Modify `cmd/collector/main.go`(312행 근처)
**Consumes:** Task 4의 `GraphSource`, Task 5의 `Projector`
**Produces:**

```go
type GraphProjectionSource interface {
	ListRelationsAfter(ctx context.Context, afterID int64, limit int) ([]model.ProjectionRelation, error)
	ListMentionsAfter(ctx context.Context, afterDocumentID string, afterEntityID int64, limit int) ([]model.ProjectionMention, error)
}
// GraphProjector: Task 5 Projector의 7개 메서드(EnsureSchema/State/UpsertRelations/
// UpsertMentions/SetLastRelationID/SetResetToken/Wipe)를 동일 시그니처로 선언한 인터페이스.
// 테스트가 fake를 끼울 수 있도록 worker 패키지에 둔다.
type GraphProjectionWorkerConfig struct {
	Source GraphProjectionSource; Projector GraphProjector
	Interval time.Duration // 기본 5m
	BatchSize int          // 기본 500
	Overlap  int64         // 기본 1000
	ResetToken string
}
func NewGraphProjectionWorker(cfg GraphProjectionWorkerConfig) *GraphProjectionWorker
func (w *GraphProjectionWorker) Run(ctx context.Context)
```

**패턴**: `ExtractionWorker`를 그대로 따른다 — `Run`이 ticker를 돌리고 `tick`이 배치를 **직렬로** 처리하며 개별 실패가 워커를 죽이지 않는다. LLM을 부르지 않으므로 3중 데드라인이 필요 없고 tick당 `context.WithTimeout` 하나(기본 2분)면 충분하다.

**한 tick의 순서**: (1) 첫 성공 연결에서 `EnsureSchema` 1회(성공 여부를 필드에 기억) → (2) `State()` 조회, `st.ResetToken != cfg.ResetToken`이면 `Wipe()` → `SetResetToken` → 워터마크 0에서 시작 → (3) 관계: `from := max(0, st.LastRelationID - cfg.Overlap)`부터 `ListRelationsAfter` → `UpsertRelations` → 배치 최대 id로 `SetLastRelationID`, 빈 배치까지 반복하되 tick당 최대 20배치 → (4) 멘션: 커서 빈 값부터 페이지 단위 전량 스윕 → (5) 건수·소요시간만 로그(이름 금지, §프라이버시 4).

- [ ] **Step 1: 실패하는 테스트 작성**

```go
func TestGraphProjectionWorker_TickAdvancesWatermark(t *testing.T) {
	src := &fakeGraphSource{relations: []model.ProjectionRelation{{ID: 7}, {ID: 9}}}
	proj := &fakeProjector{}
	NewGraphProjectionWorker(GraphProjectionWorkerConfig{Source: src, Projector: proj, BatchSize: 10, Overlap: 1000}).
		tick(context.Background())
	if proj.lastRelationID != 9 { t.Fatalf("lastRelationID = %d, want 9", proj.lastRelationID) }
}

func TestGraphProjectionWorker_ResetTokenTriggersWipe(t *testing.T) {
	proj := &fakeProjector{state: model.GraphProjectionState{LastRelationID: 100, ResetToken: "old"}}
	NewGraphProjectionWorker(GraphProjectionWorkerConfig{Source: &fakeGraphSource{}, Projector: proj, ResetToken: "new"}).
		tick(context.Background())
	if !proj.wiped { t.Fatal("reset token changed: want Wipe() called") }
	if proj.resetToken != "new" { t.Fatalf("resetToken = %q, want %q", proj.resetToken, "new") }
}
func TestGraphProjectionWorker_SurvivesProjectorError(t *testing.T) {
	proj := &fakeProjector{upsertErr: errors.New("neo4j down")}
	NewGraphProjectionWorker(GraphProjectionWorkerConfig{
		Source: &fakeGraphSource{relations: []model.ProjectionRelation{{ID: 1}}}, Projector: proj,
	}).tick(context.Background()) // panic 하지 않아야 한다
	if proj.lastRelationID != 0 {
		t.Fatalf("watermark advanced past a failed batch: %d, want 0", proj.lastRelationID)
	}
}
```

세 번째가 핵심 안전 속성이다 — **실패한 배치를 건너뛰고 워터마크가 전진하면 데이터가 영구 유실된다.**
Run: `go test ./internal/worker/ -run TestGraphProjectionWorker -v` → FAIL

- [ ] **Step 2: 워커 구현** — 위 tick 순서대로. `Run`은 `ExtractionWorker.Run`처럼 즉시 1회 tick 후 ticker 루프, `ctx.Done()`에서 반환.

- [ ] **Step 3: 테스트 통과 확인** — 위 Run → PASS

- [ ] **Step 4: collector 배선** — structural signal worker 배선(312행) 바로 뒤. `EXTRACTION_ENABLED`와 **별개 플래그**를 쓴다 — 추출은 LLM 비용이 걸린 결정이고 투영은 아니므로, 한 스위치로 묶으면 한쪽만 끌 방법이 없다.

```go
// --- Graph projection (Part B) ---
// Neo4j는 파생 투영이므로 연결 실패가 collector를 죽이면 안 된다.
gv := os.Getenv("GRAPH_PROJECTION_ENABLED")
graphEnabled := gv == "true" || gv == "1"
wg.Add(1)
go func() {
	defer wg.Done()
	if !graphEnabled { // WaitGroup 균형을 위해 종료까지 대기 (기존 워커들과 동일 패턴)
		slog.Info("graph projection disabled (set GRAPH_PROJECTION_ENABLED=true to enable)")
		<-ctx.Done(); return
	}
	gc, err := graph.New(ctx, graph.Config{URI: os.Getenv("NEO4J_URI"),
		Username: os.Getenv("NEO4J_USERNAME"), Password: os.Getenv("NEO4J_PASSWORD")})
	if err != nil {
		slog.Error("graph projection disabled — neo4j unavailable", "err", err)
		<-ctx.Done(); return
	}
	defer func() { _ = gc.Close(context.Background()) }()
	worker.NewGraphProjectionWorker(worker.GraphProjectionWorkerConfig{
		Source: store.NewGraphSource(pg), Projector: graph.NewProjector(gc),
		Interval:  envDuration("GRAPH_PROJECTION_INTERVAL", 5*time.Minute),
		BatchSize: envInt("GRAPH_PROJECTION_BATCH_SIZE", 500),
		ResetToken: os.Getenv("GRAPH_PROJECTION_RESET_TOKEN"),
	}).Run(ctx)
}()
```

기동 시 접속 실패를 영구 비활성으로 처리하는 것은 의도된 단순화다 — 나중에 Neo4j를 띄우면 collector를 재시작하면 된다. `depends_on`을 걸지 않기로 한 결정(변경 3-3)과 짝을 이룬다.

- [ ] **Step 5: 빌드·전체 테스트 후 커밋** — `go build ./... && go test ./... 2>&1 | tail -20` → 실패 없음.
`git add internal/worker/graph_projection_worker*.go cmd/collector/main.go && git commit -m "feat(graph): wire graph projection worker into collector"`

### Task 7: 읽기 쿼리 카탈로그

**Files:** Create `internal/graph/queries.go`, `queries_test.go`, `queries_integration_test.go`
**Produces:**

```go
type EntryFilter  struct { Since time.Time; MinConfidence float64; EntityTypes []string; Limit int }
type ExpandFilter struct { EntityPgID int64; Since time.Time; MinConfidence float64; RelTypes []string; Limit int }
type EntryNode struct { PgID int64 `json:"entity_id"`; Name string `json:"name"`; Type string `json:"type"`; Degree int64 `json:"degree"` }
type Neighbor  struct { PgID int64 `json:"entity_id"`; Name string `json:"name"`; Type string `json:"type"`
                        RelType string `json:"rel_type"`; Direction string `json:"direction"` // "out"|"in"
                        Weight int64 `json:"weight"`; LastSeen time.Time `json:"last_seen"` }
type EvidenceRef struct { DocumentID string `json:"document_id"`; SourceType string `json:"source_type"`
                        OccurredAt *time.Time `json:"occurred_at"`
                        Confidence float64 `json:"confidence"`; ObservedAt time.Time `json:"observed_at"` }
type EntityHit  struct { PgID int64 `json:"entity_id"`; Name string `json:"name"`; Type string `json:"type"` }
func NewReader(c *Client) *Reader
func (r *Reader) EntryPoints(ctx context.Context, f EntryFilter) ([]EntryNode, error)
func (r *Reader) Expand(ctx context.Context, f ExpandFilter) ([]Neighbor, error)
func (r *Reader) Evidence(ctx context.Context, fromPgID, toPgID int64, relType string, limit int) ([]EvidenceRef, error)
func (r *Reader) SearchEntities(ctx context.Context, prefix string, limit int) ([]EntityHit, error)
```

**상한** (전부 서버에서 clamp): 진입점 20/최대 50, 확장 50/최대 200, 근거 20/최대 100, 검색 10/최대 50, 트랜잭션 타임아웃 5초.

- [ ] **Step 1: clamp·화이트리스트 테스트 작성**

```go
func TestClampLimit(t *testing.T) {
	for _, c := range []struct{ in, def, max, want int }{{0,20,50,20},{-5,20,50,20},{10,20,50,10},{999,20,50,50}} {
		if got := clampLimit(c.in, c.def, c.max); got != c.want {
			t.Errorf("clampLimit(%d,%d,%d) = %d, want %d", c.in, c.def, c.max, got, c.want)
		}
	}
}
func TestSanitizeRelTypes_DropsUnknown(t *testing.T) {
	if got := sanitizeRelTypes([]string{"COMMUNICATED_WITH", "DROP_EVERYTHING", "MENTIONS"}); len(got) != 2 {
		t.Fatalf("kept %d entries, want 2 (%v)", len(got), got)
	}
}
```

`sanitizeRelTypes`는 Task 3의 `AllCypherRelTypes()` 집합만 통과시킨다. 관계 타입은 파라미터로 넘길 수 있는 자리(`type(r) IN $relTypes`)에서만 쓰지만 화이트리스트를 한 번 더 거쳐 오타·탐색 시도를 조기에 잘라낸다.
Run: `go test ./internal/graph/ -run "TestClampLimit|TestSanitizeRelTypes" -v` → FAIL

- [ ] **Step 2: 진입점 쿼리 구현**

```cypher
MATCH (e:Entity)-[r]-(:Entity)
WHERE r.observedAt >= datetime($since) AND r.confidence >= $minConfidence
  AND (size($types) = 0 OR e.type IN $types)
WITH e, count(r) AS degree
ORDER BY degree DESC, e.pgId ASC LIMIT $limit
RETURN e.pgId AS pgId, e.name AS name, e.type AS type, degree
```

이 쿼리만 전역 관계 스캔을 한다(관계 수천 개에서 수 밀리초). `e.pgId ASC` 2차 정렬은 동점 시 순서가 흔들려 화면이 매번 달라지는 것을 막는다.

- [ ] **Step 3: 확장 쿼리 구현**

```cypher
MATCH (e:Entity {pgId: $pgId})-[r]-(n:Entity)
WHERE r.observedAt >= datetime($since) AND r.confidence >= $minConfidence
  AND (size($relTypes) = 0 OR type(r) IN $relTypes)
WITH n, type(r) AS relType,
     CASE WHEN startNode(r) = e THEN 'out' ELSE 'in' END AS direction,
     count(r) AS weight, max(r.observedAt) AS lastSeen
ORDER BY weight DESC, lastSeen DESC, n.pgId ASC LIMIT $limit
RETURN n.pgId AS pgId, n.name AS name, n.type AS type, relType, direction, weight, lastSeen
```

`e`가 유니크 제약 인덱스로 seek되므로 슈퍼노드여도 확장 범위가 그 노드의 이웃으로 한정되고 `LIMIT`이 결과를 자른다. 가변 길이 경로를 쓰지 않는다 — 화면은 항상 1홉씩만 확장한다.

- [ ] **Step 4: 근거·검색 쿼리 구현**

```cypher
MATCH (a:Entity {pgId: $from})-[r]-(b:Entity {pgId: $to}) WHERE type(r) = $relType
MATCH (d:Document {pgId: r.evidencePgId})
RETURN d.pgId AS documentId, d.sourceType AS sourceType, d.occurredAt AS occurredAt,
       r.confidence AS confidence, r.observedAt AS observedAt
ORDER BY r.observedAt DESC LIMIT $limit
```
```cypher
MATCH (e:Entity) WHERE e.normalizedName STARTS WITH $prefix
RETURN e.pgId AS pgId, e.name AS name, e.type AS type ORDER BY e.normalizedName ASC LIMIT $limit
```

`CONTAINS`가 아니라 `STARTS WITH`를 쓰는 이유: range 인덱스는 접두 매치를 가속하지만 부분 문자열은 가속하지 못한다. 부분 검색이 필요해지면 text/full-text 인덱스를 추가한다(범위 밖).

모든 쿼리는 `Client.Read`(읽기 모드 세션 + 5초 트랜잭션 타임아웃)로만 실행한다. **문자열 연결로 Cypher를 만들지 않는다** — 위 4개 문자열은 전부 상수이고 값은 전부 `$param`이다.

- [ ] **Step 5: 단위 테스트 통과 확인** — `go test ./internal/graph/ -v` → PASS

- [ ] **Step 6: 통합 테스트로 계획 검증** — 더미 그래프(엔티티 3개, 관계 4개)를 만들고 4개 쿼리의 결과 개수·정렬을 확인한 뒤, `EXPLAIN`으로 진입점 쿼리 외에는 `AllNodesScan`이 없음을 확인한다.
Run: `NEO4J_TEST_URI=bolt://localhost:7687 NEO4J_TEST_PASSWORD="$NEO4J_PASSWORD" go test ./internal/graph/ -run Integration -v` → PASS

- [ ] **Step 7: 커밋** — `git add internal/graph/queries*.go && git commit -m "feat(graph): add fixed read-only cypher query catalogue"`

### Task 8: 읽기 API 엔드포인트

**Files:** Create `internal/api/graph.go`, `internal/api/graph_test.go`; Modify `internal/api/router.go`, `cmd/server/main.go`
**Produces:** `type GraphReader interface`(Task 7 `Reader`의 4개 메서드와 동일 시그니처, 테스트용 fake를 위해 선언), `func (s *Server) WithGraph(g GraphReader) *Server`

**라우트** (기존 Bearer 인증 그룹 안, `s.graph != nil`일 때만 등록):

| 메서드 · 경로 | 파라미터 |
|---|---|
| `GET /api/v1/graph/entry` | `days`(기본 30), `min_confidence`(기본 0.5), `types`(CSV), `limit` |
| `GET /api/v1/graph/expand` | `entity_id`(필수), `days`, `min_confidence`, `rel_types`(CSV), `limit` |
| `GET /api/v1/graph/evidence` | `from`, `to`, `rel_type`(전부 필수), `limit` |
| `GET /api/v1/graph/entities` | `q`(1자 이상 필수), `limit` |

**GET만 노출한다.** 쓰기 라우트를 아예 만들지 않는 것이 이 API가 그래프를 변경할 수 없다는 가장 단순한 보증이다.

- [ ] **Step 1: 실패하는 핸들러 테스트 작성**

```go
func TestGraphEntryHandler_ClampsLimit(t *testing.T) {
	fake := &fakeGraphReader{}
	srv := newTestServer(t).WithGraph(fake)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/graph/entry?limit=9999", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder(); srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK { t.Fatalf("status = %d, want 200", rr.Code) }
	if fake.lastEntryFilter.Limit != 50 { t.Fatalf("Limit = %d, want clamped to 50", fake.lastEntryFilter.Limit) }
}

func TestGraphExpandHandler_RequiresEntityID(t *testing.T) {
	// entity_id 없는 요청 → 400, reader 호출 없음
}

func TestGraphRoutes_NotRegisteredWithoutWithGraph(t *testing.T) {
	srv := newTestServer(t) // WithGraph 미호출
	req := httptest.NewRequest(http.MethodGet, "/api/v1/graph/entry", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder(); srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound { t.Fatalf("status = %d, want 404 when graph is not wired", rr.Code) }
}
```

세 번째 테스트가 §롤백의 "플래그 off만으로 기능이 사라진다"를 코드로 고정한다.
Run: `go test ./internal/api/ -run TestGraph -v` → FAIL(`WithGraph` 미정의)

- [ ] **Step 2: 라우터 배선** — `Server`에 `graph GraphReader` 필드와 `WithGraph`를 추가하고, `buildHandler`의 Bearer 그룹 안(`/api/v1/notes` 등록부 근처)에 `if s.graph != nil { r.Get(...) × 4 }`를 넣는다. 기존 `WithNotes`/`WithCollectStatus`와 완전히 같은 모양이다.

- [ ] **Step 3: 핸들러 구현** — (1) 파라미터 파싱 + clamp, (2) `days` → `time.Now().AddDate(0,0,-days)`, (3) reader 호출, (4) `{"nodes":[...]}` / `{"neighbors":[...]}` / `{"evidence":[...]}` / `{"entities":[...]}` 반환. reader 에러는 **503**으로 매핑한다 — Neo4j가 죽었을 때 500이 아니라 503이어야 프런트가 "그래프 일시 사용 불가"를 정확히 구분한다. 에러 로그에는 오류 문자열과 엔드포인트명만 남기고 파라미터 값을 남기지 않는다.

- [ ] **Step 4: server 배선** — `NEO4J_URI`와 `NEO4J_PASSWORD`가 **둘 다** 있을 때만 `graph.New`를 호출하고 성공 시 `srv = srv.WithGraph(graph.NewReader(gc))`. 실패하면 경고 로그만 남기고 그래프 라우트 없이 계속 기동한다 — 그래프 때문에 API 서버가 죽으면 안 된다.

- [ ] **Step 5: 통과 확인 후 커밋** — `go test ./internal/api/ -run TestGraph -v && go build ./...` → PASS.
`git add internal/api/graph*.go internal/api/router.go cmd/server/main.go && git commit -m "feat(graph): expose read-only graph query endpoints"`

### Task 9: 웹 프록시 라우트와 API 클라이언트

**Files:** Create `web/src/app/api/graph/{entry,expand,evidence,entities}/route.ts`; Modify `web/src/lib/types.ts`, `web/src/lib/api.ts`
**Produces:**

```ts
export interface GraphNode { entity_id: number; name: string; type: string; degree: number }
export interface GraphNeighbor { entity_id: number; name: string; type: string;
  rel_type: string; direction: "out" | "in"; weight: number; last_seen: string }
export interface GraphEvidence { document_id: string; source_type: string;
  occurred_at: string | null; confidence: number; observed_at: string }
export interface GraphEntityHit { entity_id: number; name: string; type: string }
export interface GraphFilters { days: number; minConfidence: number; entityTypes: string[]; relTypes: string[] }

export async function getGraphEntry(f: GraphFilters, limit?: number): Promise<GraphNode[]>
export async function expandGraphNode(entityId: number, f: GraphFilters, limit?: number): Promise<GraphNeighbor[]>
export async function getGraphEvidence(from: number, to: number, relType: string): Promise<GraphEvidence[]>
export async function searchGraphEntities(q: string): Promise<GraphEntityHit[]>
```

- [ ] **Step 1: 프록시 라우트 4개 작성** — `web/src/app/api/documents/recent/route.ts`를 **글자 그대로 본떠서** 만든다(`BRAIN_API_URL` + `authHeaders()` + 쿼리스트링 전달 + 502 폴백). 새 인증을 붙이지 않는다(Access는 `proxy.ts`가 처리한다).

```ts
/** GET /api/graph/entry → proxies to GET /api/v1/graph/entry */
export async function GET(request: NextRequest): Promise<NextResponse> {
  try {
    const qs = request.nextUrl.searchParams.toString();
    const upstream = await fetch(`${BRAIN_API_URL}/api/v1/graph/entry${qs ? `?${qs}` : ""}`, {
      headers: authHeaders(),
    });
    const data: unknown = await upstream.json();
    return NextResponse.json(data, { status: upstream.status });
  } catch (err) {
    console.error("[api/graph/entry] upstream error:", err);   // 본문을 찍지 않는다
    return NextResponse.json({ error: "upstream error" }, { status: 502 });
  }
}
```

`console.error`에 응답 본문을 찍지 않는다 — 엔티티 이름이 로그로 새는 경로다(§프라이버시 4).

- [ ] **Step 2: 타입과 클라이언트 함수 추가** — 기존 `listRecentByKind` 등과 같은 형태(`fetch` → `res.ok` 검사 → JSON). 503이면 `GraphUnavailableError`를 던져 페이지가 "그래프 일시 사용 불가"를 구분 표시하게 한다.

- [ ] **Step 3: 타입 체크·린트 후 커밋** — `cd web && bun install && bun run type-check && bun run lint` → 오류 없음. **`npm`을 쓰지 않는다.**
`git add web/src/app/api/graph web/src/lib/api.ts web/src/lib/types.ts && git commit -m "feat(web): add graph API proxy routes and client"`

### Task 10: 그래프 페이지 셸 (필터 + 진입점)

**Files:** Create `web/src/app/graph/page.tsx`; Modify 기존 내비게이션(`web/src/app/page.tsx`)
**Consumes:** Task 9의 `getGraphEntry`, `searchGraphEntities`, `GraphFilters`
**Produces:** `GraphFilters` 상태와 선택된 진입점 `entity_id` — Task 11의 캔버스에 그대로 넘어간다

**캔버스 없이 먼저 만드는 이유**: 데이터 경로 전체(브라우저 → Next 프록시 → Go → Neo4j)를 시각화 라이브러리 없이 검증할 수 있어야 한다. 렌더링에서 문제가 생겨도 이 산출물은 그대로 쓸모가 있다.

- [ ] **Step 1: 페이지 작성** — `"use client"`. 상단 필터 4종, 하단 진입점 목록(이름 / 타입 배지 / 연결 수). 기존 관례를 따른다: `@/components/ui`의 `Button`·`Badge`·`Card`·`Spinner`, Tailwind 유틸리티, 실패는 붉은 배너.

| 필터 | UI | 기본값 |
|---|---|---|
| 기간 | 7 / 30 / 90일 세그먼트 버튼 | 30일 |
| 엔티티 타입 | `PERSON`/`ORG`/`CONCEPT`/`OTHER` 토글 배지 | 전체 켬 |
| 관계 타입 | 8종 토글 배지 | 전체 켬 |
| 최소 신뢰도 | 0–1 슬라이더(0.05 단위) | 0.5 |

필터 상태는 URL 쿼리스트링에 반영해 새로고침·공유가 되게 한다(`ask/page.tsx`의 `useSearchParams` 방식과 동일).

- [ ] **Step 2: 진입점 로드** — 마운트·필터 변경 시 `getGraphEntry(filters)`. 결과가 비면 "이 조건에 해당하는 엔티티가 없습니다 — 기간을 넓히거나 신뢰도를 낮춰보세요"를 표시한다(추출 전 흔한 상태다).

- [ ] **Step 3: 엔티티 검색 상자** — 2자 이상에서 300ms 디바운스로 `searchGraphEntities(q)`, 선택 시 그 엔티티를 진입점으로 고정.

- [ ] **Step 4: 내비게이션 링크 추가** — 기존 목록(`/ask`, `/documents`, `/capture`, `/dashboard`, `/governance`)과 같은 위치에 `/graph` 추가.

- [ ] **Step 5: 브라우저 렌더 확인** — `cd web && bun run dev` 후 `http://localhost:3000/graph`.
Expected: 필터가 보이고 진입점 목록 또는 빈 상태 메시지가 나오며 콘솔 에러가 없다. 필터를 바꾸면 URL이 바뀌고 목록이 재로드된다. **타입 체크 통과만으로 완료로 보지 않는다 — 실제 렌더를 확인한다.**

- [ ] **Step 6: 타입 체크·린트 후 커밋** — `cd web && bun run type-check && bun run lint`.
`git add web/src/app/graph web/src/app/page.tsx && git commit -m "feat(web): add graph page shell with filters and entry points"`

### Task 11: 캔버스 렌더링과 1홉 확장

**Files:** Create `web/src/app/graph/GraphCanvas.tsx`; Modify `web/src/app/graph/page.tsx`, `web/package.json`
**Produces:** `GraphCanvas` props — `{ nodes, links, onNodeClick, onLinkClick, maskNames }`

- [ ] **Step 1: 기준 번들 크기 측정** (설치 **전에**) — `cd web && bun run build`. 출력 표의 공유 First Load JS와 라우트별 값을 기록한다. Step 5 게이트의 기준선이다.

- [ ] **Step 2: 라이브러리 설치** — `cd web && bun add react-force-graph-2d`.
우산 패키지 `react-force-graph`가 아니라 **2d 전용**이다(우산 패키지는 three.js를 함께 끌어온다).

- [ ] **Step 3: 캔버스 컴포넌트 작성** — `"use client"`이며 `react-force-graph-2d`를 직접 import한다. 페이지는 **동적으로만** 불러온다.

```tsx
const GraphCanvas = dynamic(() => import("./GraphCanvas"), { ssr: false, loading: () => <Spinner /> });
```

`ssr: false`가 필수다 — force-graph는 `window`/`canvas`에 접근하므로 서버 렌더에서 터진다. 동시에 이 지연 로드가 라이브러리를 초기 번들 밖으로 밀어낸다. 노드 색은 타입별 4색, 링크 굵기는 `weight`의 로그 스케일, 링크 라벨은 관계 타입. `maskNames`가 참이면 마스킹된 문자열을 그린다(Task 12).

- [ ] **Step 4: 1홉 확장 배선** — 노드 클릭 → `expandGraphNode(entity_id, filters)` → 이웃을 기존 노드/링크 집합에 **병합**(중복 `entity_id`는 갱신). 이미 확장한 노드는 `expanded: Set<number>`로 기억해 재요청하지 않는다. 전체 그래프를 한 번에 그리지 않는다(hairball 방지, 설계 §6.3). 화면 노드가 300개를 넘으면 확장을 멈추고 "노드가 너무 많습니다 — 필터를 좁히거나 초기화하세요" 배너 + 초기화 버튼을 띄운다(서버 상한과 별개인 **클라이언트 가독성 상한**).

- [ ] **Step 5: 브라우저 확인 + 번들 게이트**
Expected(렌더): 진입점 20개가 그려지고 노드 클릭 시 이웃이 추가되며 콘솔 에러가 없다. 필터 변경 시 캔버스가 초기화된다.
Run: `cd web && bun run build`
Expected(게이트): `/graph` 라우트의 First Load JS가 Step 1 대비 **250KB(gzip) 이내** 증가. 초과하면 `react-force-graph-2d`를 제거하고 `d3-force` + SVG로 전환한다(props 인터페이스가 같으므로 `GraphCanvas.tsx`만 교체하면 된다). 측정값을 커밋 메시지 본문에 기록한다.

- [ ] **Step 6: 타입 체크·린트 후 커밋** — `cd web && bun run type-check && bun run lint`.
`git add web/package.json web/bun.lock web/src/app/graph && git commit -m "feat(web): render graph canvas with one-hop expansion"`

### Task 12: 근거 패널, 이름 마스킹, 로그 가드

**Files:** Create `web/src/app/graph/EvidencePanel.tsx`, `web/src/app/graph/mask.ts`, `web/src/app/graph/mask.test.ts`, `internal/api/graph_logging_test.go`; Modify `page.tsx`, `GraphCanvas.tsx`

- [ ] **Step 1: 마스킹 테스트 작성** (vitest)

```ts
describe("maskName", () => {
  it("keeps the first character and masks the rest", () => {
    expect(maskName("더미갑")).toBe("더**");
    expect(maskName("Acme Corp")).toBe("A********");
  });
  it("handles short and empty input", () => {
    expect(maskName("A")).toBe("A"); expect(maskName("")).toBe("");
  });
});
```
Run: `cd web && bun run test -- mask` → FAIL(`maskName` 미정의)

- [ ] **Step 2: `mask.ts` 구현과 토글 배선** — 토글은 페이지 헤더 체크박스, 상태는 `localStorage`(`graph.maskNames`). 마스킹은 **클라이언트 렌더링 단계에서만** 적용한다(서버 응답·API는 불변). 근거 패널의 이름에도 같은 함수를 적용한다.

- [ ] **Step 3: 근거 패널 구현** — 링크 클릭 → `getGraphEvidence(from, to, rel_type)` → 우측 패널에 문서 목록. 각 항목은 **소스 타입 배지 + 발생 시각 + 신뢰도**만 보여주고 제목·본문은 보여주지 않는다(Neo4j에 애초에 없다). 각 항목은 기존 `/documents/{id}`로 링크한다 — 원문 열람은 그 페이지의 책임이다.

- [ ] **Step 4: 로그 가드 테스트 작성** — 핸들러가 엔티티 이름을 로그에 남기지 않음을 고정한다.

```go
func TestGraphHandlers_DoNotLogEntityNames(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)
	// fake reader가 "더미갑"을 반환하도록 두고 4개 엔드포인트 + 에러 경로를 태운다.
	if strings.Contains(buf.String(), "더미갑") {
		t.Fatalf("entity name leaked into logs:\n%s", buf.String())
	}
}
```
Run: `go test ./internal/api/ -run TestGraphHandlers_DoNotLogEntityNames -v` → PASS(실패하면 핸들러 로그 문에서 이름을 제거한다)

- [ ] **Step 5: 브라우저 확인** — 링크 클릭 시 근거 패널이 열리고 문서 링크가 동작한다. 마스킹 토글을 켜면 캔버스와 패널의 이름이 즉시 마스킹되고 새로고침 후에도 유지된다.

- [ ] **Step 6: 전체 검증 후 커밋**
```bash
go test ./... 2>&1 | tail -20
cd web && bun run type-check && bun run lint && bun run test && cd ..
git add web/src/app/graph internal/api/graph_logging_test.go
git commit -m "feat(graph): add evidence panel, name masking, and log guards"
```

---

## 배포

macmini에서 수행한다. 모든 compose 명령에 `--env-file .env.local`이 붙는다. Task 1–12가 전부 머지된 뒤 시작한다.

**D1 사전 준비**
- [ ] `.env.local`에 `NEO4J_PASSWORD`(새로 생성한 강한 비밀번호), `NEO4J_URI=bolt://neo4j:7687`, `NEO4J_USERNAME=neo4j`를 채운다. `GRAPH_PROJECTION_ENABLED`는 **아직 `false`로 둔다.**
- [ ] `docker compose -f docker-compose.local.yml --env-file .env.local config >/dev/null` 로 문법 확인

**D2 Neo4j만 먼저 기동**
- [ ] `... up -d neo4j` → `... ps neo4j`가 60초 내 `healthy`
- [ ] `... port neo4j 7687` 이 `127.0.0.1:7687` 인지 확인 (`0.0.0.0`이면 즉시 중단하고 compose를 고친다)
- [ ] cloudflared 설정에 7474/7687 ingress 항목이 **없는지** 확인

**D3 server 재배포**
- [ ] `... up -d --build server`
- [ ] `curl -s -H "Authorization: Bearer $API_KEY" localhost:8081/api/v1/graph/entry | head -c 200`
      Expected: `{"nodes":[]}` (투영 전이므로 빈 배열이 정상). 404면 `WithGraph` 미배선, 503이면 Neo4j 연결 실패다.

**D4 collector 재배포 후 투영 활성화**
- [ ] `... up -d --build collector` (플래그가 꺼져 있어 워커는 대기만 한다)
- [ ] `.env.local`에서 `GRAPH_PROJECTION_ENABLED=true`로 바꾸고 `... up -d collector`로 재생성
- [ ] `... logs -f collector | grep -i graph` — 건수·소요시간만 있고 **이름이 없어야 한다**
- [ ] 1 tick(5분) 뒤 카운트 확인:
```bash
docker compose -f docker-compose.local.yml --env-file .env.local exec neo4j cypher-shell -u neo4j -p "$NEO4J_PASSWORD" \
  "MATCH (e:Entity) RETURN count(e) AS entities;
   MATCH ()-[r]->() RETURN type(r) AS relType, count(r) AS n ORDER BY n DESC;"
```
Expected: 엔티티·관계 수가 0보다 크고 관계 타입이 화이트리스트 9종 밖으로 나오지 않는다.
- [ ] **Postgres와 대조한다** — 이 검증이 투영 정확성의 판정이다.
```bash
docker compose -f docker-compose.local.yml --env-file .env.local exec postgres \
  psql -U brain -d second_brain -tAc "SELECT count(*) FROM entity_relations;"
```
Expected: Neo4j의 엔티티 간 관계 총합(`MENTIONED_IN` 제외)과 **정확히 일치**. 어긋나면 워터마크가 실패 배치를 건너뛴 것이므로 D6 재구축 후 다시 대조한다.
- [ ] 두 번째 tick 이후 관계 수가 **늘지 않는지** 확인(멱등성 실증). 새 추출분만큼만 늘어야 한다.

**D5 웹 재배포**
- [ ] `... up -d --build web`
- [ ] `https://sb.baekenough.com/graph` → Access 통과 → 진입점 렌더, 노드 클릭 확장, 근거 패널, 마스킹 토글, 콘솔 에러 없음 확인

**D6 전량 재구축 (필요 시)** — 둘 다 PostgreSQL을 건드리지 않는다.
1. **리셋 토큰**(권장, 무중단): `.env.local`의 `GRAPH_PROJECTION_RESET_TOKEN`을 새 값(예: `2026-08-20-1`)으로 바꾸고 `... up -d collector`. 다음 tick에서 그래프를 비우고 워터마크 0부터 재투영한다. 토큰이 그대로면 재시작해도 다시 비우지 않는다.
2. **볼륨 삭제**(확실): `... stop neo4j && ... rm -f neo4j && docker volume rm second-brain-local_neo4jdata && ... up -d neo4j && ... restart collector`

수천 건 규모라 어느 쪽이든 수 초~수십 초에 끝난다.

---

## 롤백

**Neo4j는 파생 투영이므로 롤백에 데이터 복구 절차가 없다.** 되돌릴 상태가 진실의 원천에 남지 않도록 설계했다 — 마이그레이션을 추가하지 않고, 워터마크를 Neo4j 안에 두며, `server`/`collector`가 Neo4j에 `depends_on`하지 않는다.

**부분 롤백(기능만 끄기, 30초)**: `.env.local`에 `GRAPH_PROJECTION_ENABLED=false` → `... up -d collector`. 투영이 멈추고 읽기 API·화면은 살아 있으며 그래프는 마지막 상태로 얼어붙는다.

**API·화면까지 끄기**: `NEO4J_URI`를 비우고 `... up -d server web`. server가 `WithGraph`를 호출하지 않아 `/api/v1/graph/*`가 404가 되고(Task 8 Step 1의 세 번째 테스트가 이 동작을 고정한다) `/graph`는 "그래프 일시 사용 불가"를 표시한다.

**완전 제거**:
```bash
docker compose -f docker-compose.local.yml --env-file .env.local stop neo4j
docker compose -f docker-compose.local.yml --env-file .env.local rm -f neo4j
docker volume rm second-brain-local_neo4jdata
# 코드 롤백이 필요하면 해당 커밋 revert. 마이그레이션이 없으므로 DB 조치는 없다.
```

**롤백 후 확인**: `... ps`에 `neo4j`가 없고, 수집·검색·`/ask`가 정상이며, collector 로그에 그래프 관련 에러 반복이 없다.

---

## Self-Review

**1. 설계 문서 커버리지(§6 Part B)**

| 설계 문서 요구 | 대응 |
|---|---|
| §6.1 별도 컨테이너, Postgres가 원천, 증분 `MERGE`, 전량 재빌드 | Task 1·5·6 + 배포 D6 |
| §6.1 Postgres 먼저 쓰고 Neo4j는 그 뒤 — 그래프 실패가 추출을 실패시키지 않음 | Task 6(별도 워커·별도 플래그·`depends_on` 없음) |
| §6.2 다중 라벨 `(:Entity:Person/Org/Topic)`, `(:Document)` | §그래프 데이터 모델 + Task 5 Step 2 |
| §6.2 8종 + `MENTIONED_IN`, 전 관계에 `evidence_pg_id`/`confidence`/`observed_at` | §그래프 데이터 모델 + Task 3·5 |
| §6.2 `MENTIONED_IN`은 기존 `document_entities` 투영(신규 추출 아님) | Task 4 Step 3 |
| §6.3 전체 그래프 미표시, 진입점 N개 → 1홉 확장 | Task 10·11 |
| §6.3 필터 4종 | Task 10 Step 1 |
| §6.3 문서 노드 기본 숨김, "근거 보기"에서만 노출 | Task 12 Step 3 |
| §6.3/§13 라이브러리·N값 확정 | 변경 2 + Task 11 Step 5 게이트 |
| §11 프라이버시 | §프라이버시 처리 방침 + Task 12 |

**2. 플레이스홀더 점검**: 모든 작업에 실행 명령과 기대 결과가 있다. 상한값·기본값을 전부 구체 수치로 확정했다.

**3. 타입 일관성**: `model.ProjectionRelation`/`ProjectionMention`/`GraphProjectionState`(Task 4)가 Task 5·6에서 같은 이름으로 쓰인다. `graph.EntryNode`/`Neighbor`/`EvidenceRef`/`EntityHit`(Task 7)의 JSON 태그가 Task 9의 TS 필드명(`entity_id`, `rel_type`, `document_id`…)과 일치한다. `CypherRelType`(Task 3)을 Task 5와 Task 7의 `sanitizeRelTypes`가 동일 시그니처로 참조한다. Task 2의 `withDefaults`는 `New` 내부 헬퍼로 같은 파일에 둔다.

---

## 미해결 위험

| 위험 | 영향 | 언제 다뤄야 하나 |
|---|---|---|
| `document_entities`에 단조 증가 키가 없어 전량 스윕에 의존 | 테이블이 커지면 매 tick 비용이 선형 증가 | 10만 행 초과 시 `created_at` 추가 + 시간 워터마크 전환 |
| Community에는 관계 유니크 제약과 RBAC이 없다 | 중복 방지가 "단일 직렬 writer"라는 **운영 규약**에 의존한다. 두 번째 writer가 생기면 조용히 깨진다 | 워커를 복제하거나 자연어→Cypher를 붙일 때 Enterprise/읽기 복제본 재검토 |
| 진입점 쿼리가 전역 관계 스캔이다 | 관계 50만 개 규모에서 느려진다 | `degree` 속성 materialize 또는 `observedAt` 관계 인덱스 추가 |
| 사용자 본인 엔티티가 슈퍼노드가 된다 | 확장 결과가 상한에 잘리는데 그 사실이 화면에 드러나지 않는다 | 응답에 `truncated` 플래그 추가 + UI 표시(후속) |
| Part A 추출 품질이 미검증이다 | 관계가 부정확하면 그래프도 그만큼 부정확하다. Part B는 이를 교정하지 않는다 | 초기 배치 1,213건 품질 검증 후 신뢰도 기본 하한 재조정 |
| macmini에 컨테이너가 하나 더 늘어난다 | postgres·server·collector·whisper·web과 메모리를 나눠 쓴다(약 1GB 예약) | 배포 후 `docker stats` 실측, 압박 시 heap 256m로 축소 |
| 향후 postgres가 ubuntu1로 이전되면 배치 전제가 다시 바뀐다 | Neo4j를 따라 옮길지 macmini에 남길지 재판단 필요 | postgres 이전 계획 착수 시점 |
