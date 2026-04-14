# Resource Verification Plan

Last updated: 2026-04-13

---

## 1. 배경과 목표

second-brain은 팀 20명 이하를 대상으로 Slack + Google Drive 문서를 수집하고 벡터 검색을 제공하는
사내 RAG 시스템이다. 현재는 MacBook Pro M4 Max (48 GB RAM, 926 GB SSD) 위에서 minikube로 운영 중이며,
2주간 자원 측정을 통해 Mac mini SKU를 결정한다.

### 전제 조건

- minikube VM 상한: 8 GB RAM, 4 vCPU (Mac mini base 모델 가정)
- 현재 수집 실적: filesystem 4,152 docs + Slack 1,801 docs = 5,953 docs
- COALESCE 패치로 재수집 중 embedding 손실 방지 적용 완료
- 컨테이너 이미지: second-brain:dev (~34 MB), vibers-web:dev (~195 MB)
- 데이터 볼륨 예상: Slack + Drive 합산 1-2M 문서 (장기)

---

## 2. 측정 대상 지표

### 2-1. 메모리

| 지표 | 단위 | 수집 방법 |
|---|---|---|
| Postgres Pod RSS (피크) | MB | `kubectl top pods` |
| Postgres Pod RSS (평균) | MB | 1분 간격 로그 |
| second-brain Pod RSS | MB | `kubectl top pods` |
| extractor 스파이크 RSS | MB | 대형 PDF 처리 중 관찰 |
| vibers-web Pod RSS | MB | `kubectl top pods` |
| minikube VM total used | MB | `minikube ssh -- free -m` |

### 2-2. CPU

| 지표 | 단위 | 관찰 구간 |
|---|---|---|
| 전체 CPU 사용률 | % | 초기 full scan |
| 증분 수집 CPU | % | 일일 cron 실행 시 |
| 검색 요청 CPU | % | 피크 검색 시 |
| 임베딩 호출 CPU | % | OpenAI 응답 처리 시 |

### 2-3. 스토리지

| 지표 | 단위 | 비고 |
|---|---|---|
| DB 전체 크기 | GB | `pg_database_size` |
| tsvector GIN 인덱스 크기 | MB | `pg_relation_size` |
| pgvector IVFFlat 인덱스 크기 | MB | `pg_relation_size` |
| Drive mount 캐시 크기 | GB | Google Drive Desktop |

### 2-4. 처리 시간

| 지표 | 단위 | 비고 |
|---|---|---|
| 초기 full scan 완료 시간 | 시간 | Slack rate limit 포함 |
| 증분 수집 주기당 소요 | 분 | 일일 실행 기준 |
| 검색 `took_ms` p50 | ms | API 응답 필드 |
| 검색 `took_ms` p95 | ms | 5개 쿼리 반복 기준 |

### 2-5. 비용

| 지표 | 단위 | 비고 |
|---|---|---|
| OpenAI 임베딩 호출 수 | 건 | 초기 full scan 합산 |
| OpenAI 월 추정 비용 | USD | text-embedding-3-small 기준 |

---

## 3. 측정 명령 (복붙 가능)

### 3-1. metrics-server 설치

```bash
minikube addons enable metrics-server
```

활성화 확인:

```bash
kubectl top nodes
kubectl top pods -n second-brain
```

### 3-2. 실시간 Pod 자원 모니터링

```bash
watch -n 5 'kubectl top pods -n second-brain'
```

### 3-3. minikube VM 레벨 1분 간격 로깅

```bash
(while true; do
  date
  minikube ssh -- "free -m | head -2; echo; uptime"
  echo "---"
  sleep 60
done) > ~/minikube-stats.log &
```

백그라운드로 실행되며 `~/minikube-stats.log`에 누적된다.
중단: `kill %1` 또는 `pkill -f minikube-stats`

### 3-4. DB 사이즈 및 행 수 추이 로깅

```bash
while true; do
  date
  kubectl -n second-brain exec statefulset/postgres -- \
    psql -U brain -d second_brain -c "
      SELECT pg_size_pretty(pg_database_size('second_brain')) AS db,
             (SELECT COUNT(*) FROM documents) AS rows,
             (SELECT COUNT(*) FILTER (WHERE embedding IS NOT NULL) FROM documents) AS embedded;"
  sleep 60
done | tee ~/db-growth.log
```

### 3-5. 인덱스 크기 스냅샷

```bash
kubectl -n second-brain exec statefulset/postgres -- \
  psql -U brain -d second_brain -c "
    SELECT indexname, pg_size_pretty(pg_relation_size(indexrelid)) AS size
    FROM pg_indexes i JOIN pg_class c ON c.relname = i.indexname
    WHERE tablename='documents' ORDER BY pg_relation_size(indexrelid) DESC;"
```

### 3-6. 검색 latency 벤치마크

```bash
export API_KEY=<your-api-key>
for q in "회의록" "OKR" "계약서" "미팅" "BBQ"; do
  echo "Query: $q"
  time curl -sS -X POST http://localhost:9200/api/v1/search \
    -H 'Content-Type: application/json' \
    -H "Authorization: Bearer $API_KEY" \
    -d "{\"query\":\"$q\",\"limit\":10,\"sort\":\"relevance\"}" | jq '.took_ms'
  echo "---"
done
```

p95를 구하려면 동일 쿼리를 20회 반복 후 결과를 정렬한다:

```bash
export API_KEY=<your-api-key>
for i in $(seq 1 20); do
  curl -sS -X POST http://localhost:9200/api/v1/search \
    -H 'Content-Type: application/json' \
    -H "Authorization: Bearer $API_KEY" \
    -d '{"query":"회의록","limit":10,"sort":"relevance"}' | jq '.took_ms'
done | sort -n | awk 'NR==19'   # p95 (20개 중 19번째)
```

### 3-7. 단일 스냅샷 (Day N 기록용)

```bash
#!/bin/bash
echo "=== Snapshot $(date) ==="
echo "-- Pod resources --"
kubectl top pods -n second-brain
echo ""
echo "-- VM memory --"
minikube ssh -- "free -m | head -2"
echo ""
echo "-- DB size --"
kubectl -n second-brain exec statefulset/postgres -- \
  psql -U brain -d second_brain -c \
  "SELECT pg_size_pretty(pg_database_size('second_brain')) AS db,
          COUNT(*) AS rows,
          COUNT(*) FILTER (WHERE embedding IS NOT NULL) AS embedded
   FROM documents;"
echo "=== End Snapshot ==="
```

---

## 4. 측정 기간

| 시점 | 목적 | 비고 |
|---|---|---|
| Day 0 | 초기 full scan 피크 자원 + 소요 시간 | Slack rate limit으로 2-5시간 걸릴 수 있음 |
| Day 1 | full scan 직후 안정 상태 | embedding 백필 완료 대기 |
| Day 2-6 | 일상 증분 수집 관찰 | 일일 스냅샷 |
| Day 7 | 1주차 중간 점검 | 스냅샷 표 기입 |
| Day 8-13 | 2주차 관찰 | 이상 패턴 확인 |
| Day 14 | 최종 스냅샷 | SKU 결정 근거 확정 |

최소 2주 권장: Slack rate limit (Tier 3 기준 50 req/min)으로 초기 수집이 수 시간 소요되며,
embedding 백필은 그 이후 별도 진행된다.

---

## 5. 측정 결과 기록 표

아래 표를 복사하여 측정일마다 채운다.

| Day | DB size | rows | embedded | pg RSS (MB) | brain RSS (MB) | CPU% | p95 ms | 비고 |
|:---:|--------:|-----:|---------:|------------:|---------------:|-----:|-------:|------|
|  0  |         |      |          |             |                |      |        | full scan 시작 |
|  1  |         |      |          |             |                |      |        |      |
|  2  |         |      |          |             |                |      |        |      |
|  3  |         |      |          |             |                |      |        |      |
|  4  |         |      |          |             |                |      |        |      |
|  5  |         |      |          |             |                |      |        |      |
|  6  |         |      |          |             |                |      |        |      |
|  7  |         |      |          |             |                |      |        | 1주차 점검 |
|  8  |         |      |          |             |                |      |        |      |
|  9  |         |      |          |             |                |      |        |      |
| 10  |         |      |          |             |                |      |        |      |
| 11  |         |      |          |             |                |      |        |      |
| 12  |         |      |          |             |                |      |        |      |
| 13  |         |      |          |             |                |      |        |      |
| 14  |         |      |          |             |                |      |        | 최종, SKU 결정 |

---

## 6. Mac mini 결정 조건

측정 완료 후 아래 기준으로 `sizing-reference.md`를 참조한다.

| 지표 | 참조 섹션 |
|---|---|
| Day 14 minikube VM 피크 RAM | sizing-reference.md § RAM 매핑 |
| Day 14 DB + Drive cache 크기 | sizing-reference.md § 저장소 매핑 |
| full scan CPU 평균 | sizing-reference.md § CPU 매핑 |
| OpenAI 월 추정 비용 | sizing-reference.md § 운영 비용 |

결정 시 피크값 기준으로 한 단계 여유를 두는 것을 권장한다
(예: 피크 8 GB → 16 GB 아닌 24 GB 선택).

---

## 7. 예상 병목 및 대응

| 병목 | 원인 | 대응 |
|---|---|---|
| 초기 Slack scan 2-5시간 | Tier 3 rate limit (50 req/min) | 야간 실행, 로그 모니터링 |
| extractor OOM (대형 PDF/xlsx) | 단일 Pod 내 스파이크 | memory request/limit 상향 또는 job 분리 |
| minikube mount 유실 | 재부팅 시 mount 해제 | launchd 서비스 등록 (deployment-plan.md Phase 6) |
| pgvector IVFFlat 미인덱스 구간 | 100 docs 미만에서 exact scan | 문서 1,000건 초과 후 `CREATE INDEX CONCURRENTLY` |
| DB 디스크 100% | embedding + tsvector 누적 | PVC size 증설 또는 외장 mount |
| 검색 p95 > 500ms | embedding 미생성 행 포함 | `WHERE embedding IS NOT NULL` 필터 확인 |
