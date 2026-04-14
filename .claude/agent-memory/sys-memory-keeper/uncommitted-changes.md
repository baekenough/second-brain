---
name: uncommitted-changes
description: 2026-04-12 세션 미커밋 변경사항 — 다음 세션에서 커밋/정리 필요
type: project
---

## 미커밋 상태 (2026-04-12 세션 종료 시점)

### 수정된 파일 (M)

| 파일 | 변경 내용 | 커밋 포함? |
|------|----------|-----------|
| `CLAUDE.md` | 사용자 omcustom 변경 | 별도 확인 필요 |
| `internal/collector/filesystem.go` | extractor 패키지 통합 | YES — 커밋 필요 |
| `cmd/server/main.go` | DriveExporter 와이어링 | YES — 커밋 필요 |
| `go.mod` | 신규 의존성 추가 | YES — 커밋 필요 |
| `go.sum` | 의존성 체크섬 | YES — 커밋 필요 |
| `web/tsconfig.tsbuildinfo` | 빌드 산출물 | 무시 (.gitignore 추가 권장) |
| `.claude/rules/*.md` | 사용자 omcustom 변경 | 별도 확인 필요 |
| `.env` | COLLECT_INTERVAL=5m | gitignore 대상 — 커밋 안 함 |

### 신규 파일 (??)

| 파일/디렉토리 | 변경 내용 | 커밋 포함? |
|-------------|----------|-----------|
| `internal/collector/extractor/extractor.go` | 추출기 인터페이스 | YES |
| `internal/collector/extractor/html.go` | HTML 추출기 | YES |
| `internal/collector/extractor/pdf.go` | PDF 추출기 | YES |
| `internal/collector/extractor/docx.go` | DOCX 추출기 | YES |
| `internal/collector/extractor/xlsx.go` | XLSX 추출기 | YES |
| `internal/collector/extractor/pptx.go` | PPTX 추출기 | YES |
| `internal/collector/gdrive_export.go` | Drive API export (disabled) | YES |
| `.claude/` | omcustom 구성 | 별도 확인 |
| `.mcp.json` | MCP 서버 설정 | 별도 확인 |
| `guides/` | 가이드 문서 | 별도 확인 |

### 권장 커밋 메시지

```
feat: implement content extractor for HTML/PDF/DOCX/XLSX/PPTX files

- Add internal/collector/extractor package with format-specific extractors
- HTML: strip tags using golang.org/x/net/html parser
- PDF: extract text via ledongthuc/pdf with timeout handling
- DOCX/PPTX: OOXML-based XML extraction
- XLSX: cell value extraction via excelize/v2
- Add Google Drive API export scaffold (ADC-based, disabled pending credentials)
- Wire extractor into filesystem collector pipeline
```

### 선행 조건

- 재처리 4133건 완료 확인 후 커밋 (진행 중인 작업 안정화)
- `web/tsconfig.tsbuildinfo` `.gitignore`에 추가 권장

### .env 변경 (커밋 안 함)

```
COLLECT_INTERVAL=1h → 5m
```

이 변경은 `.env` 파일로 로컬에만 존재. 팀 공유 시 `.env.example` 업데이트 고려.
