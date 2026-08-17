# Ask 파이프라인 설계 — 3단계 정제 + 배치 레이어

**작성일**: 2026-08-17
**대상**: `internal/api`, `internal/search`, `internal/model`, `internal/worker` (Go API 서버 + 컬렉터 데몬). 배치 레이어·관측 인프라는 별도 저장소(`sb-agent-platform`)에서 다룬다(§7).
**상태**: 설계 확정 — 구현 전
**관계**: 본 문서는 `2026-08-17-ask-capture-design.md`(이하 "부모 명세")를 대체하지 않는다. 부모 명세 §5는 `POST /api/v1/ask`를 "검색 → 컨텍스트 조립 → 생성"으로 스케치했다. 본 문서는 그 스케치를 **의도분석 → RAG검색 → 종합·배출**의 명시적 3단계 파이프라인으로 정제한다(Part A). 배치 레이어(google/ax)와 관측(Prometheus/Grafana) 설계는 개인 데이터 추론을 포함하지 않는 범용 인프라이므로 별도 공개 저장소 `sb-agent-platform`으로 이전되었다(§7). 부모 명세의 §3(데이터 세그멘테이션), §6(Capture/enrichment), §9–§11(오류 처리·보안·테스트)은 그대로 유효하며 이 문서에서 반복하지 않고 참조만 한다.

---

## 1. 개요

부모 명세 §5.2는 `search.Service.Search`를 두 번(본검색 + 인사이트검색) 호출해 컨텍스트를 조립한다고만 기술했다. 이 설계에는 세 가지 빈틈이 있었다.

1. **의도분석이 없다.** 사용자의 자연어 질문을 그대로 `search.Service`에 넘긴다 — "지난달"이 언제인지, "김대표"가 특정 인물 조회인지, 전화번호 하나를 정확히 찾는 질의인지 구분하지 않는다.
2. **intent 파라미터가 `model.SearchQuery` 필드로 어떻게 매핑되는지 정의되지 않았다** — `SourceType`, `ExcludeSourceTypes`, `Sort`, `UseHyDE`, `UseRerank`, `Weights` 각각을 언제 어떻게 채우는지 명세가 없다.
3. **배치 워크로드(엔티티/요약 enrichment, 평가 실행)를 어디서 돌릴지 결정되지 않았다** — 지금은 전부 `cmd/collector`의 상시 워커이거나 Mac mini에서 직접 실행하는 CLI(`cmd/eval`, `cmd/recordingbackfill`)다.

본 문서는 이 세 가지를 순서대로 다룬다.

### 1.1 오늘(2026-08-17) 측정된 근거

오늘 25개 한국어 질의로 baseline search 평가를 돌린 결과 9건이 실패했다. 실패 유형은 세 갈래로 뚜렷하게 갈렸다.

| 실패 유형 | 증상 | 원인 층위 |
|-----------|------|-----------|
| 인물 엔티티 조회 | "김대표 관련 문서 찾아줘" 류 질의가 아무것도 반환하지 않음 | 검색 파라미터(가중치/쿼리 재작성) 문제 — **의도분석으로 개선 가능** |
| 시간 질의 | "2026년 6월 요약" 이 엉뚱한 달의 문서를 반환 | 검색 파라미터(필터 부재) 문제 — **의도분석으로 개선 가능, 단 스키마 확장 필요(§4.5)** |
| 정확 토큰 조회 | 전화번호 하나를 정확히 찾는 질의가 완전히 실패 | 텍스트 인덱스 토큰화 문제 — **의도분석으로 고칠 수 없음(§4.3)** |

이 표가 Part A 설계 전체의 근거다. 아래에서 각 실패 유형을 어떤 스테이지가, 어떻게, 그리고 왜 고치거나 못 고치는지 구체적으로 다룬다.

---

## 2. 목표 및 비목표

### 목표

- `POST /api/v1/ask`(부모 명세 §5.1)의 내부 구현을 의도분석 → RAG검색 → 종합·배출 3단계로 명시적으로 분리하고, 각 단계를 독립적으로 테스트 가능하게 한다.
- intent 파라미터가 기존 `model.SearchQuery` 필드로 정확히 어떻게 매핑되는지 확정한다 — 존재하지 않는 필드를 가정하지 않는다.
- 원문/정리/추론 3계층 분리(부모 명세 §3)를 RAG검색 단계에서 어떻게 물리적으로 유지하는지 명세한다.
- 반복 실행되는 배치 워크로드(엔티티/요약 enrichment, 평가) 중 어떤 것을 google/ax 기반 배치 레이어로 옮길 가치가 있는지, 어떤 것을 지금 그대로 둘지 근거와 함께 결정한다(결정 및 상세 설계는 `sb-agent-platform` 저장소로 이전, §7).
- 파이프라인과 배치 레이어가 방출해야 할 관측 신호를 기존 prometheus/grafana에 맞춰 정의한다(상세 설계는 `sb-agent-platform` 저장소로 이전, §7).

### 비목표

- `POST /api/v1/ask`의 SSE 프로토콜, 에러 케이스, 스트리밍 구현(`Completer.StreamWithMessages`) — 부모 명세 §5.1·§5.3에서 이미 확정되었다. 본 문서는 그 안의 컨텍스트 조립 로직만 정제한다.
- Capture/enrichment 워커의 재시도 정책, 암묵지 추출 범위 — 부모 명세 §6.3·§6.4에서 이미 확정. 본 문서는 그 워커를 배치 레이어 후보로만 다룬다(§6.1).
- google/ax의 실제 Go API(패키지 함수 시그니처 수준)를 확정하는 것 — v0.2.3 시점의 정확한 API는 이 저장소에 아직 임포트된 적이 없어 검증할 수 없다(§6.4에 명시).
- 의도분석 단계의 프롬프트 템플릿 최종본 — 분류 스키마와 인터페이스만 확정하고, 프롬프트 튜닝은 구현 단계로 미룬다(§9).

---

## 3. Part A 아키텍처 — 왜 인프로세스인가

파이프라인 3단계(의도분석·RAG검색·종합·배출)는 **분산되지 않는다.** 전부 Mac mini의 기존 Go API 서버(`cmd/server`) 프로세스 안에서, 하나의 요청 컨텍스트로 순차 실행된다. google/ax(§6)는 이 경로에 들어오지 않는다. 세 가지 이유가 있다.

1. **검색은 pgvector 옆에 있어야 한다.** `internal/store/document.go`의 hybrid search는 FTS/vector/bigm/summvec/entity 5개 CTE를 하나의 SQL 쿼리로 RRF 융합한다(§4.4). 이 쿼리는 Postgres 안에서 실행되고, Go 서버는 로컬 소켓으로 그 결과를 받는다. 파이프라인을 분산시키면 매 질의마다 Tailscale 홉이 하나 끼어든다 — Mac mini(DB) ↔ ubuntu1(파이프라인) 왕복이 검색 자체보다 느려질 수 있다. 사용자를 더 빠르게 만들려고 분산했는데 더 느려지는 구조는 채택할 이유가 없다.
2. **ax의 가치는 suspend/resume이고, 이 경로는 그게 필요 없다.** ax는 격리되고 중단·재개 가능한 실행 환경을 제공하는 분산 하니스 런타임이다(§6.2). `/ask`는 초 단위로 끝나는 요청-응답 흐름이다 — 중단됐다가 나중에 재개할 "장기 실행 에이전트"가 아니다. ax를 여기 끼워 넣으면 얻는 것 없이 배포 대상 하나와 네트워크 홉 하나만 추가된다.
3. **ax는 v0.2.3이고, 이 경로는 사용자 대면이다.** `github.com/google/ax`는 2026-08-13 릴리스된 v0.2.3(`go 1.26.3`)이 최신 태그이고, 이 시점 기준 breaking change가 예상되는 초기 버전이다. 검색 경로를 여기 올리면, ax가 깨질 때 사용자가 매일 쓰는 "질문하기" 기능이 함께 깨진다. 배치 워크로드(§6)라면 ax 장애가 재시도로 흡수되지만, 요청-응답 경로에서는 그 장애가 곧 사용자에게 보이는 오류다.

```
브라우저 → Next.js proxy → Go API 서버(cmd/server, Mac mini)
                                │
                                ├─ Stage 1: 의도분석 (internal/intent, 신규)
                                ├─ Stage 2: RAG검색   (internal/search.Service, 기존)
                                └─ Stage 3: 종합·배출  (internal/api/ask.go, 기존 — SSE)
                                                          │
                                                          ▼
                                              PostgreSQL + pgvector (같은 호스트, 로컬 소켓)
```

세 스테이지는 별도 프로세스나 별도 패키지 경계로 물리적으로 분리되지 않고, 같은 요청 안에서 순차 호출되는 Go 함수 체인이다 — "독립적으로 테스트 가능"은 배포 단위가 아니라 **인터페이스 단위**의 독립성을 의미한다(각 스테이지 입출력이 좁은 인터페이스로 정의되어, 다음 스테이지가 이전 스테이지를 가짜(fake)로 대체해 테스트할 수 있다는 뜻).

---

## 4. Stage 1 — 의도분석 (Intent)

### 4.1 역할

사용자 질문 텍스트를 받아, RAG검색 단계가 `model.SearchQuery`를 채우는 데 쓸 수 있는 **구조화된 파라미터**를 뽑아낸다. 의도분석은 검색을 실행하지 않는다 — 검색 실행 방법을 결정할 뿐이다.

### 4.2 인터페이스

```go
// internal/intent/intent.go (신규 패키지)

package intent

import "time"

// Kind classifies what the user is asking for, at the level of granularity
// that changes how retrieval should be parameterised.
type Kind string

const (
	KindGeneral    Kind = "general"     // no special routing — plain hybrid search
	KindEntity     Kind = "entity"      // person/org lookup — boost entity lane
	KindTemporal   Kind = "temporal"    // date-scoped query ("2026년 6월 요약")
	KindExactToken Kind = "exact_token" // phone number, code, etc. — flagged, NOT fixed here (§4.3)
)

// Params is the output of Stage 1, consumed by Stage 2 (§5.2 mapping table).
type Params struct {
	RawQuery     string
	Kind         Kind
	EntityName   string     // non-empty when Kind == KindEntity; the extracted name, lower-cased
	OccurredFrom *time.Time // non-nil when Kind == KindTemporal AND a range was resolved
	OccurredTo   *time.Time
	Confidence   float64    // 0–1; low confidence falls back to KindGeneral in Stage 2 (§4.4)
}

// Classifier turns a raw question into Params. Implementations MAY combine a
// cheap deterministic pre-pass (regex for phone-number-shaped tokens, Korean
// relative-date phrases) with an LLM fallback for ambiguous cases — see
// classification strategy below. Both paths return the same Params shape so
// Stage 2 does not need to know which path produced it.
type Classifier interface {
	Classify(ctx context.Context, question string) (Params, error)
}
```

**분류 전략**: 두 단계로 나눈다.

1. **결정론적 사전 검사** — 정규식으로 전화번호 형태(`010-\d{4}-\d{4}` 등), 날짜 상대 표현("지난달", "이번 주", "2026년 6월")을 먼저 검사한다. 매치되면 LLM 호출 없이 `Kind`를 확정한다(정확 토큰은 `KindExactToken`으로 플래그만 남기고 §4.3 이유로 파라미터를 만들지 않는다; 날짜 상대 표현은 결정론적으로 `OccurredFrom`/`OccurredTo`를 계산한다 — "2026년 6월"은 모호함이 없는 문자열이므로 LLM 없이 파싱 가능하다).
2. **LLM 폴백** — 사전 검사에 걸리지 않으면 `internal/llm.Completer`(부모 명세 §5.3의 기존 `CompleteWithMessages`)로 분류한다. 인물 엔티티 조회("김대표 관련...")처럼 자연어 형태가 다양한 경우가 여기 해당한다.

이 순서는 임의가 아니다 — 날짜·전화번호처럼 문법이 고정된 패턴을 LLM에 보내는 것은 그 자체로 낭비이고, LLM 분류는 오분류 가능성이 있어 결정론적으로 풀리는 것부터 결정론적으로 처리하는 편이 더 신뢰할 수 있다.

### 4.3 무엇을 고칠 수 있고, 무엇을 고칠 수 없는가 (중요)

**고칠 수 있음 — 인물 엔티티 조회**: `internal/store/document.go`의 hybrid search에는 이미 entity RRF 레인이 있다(`model.SearchWeights.EntityWeight`, #139). 이 레인은 `query.Query` 문자열을 `entities.normalized_name`에 `LIKE` 매치한다(document.go:556-574). 문제는 이 레인이 기본으로 꺼져 있거나(`EntityWeight`가 0이고 `ENTITY_EXTRACTION_ENABLED`가 미설정) 켜져 있어도 다른 신호와 동등 가중치로 묻혀버릴 수 있다는 데 있다. Stage 1이 `KindEntity`를 감지하면 Stage 2는 `EntityWeight`를 명시적으로 높이고, 쿼리 텍스트를 엔티티 이름 중심으로 좁혀 이 레인이 실제로 지배적으로 작동하게 만들 수 있다(§5.2). **주의**: `model.SearchQuery`에 "엔티티 이름으로 필터링"하는 별도 필드는 없다 — entity 레인은 필터가 아니라 RRF 신호이므로, 의도분석이 할 수 있는 일은 그 신호의 가중치를 올리고 검색 문자열을 좁히는 것이지, "이 엔티티가 아니면 제외"하는 하드 필터를 거는 것이 아니다.

**고칠 수 있음(단, 스키마 확장 필요) — 시간 질의**: §4.5에서 별도로 다룬다.

**고칠 수 없음 — 정확 토큰 조회**: 전화번호 같은 정확 토큰 조회 실패는 텍스트 인덱스의 토큰화 문제이지, 의도 문제가 아니다. 이유는 세 가지다.

1. `documents.tsv`는 `to_tsvector('simple'|'english', content)`로 생성된다(`migrations/001_init.sql`). Postgres FTS 파서는 하이픈이 섞인 숫자열(`010-1234-5678`)을 인덱싱 시점과 조회 시점에 항상 동일하게 토큰화한다는 보장이 없다 — `plainto_tsquery`가 만드는 lexeme과 문서 안 표기가 다르면(공백/하이픈 유무, 국가번호 포함 여부) 매치가 실패한다.
2. bigm 레인(`bigm_similarity` + `LIKE '%...%'`)은 원문 그대로의 부분 문자열 매치이므로 이론적으로는 더 안전하지만, 질의 문자열의 표기(하이픈 유무 등)가 저장된 표기와 정확히 일치해야 한다 — 사용자가 "01012345678"로 묻고 문서에 "010-1234-5678"로 저장되어 있으면 실패한다.
3. `internal/collector/smsmap/redact.go`가 수집 시점에 OTP 등 민감 토큰을 리댁션한다(부모 명세 §10). 조회하려는 번호 자체가 리댁션 대상이었다면, 애초에 원문이 저장되어 있지 않으므로 어떤 검색 전략으로도 찾을 수 없다 — 이건 버그가 아니라 의도된 프라이버시 가드다.

의도분석이 `KindExactToken`을 감지해봤자, 그 신호로 Stage 2가 할 수 있는 일은 bigm 레인 가중치를 최대화하는 것뿐이고, 위 세 가지 근본 원인 중 어느 것도 해결하지 못한다. **이 실패 유형을 실제로 고치려면 검색/인덱스 레이어의 변경(예: 전화번호 정규화 컬럼 + 별도 인덱스)이 필요하며, 그 작업은 본 문서의 범위 밖이다.** 이 사실을 의도분석 단계가 마법처럼 해결한다고 암시하지 않는다 — §9 미결 사항에 남긴다.

### 4.4 실패 모드

Stage 1은 항상 무언가를 반환해야 한다 — 실패해서 아무것도 못 돌려주는 상황은 허용하지 않는다.

| 상황 | 처리 |
|------|------|
| LLM 분류 호출 실패(오류/타임아웃) | `Kind: KindGeneral`, `Confidence: 0`으로 폴백 — Stage 2는 일반 hybrid search로 진행(§5.4) |
| LLM이 파싱 불가능한 응답 반환 | 위와 동일하게 `KindGeneral` 폴백 |
| 결정론적 사전 검사와 LLM 결과가 모순 | 결정론적 사전 검사가 우선(문법이 고정된 패턴이 자연어 분류보다 신뢰도가 높음) |
| `Confidence < 0.5`(LLM 경로만 해당) | Stage 2에서 `KindGeneral`로 취급 — 낮은 확신으로 검색 파라미터를 좁히면 recall이 오히려 나빠진다 |

즉 Stage 1의 유일한 "치명적" 실패는 없다 — 항상 `KindGeneral`로 안전하게 저하(degrade)한다. 이는 `search.Service.Search` 자체가 임베딩 실패 시 FTS-only로 저하하는 기존 패턴(search.go:139-149)과 같은 설계 원칙이다.

### 4.5 시간 질의 — 스키마 확장이 필요함 (계획과 코드의 불일치)

**"2026년 6월 요약" 실패를 의도분석으로 고칠 수 있다는 전제는, 오늘 시점 코드에서는 성립하지 않는다.** `model.SearchQuery`에는 날짜 범위 필터 필드가 없다(`internal/model/document.go` 전체 검토 결과 `OccurredFrom`/`OccurredTo`류 필드 부재). `documents.occurred_at` 컬럼 자체는 존재하고 `Sort: "recent"`일 때 정렬 기준(`COALESCE(occurred_at, collected_at) DESC`)으로만 쓰인다(`internal/store/document.go:401-411`) — **정렬이지 필터가 아니다.** 즉 "2026년 6월" 범위 밖 문서라도 다른 신호(FTS/vector 점수)가 높으면 여전히 결과에 섞여 나온다. 이것이 정확히 오늘 관측된 실패 증상(엉뚱한 달의 문서가 반환됨)의 원인이다.

이 실패 유형을 실제로 고치려면 다음 스키마 확장이 **선행되어야 한다** — 본 문서에서 결정으로 확정하되, 구현은 §9로 미룬다.

- `model.SearchQuery`에 `OccurredFrom *time.Time`, `OccurredTo *time.Time` 필드 추가.
- `internal/store/document.go`의 hybrid search 5개 CTE(fts/vec/bigm/summvec/entity) 각각의 `WHERE` 절에 `occurred_at BETWEEN $n AND $m`(NULL 허용 시 `COALESCE(occurred_at, collected_at)`) 조건을 추가. 기존 `statusFilter`/`sourceFilter`/`excludeFilter` 문자열 조립 패턴(document.go:650-660)과 동일한 방식으로 끼워 넣는다.

Stage 1은 `OccurredFrom`/`OccurredTo`를 결정론적으로(regex + 달력 계산, LLM 불필요) 채울 수 있다 — "2026년 6월"은 `2026-06-01T00:00:00`~`2026-06-30T23:59:59`로 명확히 환산 가능하다. 이 스키마 확장이 없으면 Stage 1이 아무리 정확하게 범위를 뽑아내도 Stage 2가 그것을 쓸 곳이 없다 — **의도분석 단계 하나만으로는 이 실패 유형을 고칠 수 없고, 검색 스키마 확장과 짝을 이루어야 고쳐진다.**

### 4.6 테스트

기존 저장소 컨벤션(테이블 기반 + 가짜(fake) 구현체, DB 통합 테스트 없음)을 그대로 따른다.

| 대상 | 테스트 케이스 |
|------|--------------|
| 결정론적 사전 검사 | 전화번호 패턴 매치/불일치, "2026년 6월"·"지난달"·"이번 주" 등 날짜 표현 → 정확한 `OccurredFrom`/`OccurredTo` 계산 검증(테이블 기반, `time.Time` 고정 기준시각 주입) |
| LLM 분류 폴백 | 가짜 `llm.Completer`로 정상 JSON 응답 → `Kind`/`EntityName`/`Confidence` 파싱 검증 / 파싱 실패·오류 → `KindGeneral` 폴백 검증(§4.4) |
| Confidence 임계값 | `Confidence < 0.5` 응답 → Stage 2 소비 시 `KindGeneral`로 취급되는지 |
| 결정론적/LLM 우선순위 | 두 경로가 모순되는 인위적 케이스에서 결정론적 결과가 이기는지 |

DB 통합 테스트는 만들지 않는다 — `internal/intent` 패키지는 DB에 의존하지 않는다(순수 텍스트 → `Params` 변환).

---

## 5. Stage 2 — RAG검색 (Retrieval)

### 5.1 역할

Stage 1의 `intent.Params`를 소비해 `internal/search.Service`(기존, 수정 없음)를 구동한다. 새 검색 알고리즘을 만들지 않는다 — 기존 RRF 융합·HyDE·rerank를 어떤 파라미터로 호출할지만 결정한다.

### 5.2 intent → `model.SearchQuery` 매핑

| `intent.Params` | `model.SearchQuery` 필드 | 매핑 규칙 |
|------------------|---------------------------|-----------|
| `Kind == KindEntity`, `EntityName` | `Weights.EntityWeight`, `Query` | `EntityWeight`를 `model.DefaultEntityWeight`(0.5)보다 높은 값(예: 1.5)으로 명시 설정해 엔티티 레인이 지배적이 되게 함. `Query`는 원문 질문 그대로 유지하되(다른 레인이 여전히 작동해야 하므로), entity 레인의 `LIKE` 매치 대상은 `Query` 문자열 전체이므로 `EntityName`이 `Query`에 실제로 포함되어 있어야 매치된다(그렇지 않으면 원 질문에 사용자가 이미 이름을 언급한 경우만 이 레인이 작동함 — 새 필드 없이 기존 구조로 할 수 있는 한계) |
| `Kind == KindTemporal`, `OccurredFrom`/`OccurredTo` | (§4.5의 신규 필드, 미구현) `OccurredFrom`/`OccurredTo` | §4.5 스키마 확장 완료 후에만 유효. 확장 전에는 `Sort: "recent"`로만 근사(§4.5에서 설명한 대로 완전한 해결책이 아님을 §9에 명시) |
| `Kind == KindExactToken` | 없음(§4.3) | Stage 2는 이 신호를 받아도 `Weights.BigmWeight`를 소폭 상향하는 것 외에 할 수 있는 일이 없다 — 실패를 감추지 않고 `sources: []`로 정직하게 반환한다(§5.4) |
| `Kind == KindGeneral` 또는 `Confidence < 0.5` | (변경 없음) | 기존 부모 명세 §5.2의 기본 동작 그대로 — `SearchQuery{Query: question, Limit: K}` |
| (모든 경우) 인사이트 제외 여부 | `ExcludeSourceTypes: []model.SourceType{model.SourceInsight}` (본검색) / `SourceType: &model.SourceInsight` (인사이트검색) | 부모 명세 §5.2에서 이미 확정 — Stage 2는 이 두 번 호출 구조를 그대로 유지한다(§5.3) |
| 재질문 신뢰도가 낮은 애매한 질문 | `UseHyDE: true` | Stage 1이 `Confidence < 0.5`이면서 질문 길이가 짧을 때(모호한 단문 질의) HyDE로 recall을 보강하는 것을 권장 — 필수는 아님, 실측 후 튜닝(§9) |
| (변경 없음) | `UseRerank`, `Weights.FTSWeight`/`VecWeight`/`BigmWeight`/`SummaryVec`, `Sort` | 이번 설계에서 intent가 건드리지 않는다 — 기존 서비스 기본값 유지 |

### 5.3 3계층 분리 유지

부모 명세 §3의 원문/정리/추론 3계층 분리는 Stage 2에서 물리적으로 강제된다 — Stage 2가 스스로 라벨을 붙이는 게 아니라, **두 번의 별도 검색 호출**로 애초에 섞이지 않게 한다(부모 명세 §5.2와 동일한 구조, 여기서 재확인).

1. **본검색**: `ExcludeSourceTypes: []model.SourceType{model.SourceInsight}` — `insight` 문서는 결과 집합에 존재조차 하지 않는다.
2. **인사이트검색**: `SourceType: &model.SourceInsight` — 오직 `insight` 문서만.

Stage 2의 출력 타입은 이 분리를 반영한다.

```go
// internal/search/retrieval.go 또는 internal/api/ask.go 내부 (배치 위치는 구현 단계 결정)
type RetrievalResult struct {
	Observed  []*model.SearchResult // 본검색 결과 — raw note + 정리(metadata)는 포함되나 insight는 없음
	Inferred  []*model.SearchResult // 인사이트검색 결과 — source_type=insight만
}
```

`Observed`와 `Inferred`를 하나의 슬라이스로 합치지 않는다 — Stage 3(종합·배출)이 이 둘을 서로 다른 프롬프트 섹션에 배치해야 하기 때문이다(부모 명세 §5.2의 `[관측된 사실]`/`[추론]` 구조, §6.1에서 재확인).

### 5.4 실패 모드

| 상황 | 처리 |
|------|------|
| 본검색 0건 | `RetrievalResult{Observed: nil, Inferred: <인사이트검색 결과>}` 반환 — Stage 3이 `no_evidence`로 종료(부모 명세 §5.1 `finish_reason`) |
| 인사이트검색 0건 (흔함 — 노트가 없거나 enrichment 전) | `Inferred: nil`, `Observed`는 정상 반환 — 인사이트 레인이 비는 것은 오류가 아니라 정상 상태(부모 명세 §3.2 가드가 의도한 대로) |
| `search.Service.Search` 자체가 에러 반환(DB 오류 등) | Stage 2는 에러를 그대로 Stage 3에 전파 — Stage 3이 `error` SSE 이벤트로 변환(부모 명세 §5.1) |
| `Kind == KindExactToken`인데 검색 0건(§4.3에서 설명한 대로 예견된 실패) | 다른 실패와 동일하게 `no_evidence`로 처리 — "의도는 파악했지만 못 찾았다"를 사용자에게 숨기지 않는다. 프롬프트에 "정확한 토큰 조회를 시도했으나 근거를 찾지 못했다"는 문구를 추가하는 것은 Stage 3 책임(§9 미결) |

### 5.5 테스트

| 대상 | 테스트 케이스 |
|------|--------------|
| intent → SearchQuery 매핑 | 각 `Kind`별로 올바른 `model.SearchQuery` 필드가 채워지는지 — 가짜 `search.DocumentSearcher`(기존 인터페이스, `internal/search/search.go:24`)로 실제 호출 파라미터를 캡처해 검증. DB 접근 없음 |
| 3계층 분리 | `ExcludeSourceTypes`/`SourceType` 두 호출이 항상 정확한 값으로 나가는지 — `search.Service` 자체가 아니라 그것을 감싸는 Stage 2 조립 함수를 대상으로 |
| 본검색 0건 폴백 | `Observed: nil`이어도 패닉하지 않고 `RetrievalResult`를 정상 반환하는지 |
| `EntityWeight` 상향 | `KindEntity`일 때 기본값(0.5)이 아닌 상향된 값이 `SearchQuery.Weights.EntityWeight`에 들어가는지 |

`internal/search.Service`, `internal/store` 자체의 RRF SQL 로직은 이미 `internal/search/search.go`, `internal/store/document.go`에 기존 테스트가 있으며 본 문서에서 재검증하지 않는다 — Stage 2는 그 위에 얇게 얹히는 파라미터 조립 레이어이므로, 가짜 `DocumentSearcher`로 "무엇을 호출했는가"만 검증하면 충분하다. DB 통합 테스트는 만들지 않는다(저장소 전체에 DB 통합 테스트가 없다는 기존 컨벤션 유지).

---

## 6. Stage 3 — 종합·배출 (Synthesis & Output)

### 6.1 역할

부모 명세 §5.1–§5.3이 이미 확정한 내용을 그대로 이어받는다 — 이 절은 **변경이 아니라 Stage 2 출력과의 접합부**를 명시한다.

- `RetrievalResult.Observed`/`Inferred`를 부모 명세 §5.2의 프롬프트 포맷(`[관측된 사실]`/`[추론 — 가설이며 사실로 인용 불가]`)에 그대로 대입한다.
- 시스템 프롬프트 고정 지시(부모 명세 §5.2): 답은 컨텍스트에 근거해야 하고, 근거 불충분 시 "모른다"고 답하며, `insight` 레인은 가설로만 인용한다 — **이 제약은 이 문서에서도 무조건 유지된다.** 추론이 사실로 인용되는 경로를 여는 어떤 변경도 이 설계의 범위 밖이다.
- SSE 스트리밍(`sources`/`token`/`done`/`error` 이벤트, `Completer.StreamWithMessages`)은 부모 명세 §5.1·§5.3 그대로.

### 6.2 인터페이스

```go
// internal/api/ask.go (부모 명세 §12 파일 레이아웃에 이미 명시된 파일)
type Synthesizer interface {
	// Synthesize streams the answer for question given the retrieved context.
	// onToken is called once per generated token chunk (SSE pass-through);
	// the returned FinishReason maps directly to the "done" event payload
	// (parent spec §5.1: "stop" | "error" | "no_evidence").
	Synthesize(
		ctx context.Context,
		question string,
		result RetrievalResult,
		onToken func(text string),
	) (finishReason string, err error)
}
```

### 6.3 실패 모드

부모 명세 §9.1의 표를 그대로 계승한다 — 이 문서에서 추가하는 것은 Stage 2 출력이 비었을 때의 접합부뿐이다.

| 상황 | 처리 |
|------|------|
| `RetrievalResult.Observed`가 비어 있음 | `finishReason: "no_evidence"` — 프롬프트에 "근거 없음"을 명시(부모 명세 §5.1) |
| `RetrievalResult.Inferred`가 비어 있음 | 정상 — 프롬프트의 `[추론]` 섹션을 생략하고 진행(§5.4에서 이미 언급한 대로 오류 아님) |
| Stage 2가 에러 반환 | `error` SSE 이벤트 + `finishReason: "error"`(부모 명세 §9.1) |
| LLM 스트리밍 중 오류 | 부모 명세 §9.1과 동일 |

### 6.4 테스트

부모 명세 §11.1이 이미 `POST /api/v1/ask` 핸들러 테스트 케이스(인증/빈 질문/검색 0건/이벤트 순서/LLM 미설정)를 정의했다. 본 문서는 그 테스트가 이제 `RetrievalResult{Observed, Inferred}`를 가짜로 주입하는 형태로 바뀐다는 점만 추가한다 — 기존 테스트 케이스 목록은 그대로 유효하다.

---

## 7. 관련 인프라 — 배치 실행 레이어 및 관측

배치 워크로드 실행 환경(google/ax 기반 격리·suspend/resume 하니스)과 메트릭 관측(Prometheus + Grafana) 설계는 별도 저장소 **`sb-agent-platform`**으로 이전되었다. 이 두 레이어는 어떤 소비 서비스가 무엇을 처리하는지 알지 못하는 범용 인프라이며, 개인 데이터에 대한 추론이나 가공을 포함하지 않으므로 공개(public) 저장소로 관리한다.

- 배치 레이어 후보 워크로드 선정 근거(요약 백필을 1순위로 선택하고 note enrichment worker·평가 실행은 유보한 이유), 크로스호스트 DB 접근 비용, ax 도입 비용은 `sb-agent-platform`의 배치 레이어 설계 문서를 참조한다.
- 파이프라인/배치 레이어가 방출해야 할 Prometheus 메트릭 스키마(`/metrics` 엔드포인트 신설, 스크레이프 방식)는 `sb-agent-platform`의 관측 설계 문서를 참조한다.

이 저장소(second-brain) 쪽에서 남는 작업은 위 문서가 정의한 메트릭 이름 규약에 맞춰 `internal/api`, `internal/worker`에 실제 `/metrics` 핸들러와 카운터/히스토그램을 배선하는 것뿐이다 — 그 배선 작업 자체는 이 문서의 범위 밖이다.

---

## 8. 미결 사항

### 8.1 `model.SearchQuery` 날짜 범위 필드의 정확한 이름과 시맨틱

§4.5에서 `OccurredFrom`/`OccurredTo` 추가가 필요하다고 확정했으나, `IncludeDeleted`처럼 기존 명명 규칙과 정확히 어떻게 어울리는지, NULL `occurred_at`(수집 시점 이벤트-시간 개념이 없는 소스)을 범위 필터에서 어떻게 다룰지(포함/제외)는 구현 단계에서 정한다.

### 8.2 `KindExactToken`의 사용자 노출 방식

§5.4에서 "정확 토큰 조회를 시도했으나 근거를 찾지 못했다"는 문구를 Stage 3 프롬프트에 추가하는 것을 언급했으나, 이것이 부모 명세 §5.1의 `finish_reason` 값 집합(`stop`/`error`/`no_evidence`)에 새 값을 추가할 일인지, 기존 `no_evidence` 안에서 프롬프트 문구로만 처리할지 정하지 않았다.

### 8.3 전화번호 등 정확 토큰 조회 자체의 근본 해결책

§4.3에서 이것이 검색/인덱스 레이어 변경(정규화된 전화번호 컬럼 + 별도 인덱스) 없이는 풀리지 않는다고 확정했으나, 그 변경 자체는 이 문서의 범위 밖이며 별도 설계가 필요하다. 리댁션된 번호(§4.3의 세 번째 이유)는 애초에 검색 불가능한 것이 의도된 동작이므로 이 미결 항목에서 제외한다.

### 8.4 `/metrics` 엔드포인트의 인증 여부

`sb-agent-platform`의 관측 설계 문서가 신규 `/metrics` 핸들러 필요성을 언급했으나, 비인증으로 둘지(prometheus 스크레이프 편의) `requireAPIKey` 그룹에 넣을지(일관성) 정하지 않았다. 사설 오버레이 네트워크 안에서만 도달 가능하다는 전제로 비인증도 안전할 수 있으나, 확정하지 않는다.

### 8.5 요약 백필 배치 작업의 진행 상태 기록 스키마

`sb-agent-platform`의 배치 레이어 설계 문서가 "Postgres에 결과를 기록하고 얇은 어댑터로 노출"하라고 방향만 정했다 — 실제 테이블 스키마(체크포인트 방식, 재개 시 어디서부터 이어가는지)는 ax의 실제 suspend/resume API를 확인한 뒤에만 정확히 설계할 수 있다. v0.2.3의 Go API 표면은 이 저장소에서 검증된 바 없다(§2 비목표).

### 8.6 ax 파일럿의 성공 기준

요약 백필을 ax로 옮기기로 결정했으나(`sb-agent-platform` 참조), "성공"을 무엇으로 판단할지(예: 12시간 실행이 중단 없이 완료되는 비율, 기존 일회성 마이그레이션 CLI 방식 대비 재개 시간 단축폭) 수치 기준은 정하지 않았다. 파일럿 실행 후 사후에 정한다.

---

## 9. 요약 — 이 문서가 확정한 것과 확정하지 않은 것

**확정**: 3단계 파이프라인이 인프로세스로 남는다는 것(Part A, §3의 3가지 이유), intent → SearchQuery 매핑 표(§5.2, 존재하는 필드만 사용), 시간 질의는 스키마 확장 없이는 못 고친다는 것(§4.5), 정확 토큰 조회는 의도분석으로 못 고친다는 것(§4.3), 배치 레이어·관측 설계는 `sb-agent-platform` 저장소로 이전되었다는 것(§7).

**미확정**: 스키마 확장의 정확한 필드 시맨틱(§8.1), ax의 실제 Go API 표면(§2 비목표, §8.5), `/metrics` 인증 정책(§8.4), 파일럿 성공 기준(§8.6).
