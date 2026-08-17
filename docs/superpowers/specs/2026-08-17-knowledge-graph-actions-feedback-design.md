# 지식 그래프·액션·피드백 설계 — 추출 → 그래프/액션·브리핑/피드백 루프

**작성일**: 2026-08-17
**대상**: `internal/store`(신규 마이그레이션), `internal/worker`(신규 추출 워커), `internal/api`(신규 그래프·액션·브리핑 엔드포인트), 신규 `internal/graph`(Neo4j 클라이언트), 신규 `internal/actions`, `internal/search`(가중치 최적화기 신설), `web/`(그래프 뷰·브리핑 화면). 배포 대상 호스트: Mac mini(PostgreSQL, Go API 서버), ubuntu1(Neo4j 컨테이너, 가중치 최적화기 배치 작업).
**상태**: 설계 확정 — 구현 전
**관계**: 본 문서는 `2026-08-17-ask-pipeline-design.md`(이하 "ask 파이프라인 명세"), `2026-08-17-ask-capture-design.md`(이하 "ask/capture 명세")와 함께 읽어야 한다. Part D(피드백 루프)는 `POST /api/v1/ask`(ask/capture 명세 §5, ask 파이프라인 명세가 3단계로 정제)의 존재를 전제로 한다 — 이 백엔드는 현재 **구현되어 있지 않으며**, 별도 구현 계획서도 아직 없다. 본 문서는 `/ask` 백엔드 자체를 설계하거나 구현하지 않는다 — Part D는 그 백엔드가 존재한다고 가정한 상태에서 위에 얹히는 피드백 수집·가중치 최적화 계층만 다룬다(§10 의존성 참조).

---

## 1. 개요

second-brain은 지금까지 SMS·통화·Gmail 등을 자동 수집해 검색 가능한 문서 코퍼스로 축적해 왔다. 문서 단위 검색(FTS/vector/bigm RRF 융합)은 이미 동작하지만, 문서 **사이**의 관계 — 누가 누구와 무엇을 주고받았는지, 어떤 약속이 아직 처리되지 않았는지 — 는 어디에도 구조화되어 있지 않다. `entities` 테이블(684행)과 `document_entities`(1,402행)로 "이 문서에 이 엔티티가 등장한다"는 사실만 기록되어 있고, 엔티티 간 관계 테이블은 존재하지 않는다.

본 명세는 네 부분으로 구성된 하나의 기능 묶음을 기술한다.

- **A(추출)**: 문서에서 엔티티 간 관계와 미처리 액션(응답 대기, 약속)을 LLM으로 추출해 신규 테이블에 저장한다.
- **B(그래프 뷰)**: A의 산출물을 Neo4j에 파생 투영해 탐색 가능한 그래프 화면을 제공한다.
- **C(액션·브리핑)**: A의 액션 산출물과 SQL만으로 계산되는 구조 신호를 병합해, 사용자가 놓치기 쉬운 미결 항목을 브리핑으로 보여준다.
- **D(피드백 루프)**: `/ask`의 검색 결과에 사용자가 근거 단위로 피드백을 남기면, 그 피드백이 검색 가중치 자동 튜닝으로 이어지는 폐루프를 만든다.

B·C는 A의 산출물에 의존하고, A 없이는 성립하지 않는다. D는 A·B·C와 데이터를 공유하지 않는 독립된 서브시스템이지만, 전제 조건인 `/ask` 백엔드가 아직 없다는 점에서 이 넷 중 가장 먼저 다른 작업(별도 구현 계획)을 필요로 한다. `/ask`는 이 설계에서 "나중에 만들 부가 화면"이 아니라 — 피드백이 쌓이는 유일한 경로이자 검색 품질 개선 루프 전체의 입구이므로 — **이 기능 묶음의 허브이자 토대**로 취급한다.

---

## 2. 목표 및 비목표

### 목표

- 활성 소스(gmail, sms, call-log, call-transcript)의 최근 문서에서 엔티티 관계와 미결 액션을 구조화된 데이터로 추출한다(Part A).
- 추출된 관계를 탐색 가능한 그래프 화면으로 제공하되, PostgreSQL을 단일 진실 원천으로 유지하고 Neo4j는 언제든 재구축 가능한 파생 투영으로 취급한다(Part B).
- "내가 답장을 안 한 것", "내가 약속한 것", "상대가 나에게 약속한 것"을 LLM 없이도 계산 가능한 SQL 구조 신호와 LLM 추출 신호를 병합해 근거가 명시된 브리핑으로 제공한다(Part C).
- `/ask`의 검색 결과에 대한 근거 단위 피드백을 수집해, 오염되지 않은 평가셋을 기반으로 검색 가중치를 자동 튜닝하는 폐루프를 만든다(Part D).
- 기존 `entities`/`document_entities` 스키마, 기존 검색 자산(`internal/search/{search,rerank,hyde,tune}.go`)을 깨지 않고 확장한다.

### 비목표

폰 푸시 알림, 액션 자동 처리(답장 대필), 전체 코퍼스로의 즉시 확장, 그래프 알고리즘 기반 추천(중심성·커뮤니티 탐지), 모델 파인튜닝 — 사유와 함께 §9에서 다룬다.

---

## 3. 현재 상태 — 실측 근거

이 절의 모든 수치는 2026-08-17 시점 Mac mini prod DB에서 실측한 값이다. 아래 설계는 이 수치를 전제로 한다.

### 3.1 DB 규모

PostgreSQL 16.14, 총 12 GB(`chunks` 10 GB, `documents` 2.5 GB). 확장: `vector 0.8.2`, `pg_bigm 1.2`, `uuid-ossp`, `plpgsql`.

### 3.2 문서 분포와 시간 필드 채움률

`deleted_at is null` 기준.

| source | 문서 | occurred_at 채움 | entities_processed_at 채움 | occurred_at 범위 | 최근 30일(occurred 기준) |
|---|---|---|---|---|---|
| llm-memory | 19,885 | 0 | 134 | — | 0 |
| secretary | 18,322 | 18,322 | 0 | 2022-04-19 ~ 2026-06-22 | 0 |
| gmail | 6,133 | 6,127 | 8 | 2025-04-22 ~ 2026-08-17 | 487 |
| call-log | 770 | 770 | 0 | 2026-05-30 ~ 2026-08-16 | 333 |
| call-transcript | 551 | 551 | 0 | 2026-06-08 ~ 2026-08-15 | 206 |
| sms | 441 | 441 | 0 | 2026-06-01 ~ 2026-08-17 | 204 |

**활성 4개 소스**(gmail, call-log, call-transcript, sms)의 최근 30일(occurred_at 기준) 합계는 487 + 333 + 206 + 204 = **1,230건**이다. 이 값이 Part A 초기 배치 범위를 결정한다(§5.1). llm-memory와 secretary는 `occurred_at`이 대부분 없거나(llm-memory 전량 null) 최근 활동이 없어(secretary 최근 30일 0건, 최신 문서가 2026-06-22로 약 2개월 전) 이번 범위에서 제외한다.

### 3.3 기존 스키마 (변경하지 않음, 확장만 함)

```sql
entities(id, name, type, normalized_name, created_at)          -- 684행
document_entities(document_id, entity_id)                      -- 1,402행
-- 엔티티 간 관계 테이블 없음
documents(..., occurred_at, entities_processed_at, ...)        -- 두 컬럼 이미 존재
feedback(id, query, document_id, chunk_id, source, session_id,
         user_id, thumbs, comment, metadata, created_at)       -- 50행 (eval 시드)
```

### 3.4 소스별 metadata 키 (값이 아니라 키 이름)

| source | metadata 키 |
|---|---|
| gmail | `from`, `to`, `thread_id`, `label_ids` |
| sms | `contact_name`, `direction`, `is_auth_like` |
| call-log | `contact_name`, `direction`, `duration_seconds`, `audio_file`, `transcription`, `recording_type` |
| call-transcript | `contact_name`, `direction`, `language`, `model`, `relative_path`, `audio_size`, `duration_seconds`, `recording_type` |

이 키들은 Part C의 구조 신호(§7.1)와 Part A의 프롬프트 컨텍스트(§5.2)가 직접 참조한다.

### 3.5 기존 검색 자산

`internal/search/{search,rerank,hyde,tune}.go`, `cmd/eval`. `tune.go`는 **측정 라이브러리**다 — `NDCGK`, `MRRK`, `FalsePositivePenalty`, `AggregateFPPenalty`, `EvalMetrics`, `Aggregate` 함수를 제공하지만 **가중치를 자동으로 바꾸는 최적화기는 없다**. `search.go`는 `model.SearchWeights`(RRF 융합 가중치, 기본 k=60·레인 균등 가중치)를 `WithWeights`로 요청 단위 주입받는 구조다 — 즉 가중치를 바꾸는 코드 경로 자체는 이미 있고, 없는 것은 "무엇으로 바꿀지 결정하는 로직"이다. `feedback.thumbs = -1`은 이미 부적합 신호로 소비되고 있다(기존 eval 파이프라인).

---

## 4. 아키텍처 개요 — 분해와 실행 순서

```
A: 추출 파이프라인 (문서당 LLM 1회 호출)
   │  산출물: entity_relations, actions
   │
   ├──▶ B: 그래프 뷰       (Neo4j 파생 투영, entity_relations를 MERGE)
   ├──▶ C: 액션·브리핑     (actions + SQL 구조 신호 병합)
   │
D: 피드백 루프 (/ask 전제 — A/B/C와 데이터 의존 없음)
   │  전제: POST /api/v1/ask (미구현, 별도 계획 필요)
   │  산출물: feedback 근거 레이블 → 가중치 최적화기 → model.SearchWeights 승격
```

A는 B와 C의 **선행 조건**이다 — A가 채우지 않은 `entity_relations`/`actions` 없이는 B의 그래프도, C의 LLM 기반 액션도 존재할 수 없다. 단 C의 구조 신호(§7.1)는 A와 무관하게 기존 `documents`/`metadata`만으로 SQL로 계산되므로, A가 아직 한 번도 돌지 않은 상태에서도 C는 부분적으로(구조 신호만으로) 동작할 수 있다 — 이는 A 파이프라인이 실패하거나 지연되어도 C가 완전히 죽지 않는다는 뜻이며, 별도 폴백 설계가 아니라 §7.2가 이미 전제하는 병합 구조의 자연스러운 귀결이다.

D는 A/B/C와 저장소를 공유하지 않는다 — `entity_relations`/`actions`를 읽지도 쓰지도 않는다. D가 이 문서에 함께 묶인 이유는 기능적 의존이 아니라, 이번 작업 묶음의 "허브"인 `/ask`(§1)를 통해서만 D가 존재할 수 있기 때문이다. 실행 순서상 A→B→C는 순차적 데이터 의존이고, D는 `/ask` 백엔드 존재 여부라는 별도의 선행 조건을 가진 병렬 트랙이다(§10에서 재확인).

---

## 5. Part A — 추출 파이프라인

### 5.1 대상 범위

**초기 배치**: 활성 4개 소스(gmail, sms, call-log, call-transcript)의 `occurred_at` 최근 30일 문서, **약 1,230건**(§3.2). 이 배치로 품질을 검증한 뒤 과거 문서로 범위를 넓히고(시점·기준은 §9 범위 밖), 이후에는 신규 유입 문서를 증분 처리한다.

### 5.2 단일 LLM 호출 구조

문서당 LLM 호출은 **한 번**이다. `{entities, relations, actions}`를 담은 단일 JSON을 한 번의 호출로 받는다 — 엔티티 추출, 관계 추출, 액션 추출을 세 번의 패스로 나누면 입력 토큰(문서 원문 + 소스별 metadata)을 세 번 반복 전송해야 하는데, 단일 호출은 그 입력 토큰을 1/3로 줄인다.

파싱 실패(모델이 JSON 뒤에 부연 텍스트를 덧붙이는 등)는 기존 요약 파이프라인이 이미 겪고 있는 결함과 동일한 양상으로 재현될 것으로 예상하며, **기존 요약 파이프라인과 같은 재시도 경로**를 그대로 재사용한다 — 이 재추출 파이프라인만을 위한 별도 파서·재시도 로직을 새로 만들지 않는다.

### 5.3 관계 타입 — 닫힌 어휘

```
communicated_with, requested_of, committed_to, mentions,
belongs_to, scheduled_with, about_topic, related_to
```

모델이 이 8종 밖의 관계 타입을 반환하면 `related_to`로 강등한다. 닫힌 어휘를 쓰는 이유는 두 가지다 — (1) Part B의 그래프 화면이 관계 타입별 필터를 제공하려면 값의 종류가 유한해야 하고, (2) Part C가 `committed_to`/`requested_of`를 액션 판별 신호로 직접 참조하므로(§7.2) 타입 값이 자유 텍스트면 그 참조 자체가 불가능하다.

### 5.4 신규 스키마

```sql
entity_relations(
    from_entity_id, to_entity_id, type, evidence_document_id,
    confidence, observed_at,
    UNIQUE(from_entity_id, to_entity_id, type, evidence_document_id)
)

actions(
    identity_key UNIQUE, document_id, thread_key, kind, summary,
    counterpart_entity_id, due_at, detected_by, confidence, observed_at
)

action_status(
    identity_key, state, resolved_by, resolved_at, note
)
```

| 컬럼 | 값 집합 |
|---|---|
| `actions.kind` | `awaiting_my_reply` \| `my_commitment` \| `their_commitment` \| `scheduled` |
| `actions.detected_by` | `structural` \| `llm` \| `both` |
| `action_status.state` | `open` \| `done` \| `ignored` |
| `action_status.resolved_by` | `user` \| `auto` |

`entity_relations`의 `UNIQUE(from_entity_id, to_entity_id, type, evidence_document_id)`는 같은 문서 근거로 같은 관계가 중복 저장되는 것만 막는다 — 같은 두 엔티티 사이에 다른 문서를 근거로 한 같은 타입 관계는 별도 행으로 누적된다(예: "김대표"와 "박부장"이 `communicated_with`로 이메일 3통에서 각각 관찰되면 3행).

### 5.5 `identity_key` — 결정적 해시인 이유

`actions.identity_key`는 `thread_key + kind + counterpart + 정규화 요약`을 입력으로 하는 **결정적 해시**다. 재추출을 돌려도 동일한 실제 액션은 항상 같은 `identity_key`를 얻는다 — 그래서 `action_status`(사용자가 "완료"/"무시"로 표시한 상태)가 재추출 이후에도 살아남는다.

이 설계는 임의의 선택이 아니라 이 프로젝트의 실제 사고에서 나온 결정이다. 과거 SMS `source_id` 재키잉 작업에서, 파생 식별자가 재생성 시점마다 값이 바뀌는 구조였던 탓에 문서가 고아화되는 문제가 발생했고, 이 문제는 data-safety 리뷰에서 두 차례 연속 NO-GO 판정을 받은 뒤에야 메타데이터 기반 in-place 재키잉 마이그레이션(019)으로 해결되었다. `identity_key`를 처음부터 입력 필드들의 결정적 함수로 설계하는 것은, 재추출이라는 반복 가능한 작업이 사용자의 완료/무시 판단을 매번 지워버리는 동일한 함정을 재발시키지 않기 위함이다.

### 5.6 실패 모드

| 상황 | 처리 |
|---|---|
| LLM 호출 실패/타임아웃 | 기존 요약 파이프라인과 동일한 재시도 경로(§5.2) |
| JSON 파싱 실패(부연 텍스트 등) | 기존 요약 파이프라인과 동일한 재시도 경로 — 이 실패 유형 자체가 기존 파이프라인에서 이미 관측된 결함이므로 재현을 전제로 설계함 |
| 관계 타입이 닫힌 어휘 밖 | `related_to`로 강등(§5.3), 재시도 없음 — 정보 손실보다 파이프라인 중단이 더 나쁘다는 판단 |
| 같은 문서를 재추출 | `entity_relations`는 `UNIQUE` 제약으로 중복 삽입 방지(upsert), `actions`는 `identity_key` 충돌 시 기존 행 갱신(§5.5) |

---

## 6. Part B — 그래프

### 6.1 저장소와 원칙

**Neo4j를 별도 컨테이너로 ubuntu1 호스트에 배치**한다. PostgreSQL이 진실의 원천이고, **Neo4j는 언제든 재구축 가능한 파생 투영**이다. 추출 결과(§5.4)는 항상 PostgreSQL에 먼저 쓰고, Neo4j는 그 뒤 증분 `MERGE`로 반영한다 — 이 순서 때문에 Neo4j 쓰기가 실패해도 추출 자체는 실패로 이어지지 않는다. 전량 재빌드 명령을 제공한다(엔티티가 수천 규모라 재빌드는 수 초 내에 끝난다).

### 6.2 노드·관계 스키마

```
(:Entity:Person), (:Entity:Org), (:Entity:Topic)   -- entities.type을 다중 라벨로 매핑
(:Document)                                          -- 기본 화면에서 숨김(§6.3)
```

type을 노드 속성이 아니라 **다중 라벨**로 두는 이유는 두 가지다 — 라벨은 Neo4j 인덱스가 직접 활용할 수 있고, LLM이 Cypher 쿼리를 생성할 때(향후 자연어 그래프 질의를 붙일 경우) 속성 필터보다 라벨 매치가 환각 위험이 적다.

관계는 §5.3의 8종 닫힌 어휘에 `(:Entity)-[:MENTIONED_IN]->(:Document)` 하나를 더한다. `MENTIONED_IN`은 신규 추출이 아니라 **기존** `document_entities`(1,402행)를 그대로 투영한 것이다 — Part A가 새로 만드는 관계가 아니다. 이 8+1 관계 전부에 `evidence_pg_id`, `confidence`, `observed_at` 속성을 붙인다.

### 6.3 화면 설계

전체 그래프를 기본으로 그리지 않는다(hairball 방지). 진입점은 최근 활동 상위 엔티티 N개이고, 클릭 시 **1홉씩** 확장한다. 필터는 4종 — 기간, 엔티티 타입, 관계 타입, 최소 신뢰도. **문서 노드는 기본 숨김**이며, 엣지의 "근거 보기"를 눌렀을 때만 나타난다.

렌더링 라이브러리는 `react-force-graph`가 유력 후보이나, **번들 크기 측정 후 구현 계획 단계에서 확정**한다(§13). 상위 엔티티 개수 N의 정확한 값도 같은 단계에서 확정한다.

---

## 7. Part C — 액션·브리핑

### 7.1 구조 신호 (SQL, LLM 불필요)

`thread_key` 생성 규칙:

| source | thread_key |
|---|---|
| gmail | `gmail:{thread_id}` |
| sms | `sms:{contact_name}` |
| call-log / call-transcript | `call:{contact_name}` |

각 스레드에서 `occurred_at` 최대인 문서의 방향(`direction`)이 수신이면 "내 응답 대기" 후보다. gmail은 `direction` metadata 키가 없으므로, `from`이 사용자 자신의 주소인지로 방향을 판정한다(sms/call은 기존 `direction` 키를 그대로 쓴다). `contact_name`이 null이면(sms/call) 전화번호 해시를 스레드 키로 쓰고, 화면에는 "알 수 없는 번호"로 표시한다.

### 7.2 신호 병합

구조 신호(§7.1)와 A의 LLM 액션(§5)을 `identity_key`로 병합하고, `detected_by`로 출처를 표시한다(`structural`/`llm`/`both`). 두 경로가 같은 실제 사건을 가리켜도 `identity_key`가 `thread_key + kind + counterpart + 정규화 요약`의 결정적 함수이므로(§5.5) 같은 키로 수렴해 하나의 행으로 합쳐진다. 근거 문서 링크는 **모든 액션에 필수**다 — 구조 신호로 생성된 액션도 그 판정의 근거가 된 스레드 최신 문서를 링크로 갖는다.

### 7.3 브리핑 생성

브리핑은 **온디맨드**로 생성된다(화면을 열 때). 다만 `hash(열린 액션 identity_key 집합 + 최신 문서 occurred_at)`을 키로 짧게 캐시해, 동일 입력으로 재생성하는 것을 막는다. 새 문서 유입이나 액션 상태 변경(완료/무시 처리) 시 이 해시가 바뀌므로 캐시는 자동으로 무효화된다 — 별도의 TTL이나 명시적 무효화 트리거가 필요 없는 구조다.

**브리핑 출력의 모든 문장에 근거 document id를 강제**한다. 근거가 없거나 존재하지 않는 id를 단 문장은 폐기한다. 이 제약을 두는 이유는, 브리핑이 여러 액션·문서를 하나의 요약 문단으로 종합하는 LLM 생성물이라 창작이 섞이기 가장 쉬운 지점이고, 사용자가 브리핑에서 한 번이라도 거짓 진술을 접하면 이 기능 전체에 대한 신뢰가 무너지기 때문이다.

폰 푸시 알림은 범위 밖이다(§9) — 브리핑이 온디맨드 생성인 것의 직접적인 귀결이다(푸시하려면 상시 생성이 필요한데, 이번 설계는 그 반대를 확정했다).

### 7.4 실패 모드와 방어

| 실패 모드 | 방어 |
|---|---|
| 시스템 밖에서 이미 처리한 일이 계속 뜸 | "무시" 버튼 → `action_status.state = ignored`, `resolved_by = user`로 영속 저장 |
| 인증문자·광고가 액션으로 오탐 | sms 기존 `is_auth_like` metadata 플래그로 구조 신호 단계에서 사전 제외 |
| LLM이 실재하지 않는 액션을 생성 | 근거 문서 필수(§7.3) + `detected_by` 노출 + `confidence` 표시 |
| 브리핑의 그럴듯한 거짓 | 문장별 근거 document id 강제, 검증 실패 문장 폐기(§7.3) |
| 오래된 액션의 무한 누적 | `occurred_at` 기준 90일 경과 시 자동 보관 — **`action_status.state`에 새 값을 추가하지 않는다**. 활성 액션 목록/브리핑 조회 쿼리가 기본적으로 `state = 'open' AND occurred_at > now() - interval '90 days'`만 노출하는 방식으로, 조회 시점 필터링만으로 구현한다(§13에서 이 해석을 명시) |
| 발신자 미상 | 번호 해시를 스레드 키로, 화면엔 "알 수 없는 번호"(§7.1) |

---

## 8. Part D — 피드백 루프 (`/ask` 통합)

### 8.1 흐름

```
질문 → POST /api/v1/ask → 검색(RRF, model.SearchWeights) → 근거 + 답변(SSE)
                                              │
                              근거별 👍/👎 → feedback 테이블
                                              │
                                        평가셋 성장
                                              │
                          가중치 최적화기(신규, ubuntu1) → NDCG/MRR/FP penalty 측정
                                              │
                                  회귀 게이트 통과 시 승격 (model.SearchWeights)
```

### 8.2 근거 단위 피드백

**피드백은 답변 문장이 아니라 근거(문서)에 붙인다.** `(query, document_id, thumbs)` 쌍이 곧 검색 품질 레이블이 된다. 기존 `feedback` 스키마(`id, query, document_id, chunk_id, source, session_id, user_id, thumbs, comment, metadata, created_at`, §3.3)가 이미 이 형태를 상정하고 있으므로, 스키마 변경 없이 그대로 재사용한다. `/ask`의 `sources` SSE 이벤트(ask 파이프라인 명세 §5.1)에 실린 각 근거 항목에 👍/👎 UI를 붙이는 것이 D가 `/ask`에 요구하는 유일한 프런트엔드 접점이다.

### 8.3 가중치 최적화기

`model.SearchWeights`(RRF 융합 가중치, k 포함) 공간을 좌표 탐색(coordinate search)으로 탐색하는 최적화기를 신설한다. 이 작업은 CPU 바운드 배치이므로 **ubuntu1 배치 계층**에 둔다(Mac mini의 요청-응답 경로에 올리지 않는다 — ask 파이프라인 명세 §3이 이미 확립한 "인프로세스 경로에 배치 워크로드를 얹지 않는다"는 원칙과 동일선상). `internal/search/tune.go`의 기존 측정 함수(`NDCGK`, `MRRK`, `FalsePositivePenalty`, `AggregateFPPenalty`, `EvalMetrics`, `Aggregate`)를 그대로 재사용한다 — 새 측정 지표를 만들지 않는다.

### 8.4 평가셋 오염 방지

피드백 수집 즉시 학습용/검증용으로 **고정 분할**한다. 검증용 분할은 최적화기의 탐색 루프에 **절대 노출되지 않는다** — 승격 판정은 오직 검증셋에서만 이루어진다. 기존 시드 50행(§3.3)도 같은 분할 규칙을 적용받는다(수집 시점 기준으로 소급 분할).

### 8.5 승격 정책

가중치 변경은 **자동 승격**하되, 모든 변경 이력을 저장해 즉시 이전 값으로 되돌릴 수 있게 한다. 단 **평가셋(검증 분할 기준)이 질의 30개에 도달하기 전에는 자동 승격하지 않고 제안만 한다** — 30개 미만인 표본에서의 승격은 통계적으로 신뢰하기 어렵다는 판단에 따른 단계적 활성화다.

---

## 9. 범위 밖 (명시)

| 항목 | 사유 |
|---|---|
| 폰 푸시 알림 | 브리핑이 온디맨드 생성 구조이므로(§7.3), 상시 생성이 필요한 푸시와 설계상 양립하지 않는다 |
| 액션 자동 처리(답장 대필) | 이번 범위는 감지·집계까지이며, 사용자 대신 행동을 취하는 기능은 다루지 않는다 |
| 전체 코퍼스로의 즉시 확장 | 초기 배치(최근 30일, 약 1,230건, §5.1)의 품질 검증 후 별도 판단 |
| 그래프 알고리즘 기반 추천(중심성·커뮤니티 탐지) | Part B는 탐색 화면만 다루며, 그래프 알고리즘 계층은 이번 설계에 포함하지 않는다 |
| 모델 파인튜닝 | 이 프로젝트는 원격 API 호출만 허용하는 정책(로컬 추론 금지)이라 파인튜닝은 정책상 배제된다 |

---

## 10. 선행·병행 의존성

1. **PostgreSQL → ubuntu1 이전이 선행되어야 한다.** 사용자 승인은 이미 완료되었으나 착수 전이다. Neo4j(§6.1)도 ubuntu1에 배치되므로, 순서상 DB 이전이 먼저 끝나야 한다.
2. **`POST /api/v1/ask` 백엔드가 존재하지 않는다.** Part D 전체의 전제 조건이며(§1, §8), ask 파이프라인 명세는 설계만 확정된 상태이고 구현 계획서는 아직 작성되지 않았다.
3. **`llm-memory` 소스에 결함 2건이 있다.** (a) collector가 마운트한 `memory.sqlite`가 WAL 모드인데 컨테이너에 `-wal`/`-shm` 파일이 없어 2026-05-30 체크포인트 스냅샷만 보인다. (b) `occurred_at`이 19,885건 전부 null이다(§3.2). 이번 범위(활성 4소스)에는 영향이 없으나 별도 이슈로 추적이 필요하다.
4. **기존 엔티티 추출이 사실상 중단되어 있다.** 코퍼스 전체(약 46,539건) 중 `entities_processed_at`이 채워진 문서는 142건뿐이다(§3.2 표의 소스별 합계). Part A의 신규 추출 파이프라인은 이 정체된 파이프라인을 대체하지 않는다 — 별도 산출물(`entity_relations`, `actions`)을 만들 뿐이며, 기존 엔티티 추출 재가동 여부는 이번 설계의 범위 밖이다.

---

## 11. 프라이버시

액션·관계 추출(Part A)은 통화 전사와 SMS 원문을 원격 LLM(DeepSeek)에 전송한다. PR #175에서 PII 마스킹이 플래그화되고 기본값이 off로 설정되었으므로 현행 정책상 이 전송은 허용된다.

다만 **요약(제목 한 줄 생성)과 이번 액션 추출은 전송량이 질적으로 다르다.** 요약은 문서 하나의 내용을 한 줄로 압축하면 되지만, 액션 추출은 정확도를 내려면 스레드/대화 맥락 전체(§7.1의 `thread_key` 단위 문맥)를 함께 전송해야 하는 경우가 많다 — 즉 단일 문서가 아니라 관련 대화 다발이 원격 LLM에 노출될 수 있다. 이 사실을 명시적으로 기록한다. 마스킹을 활성화하면 이 전송량 자체는 줄어들지 않지만 민감 토큰(OTP 등)이 제거되므로, 액션·관계 추출의 정확도가 낮아질 가능성이 있다 — 마스킹 on/off와 추출 정확도의 트레이드오프는 구현 단계에서 실측으로 재평가한다.

---

## 12. 구현 계획 분할 권고

이 설계는 4개 영역(A: DB 스키마 + LLM 추출 워커, B: Neo4j 인프라 + 그래프 프런트엔드, C: 액션 로직 + 브리핑 프런트엔드, D: 검색 가중치 최적화기 + 평가 인프라)에 걸쳐 있고, 각 영역이 서로 다른 기술 스택(Go 워커/PostgreSQL, Neo4j/그래프 시각화 라이브러리, SQL 구조 신호/LLM 병합, 배치 최적화/eval)과 서로 다른 배포 대상(Mac mini, ubuntu1)을 갖는다. 단일 구현 계획서 하나로 이 네 영역을 모두 감당하면 다음 문제가 생긴다.

- A(추출 스키마·워커)가 끝나야 B·C가 시작 가능한 순차 의존이 있는 반면, D는 A·B·C와 무관하게 `/ask` 백엔드(§10-2)만 있으면 독립적으로 진행 가능하다 — 하나의 계획서에 묶으면 이 독립성이 가려진다.
- B(그래프 시각화)와 C(액션·브리핑 UI)는 둘 다 프런트엔드 작업을 포함하지만 서로 다른 화면이고 서로 다른 데이터 소비 패턴(그래프 탐색 vs. 목록형 브리핑)을 가진다.
- D는 전제 조건인 `/ask` 백엔드 자체가 별도 구현 계획을 필요로 하므로(§10-2), A/B/C와 같은 착수 시점에 계획을 세울 수 없다.

**권고**: 이 설계 문서 하나를 기반으로 최소 3개의 구현 계획으로 분할한다 — (1) A+B(추출 파이프라인과 그래프, 데이터 의존이 강하므로 하나로 묶는 것이 자연스럽다), (2) C(액션·브리핑, A의 산출물에 의존하지만 B와는 독립적이므로 별도 계획이 가능하다), (3) D(피드백 루프, `/ask` 백엔드 구현 계획이 먼저 나온 뒤 그 위에 얹는 후속 계획). 착수 순서는 A+B → C(A 완료 후 병행 가능) → `/ask` 백엔드 구현 계획 → D 순으로 제안하되, 최종 우선순위와 일정은 이 문서의 범위가 아니다.

---

## 13. 미결 사항

이 절은 본문에서 "구현 계획 단계에서 확정"으로 표기한 항목을 모은 것이다 — 아래 항목은 이 설계 문서가 결정을 내리지 않은 것이며, 별도 논의 없이 구현자가 임의로 정할 수 있는 세부 사항이다.

| 항목 | 관련 절 |
|---|---|
| Part B 그래프 렌더링 라이브러리 최종 확정(번들 크기 측정 후) | §6.3 |
| Part B 진입점 상위 엔티티 개수 N의 정확한 값 | §6.3 |
| `actions.identity_key`의 "정규화 요약" 정확한 정규화 규칙(공백/대소문자/구두점 처리 등) | §5.5 |
| Part C "90일 경과 시 자동 보관"이 `action_status`에 새 state를 추가하는 것이 아니라 조회 쿼리 필터링이라는 해석(§7.4에서 이미 그렇게 확정해 문서화했으나, 신규 state 도입 여지가 완전히 배제된 것은 아니며 구현 단계에서 재확인 필요) | §7.4 |
| `entity_relations`/`actions`의 `confidence` 값 스케일(0–1 연속값인지 범주형인지)과, 브리핑·그래프 화면에서 신뢰도 하한 필터의 기본값 | §6.3, §7.3 |
| Part A 초기 배치(최근 30일) 이후 과거 문서로 확장하는 정확한 시점·기준 | §5.1 |
| Part D 최적화기의 좌표 탐색 알고리즘 세부(스텝 크기, 종료 조건 등) | §8.3 |

---

## 14. 요약

**확정**: A(추출, 문서당 LLM 1회, 닫힌 관계 어휘 8종, 결정적 `identity_key`) → B(Neo4j 파생 투영, ubuntu1, 1홉 확장 화면)·C(구조 신호 + LLM 병합, 온디맨드 브리핑 + 근거 강제)가 A에 의존하는 구조라는 것(§4). D(`/ask` 근거 단위 피드백 → 고정 분할 평가셋 → 좌표 탐색 최적화기 → 30개 미만은 제안만, §8)가 A/B/C와 데이터 의존은 없지만 `/ask` 백엔드라는 별도 전제를 갖는다는 것(§10). 프라이버시상 전송량이 요약과 질적으로 다르다는 것(§11). 최소 3개 구현 계획(A+B, C, D)으로의 분할이 필요하다는 것(§12).

**미확정**: 그래프 렌더링 라이브러리, 진입점 N값, `identity_key` 정규화 규칙, confidence 스케일, 과거 확장 시점·기준, 최적화기 탐색 세부(§13).
