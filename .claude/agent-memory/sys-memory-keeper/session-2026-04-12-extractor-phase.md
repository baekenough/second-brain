---
name: session-2026-04-12-extractor-phase
description: 2026-04-12 세션 요약 — 컨텐츠 추출기 구현 및 강제 재처리 진행
type: project
---

## 세션 개요

**날짜**: 2026-04-12
**브랜치**: main
**주요 작업**: Content Extractor 패키지 구현, Go Drive API export 스캐폴드, 4133건 재처리 트리거

## 완료된 작업

### 1. JSON tag fix 커밋 (`7d6769f`)
- `internal/model/document.go`의 Document/SearchResult 구조체에 snake_case JSON 태그 추가
- 웹 프론트엔드 API 계약과 일치시킴

### 2. Content Extractor 패키지 구현 (미커밋)
- 신규 패키지: `internal/collector/extractor/`
  - `extractor.go` — 공통 인터페이스 및 MIME 타입 기반 디스패처
  - `html.go` — golang.org/x/net/html 기반 태그 제거 (108,819자 → 수 KB 기대)
  - `pdf.go` — ledongthuc/pdf 기반 (타임아웃 로직 포함)
  - `docx.go` — OOXML unzip 기반 word/document.xml 추출
  - `xlsx.go` — excelize/v2 기반 셀 값 추출
  - `pptx.go` — OOXML unzip 기반 ppt/slides/*.xml 추출
- 수정: `internal/collector/filesystem.go` — 파일 저장 전 extractor 호출
- 수정: `cmd/server/main.go` — DriveExporter 와이어링
- 수정: `go.mod`, `go.sum` — 새 의존성 추가

### 3. Google Drive Export 스캐폴드 (`internal/collector/gdrive_export.go`)
- ADC(Application Default Credentials) 기반 Drive API export 구현
- `.gdoc/.gsheet/.gslides/.gform` → Google Drive `files.export` API 활용
- 현재 credentials 미설정으로 disabled 상태 (조건부 활성화 코드 포함)
- **Why**: Google Drive는 로컬 sync 폴더(~/Google Drive/공유 드라이브/Vibers.AI)를 FS로 스캔하되, .gdoc 등 네이티브 파일은 Drive API export 필요

### 4. 강제 재처리 트리거
- `UPDATE documents SET collected_at='1970-01-01' WHERE source_type='filesystem'`
- `POST /api/v1/collect/trigger` 호출
- 4133건 재추출 진행 중 (세션 종료 시점 임베딩 ~43%)

## 새 의존성 (go.mod)

```
golang.org/x/net       — HTML 파서
github.com/ledongthuc/pdf — PDF 텍스트 추출
github.com/xuri/excelize/v2 — XLSX 처리
golang.org/x/oauth2    — OAuth2 (Drive API)
google.golang.org/api  — Google API client
```

## 서버 상태 (세션 종료 시점)

- 바이너리: `/tmp/vibers-brain-server`
- PID: 28364
- 포트: :9200
- 로그: `/tmp/vibers-brain.log`
- COLLECT_INTERVAL: 5m (.env 변경, 미커밋)
- DB: docker `vibers-brain-postgres-1` (pgvector/pg16, user=brain, db=vibers_brain)
