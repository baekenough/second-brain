# Mac mini Sizing Reference

Last updated: 2026-04-13

---

## 개요

second-brain을 Mac mini에서 영구 운영할 때 SKU 선택 기준을 제공한다.
`resource-verification.md`의 2주 측정 결과를 이 문서의 매핑 표와 대조하여 발주한다.

---

## 1. Mac mini 라인업 (2024-2025)

| 모델 | CPU 코어 | GPU 코어 | RAM | SSD | 가격 (USD) |
|---|---|---|---|---|---|
| M4 | 10C | 10C | 16 GB | 256 GB | $599 |
| M4 | 10C | 10C | 16 GB | 512 GB | $799 |
| M4 | 10C | 10C | 24 GB | 512 GB | $999 |
| M4 Pro | 12C | 16C | 24 GB | 512 GB | $1,399 |
| M4 Pro | 14C | 20C | 48 GB | 1 TB | $1,999 |
| M4 Pro | 14C | 20C | 64 GB | 2 TB | $2,499 |

비고:
- 10 Gbps Ethernet 옵션: +$100 (20명 내부 트래픽 기준 불필요)
- SSD는 BTO(Built-to-Order) 구성으로 일부 조합 가능
- M4 Pro 12C 모델은 메모리 48 GB 구성 불가

---

## 2. RAM 매핑

측정 기준: `resource-verification.md § 5` 결과표의 Day 14 `minikube VM 피크 RAM` (pg RSS + brain RSS + web RSS + VM 오버헤드 합산).

| minikube VM 피크 RAM | 권장 Mac mini | 비고 |
|---|---|---|
| < 6 GB | 16 GB (M4) | 최소선, macOS 여유분 확보 어려움. OS + Docker 기본 ~6-8 GB 소비 |
| 6-10 GB | **24 GB (M4 or M4 Pro)** | 20명 팀 일반 케이스. 권장 시작점 |
| 10-20 GB | **M4 Pro 48 GB** | 대용량 Slack + Drive 1M+ 문서 예상 시 |
| > 20 GB | M4 Pro 64 GB 또는 아키텍처 재검토 | chunks 테이블 + 대규모 IVFFlat 인덱스 포함 |

결정 원칙: 측정 피크값에서 한 단계 상위 SKU 선택 (운영 여유분 확보).

예시:
- 피크 7 GB → 16 GB 아닌 24 GB
- 피크 12 GB → 24 GB 아닌 48 GB

---

## 3. 저장소 매핑

측정 기준: Day 14 `DB size` + Google Drive Desktop 로컬 캐시 크기 합산.

| DB + Drive cache | 권장 SSD | 비고 |
|---|---|---|
| < 60 GB | 256 GB | 매우 작은 Drive, 초기 단계 |
| 60-150 GB | 512 GB | 일반 케이스 |
| 150-400 GB | **1 TB** | 대용량 Drive 공유 드라이브 포함 시 |
| > 400 GB | 2 TB | 멀티미디어 파일 다수, chunks 테이블 포함 예상 |

비고:
- Google Drive Desktop은 기본 스트리밍 모드(on-demand)로 동작하나,
  `minikube mount` 경유 접근 시 실제 파일 다운로드 발생 → 디스크 사용량 급증 가능
- Postgres PVC + IVFFlat 인덱스는 문서 1M건 기준 약 20-50 GB 예상 (embedding 차원수에 따라 다름)
- 외장 디스크(NAS)를 pg_dump 백업 전용으로 사용하면 내장 SSD 부담 경감

---

## 4. CPU 매핑

측정 기준: Day 0 초기 full scan 중 CPU 평균 사용률 (수 시간 지속 구간).

| 초기 full scan CPU 평균 | 권장 | 비고 |
|---|---|---|
| < 40% | M4 일반 충분 | 여유 있음 |
| 40-70% | M4 (적정) | 증분 수집은 훨씬 낮을 것 |
| > 70% 수 시간 지속 | **M4 Pro** | 발열 + 지속 성능 안정성 필요 |

비고:
- Apple Silicon은 P-core / E-core 혼합 구조로 단순 % 비교는 참고치
- extractor(PDF/xlsx 파싱) 스파이크는 짧고 높음 → 평균보다 피크 관찰이 중요
- M4 Pro는 동일 코어 수 대비 발열 제어와 지속 성능이 우수

---

## 5. 운영 비용

| 항목 | 월 비용 | 비고 |
|---|---|---|
| Mac mini 전기료 | ~500원 | 평균 소비전력 20W, kWh 130원 기준 |
| 인터넷 | 0 | 기존 회선 공유 |
| OpenAI 증분 임베딩 (20명 팀) | $3-10 | text-embedding-3-small, 일 100-300 신규 문서 가정 |
| cloudflared free tunnel | 0 | Quick Tunnel (URL 고정 불가) |
| cloudflared Zero Trust 무료 플랜 | 0 | Named Tunnel, 사용자 50명 이하 무료 |
| 합계 | **~1-1.5만원** | OpenAI 비용 변동폭 포함 |

OpenAI 비용 산정 근거:
- text-embedding-3-small: $0.02 / 1M tokens
- 문서 1건 평균 500 tokens
- 일 200건 신규 → 월 6,000건 → 월 3M tokens → $0.06/월
- 초기 full scan (1M 문서): 500M tokens → $10 (1회성)

---

## 6. 5년 총소유비용 (TCO) 비교

| 옵션 | 초기 비용 | 월 운영비 | 5년 합계 |
|---|---|---|---|
| AWS `m6i.2xlarge` (8C/32G) + RDS db.t3.medium | $0 | ~$170 | ~$10,200 |
| Mac mini M4 24 GB + 512 GB | $999 | ~$5 | ~$1,299 |
| Mac mini M4 Pro 48 GB + 1 TB | $1,999 | ~$5 | ~$2,299 |
| Mac mini M4 Pro 64 GB + 2 TB | $2,499 | ~$5 | ~$2,799 |

비고:
- AWS 비용은 on-demand 기준, Reserved Instance 1년 약정 시 ~30% 절감
- RDS Multi-AZ, 백업, 데이터 전송 비용 미포함 시 실제 더 높음
- Mac mini: 하드웨어 장애, 백업, HA는 본인 책임
- 5년 후 Mac mini 재구매 시 새 모델 성능 향상으로 동일 예산 대비 성능 개선 기대

**차액 (M4 Pro 48 GB vs AWS):** 약 $7,900 (Mac mini 우위)

---

## 7. 대체 시나리오

| 옵션 | 가격 | 특징 | 적합 여부 |
|---|---|---|---|
| 중고 Mac Studio M2 Max 32 GB | $1,500-2,000 | 성능 충분, 발열 안정, M2 세대 | 적합 (예산 절약 시) |
| Mac Studio M4 Max 64 GB | $3,000+ | 여유 풍부, 과스펙 가능 | 적합 (여유 우선 시) |
| 클라우드 VM (월 $300) | $0 초기 | 관리 편의, HA 내장 | 5년 TCO 불리 |
| NUC / Intel mini PC + Linux | $500-800 | 최저가 | ARM vs x86 이미지 복잡, macOS/Drive 통합 불가 |
| Raspberry Pi 5 (8 GB) | ~$100 | 극저가 | RAM 부족, 운영 리스크 높음 |

macOS 환경이 필요한 이유 (Google Drive Desktop):
- Google Drive Desktop은 macOS / Windows 전용
- Linux에서는 `rclone` 등 대안 사용 가능하나 실시간 sync 신뢰도 낮음
- 현재 `minikube mount` + Google Drive Desktop 조합이 가장 안정적

---

## 8. 결정 체크리스트

`resource-verification.md` 2주 측정 완료 후 아래를 순서대로 확인한다.

- [ ] Day 14 minikube VM 피크 RAM → § 2 RAM 매핑 적용
- [ ] Day 14 DB size + Drive cache → § 3 저장소 매핑 적용
- [ ] Day 0 full scan CPU 평균 → § 4 CPU 매핑 적용
- [ ] OpenAI 월 비용 확정 → § 5 운영 비용 업데이트
- [ ] 피크값 기준 한 단계 상위 SKU 선택 (여유분)
- [ ] § 6 TCO로 클라우드 대비 비용 확인
- [ ] § 7 대체 시나리오 검토 (필요 시)
- [ ] 발주 → `deployment-plan.md Phase 3` 진행

---

## 참조

- `resource-verification.md` — 2주 측정 플랜 및 결과 기록 표
- `deployment-plan.md` — Mac mini 이식 전체 로드맵
