---
name: phase-roadmap
description: second-brain 4단계 개발 로드맵 — RAG 기초부터 자기진화 루프까지
type: project
---

## 핵심 비전

**"99.9% 정확도 + 0.1% fallback + 자기진화 루프"**

## Phase 0: Baseline 측정 (현재 재처리 완료 후)

기대 지표 변화:
- `.html` 평균 길이: 108,819 → 수 KB (태그 제거 효과)
- `.pdf/.xlsx/.docx/.pptx` 평균 길이: ~170 → 수 KB~수십 KB (실제 컨텐츠 추출)
- 실패율 기록 필요 (특히 PDF OCR 필요 건수)

## Phase 1: RAG 기초 + 버그 수정

### P0 버그
1. **scheduler mutex** — `internal/scheduler/scheduler.go`의 `run()`이 동시 실행 가능
   - 증상: 5m-cron + 수동 trigger 동시 진행 시 `start=1620/1720/1740/1640` 로그 뒤섞임
   - 해결: sync.Mutex 또는 atomic 플래그로 단일 실행 보장

2. **PDF 다단계 fallback** — ledongthuc/pdf가 한국어 PDF에서 10초 타임아웃
   - 대상: "마티니 Performance 세일즈덱.pdf" 등
   - 필요 체인: `ledongthuc/pdf` → `pdftotext`(poppler) → `ocrmypdf`/tesseract → metadata fallback
   - 미결정: ocrmypdf/tesseract 바이너리 번들링 전략

3. **8KB 절단 제거** — `internal/scheduler/scheduler.go:165`
   - 현재: `if len(text) > 8000 { text = text[:8000] }`
   - 문제: 긴 문서 뒷부분 손실
   - 해결: 청크 단위 임베딩으로 대체 (chunks 테이블 필요)

### P1 구현
- **chunks 테이블**: (id, document_id, chunk_index, content, embedding) 마이그레이션
- **extraction_failures 큐**: 실패 파일 재시도 테이블 + 워커

## Phase 2: 의미 강화

- 섹션/헤더 기반 의미 청킹
- LLM 요약 컬럼: `title_summary`, `bullet_summary`
- 요약 별도 임베딩
- **미결정**: 요약용 LLM 모델/API/비용 예산

## Phase 3: 검색 품질

- BGE-reranker cross-encoder
- HyDE 쿼리 확장
- 하이브리드 가중치 자동 튜닝

## Phase 4: 자기진화 루프

- 피드백 테이블: (query, clicked_doc_id, thumbs, user_id, ts)
- eval set 자동 구축
- nightly eval + 회귀 감지
- 임계치 기반 자동 재인덱싱
  - **원칙**: 완전 자율 금지 — 수동 트리거 + 임계치 자동화 조합

## 다음 세션 즉시 작업

1. 재처리 완료 확인 (4133건 임베딩 진행 중)
2. baseline 지표 측정
3. 미커밋 변경사항 커밋 (아래 `uncommitted-changes.md` 참조)
4. CI/릴리즈 인프라 구축 (.github/workflows/, git tag 없음)
5. Phase 1 P0 버그 수정 시작

## 블로커

| 항목 | 상태 | 필요 행동 |
|------|------|----------|
| Google Workspace export | 비활성 | `gcloud auth application-default login --scopes=...drive.readonly` |
| PDF OCR fallback | 미구현 | ocrmypdf/tesseract 번들링 전략 결정 |
| 요약용 LLM | 미결정 | 모델/API/비용 예산 결정 |
| CI/CD | 없음 | .github/workflows/ 구축 필요 |
