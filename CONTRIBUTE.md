# 기여 가이드

second-brain 프로젝트에 기여해 주셔서 감사합니다. 이 문서는 로컬 개발 환경 구성부터 PR 제출까지의 실제 워크플로를 설명합니다.

---

## 개발 환경 설정

### 사전 요구사항

| 도구 | 버전 |
|------|------|
| Go | 1.26+ |
| PostgreSQL | 16 (pgvector + pg_bigm 확장 포함) |
| Docker / docker compose | 최신 안정 버전 |

### Git Hook 설치

저장소 클론 후 한 번만 실행합니다.

```bash
make install-hooks
```

이 명령은 `.githooks/pre-push` hook을 활성화합니다. 이후 `git push` 시 아래 검사가 자동으로 실행됩니다.

- `scripts/ci-checks.sh` — secret 노출, `.env` 추적, kustomization 가드 등 정책 검사
- `go vet` / `go build` / `go test`

---

## 로컬 실행

환경 변수는 `.env.example`을 참고하여 `.env` 파일을 작성하거나 직접 export합니다.

```bash
# 설정 마법사로 .env 생성 (권장)
go run -tags setup ./cmd/collector/ setup

# 또는 수동 설정
cp .env.example .env
# .env 파일을 편집하여 DATABASE_URL, EMBEDDING_API_KEY 등 필수 값 입력
```

### API 서버 (`cmd/server`)

```bash
export DATABASE_URL="postgres://brain:brain@localhost:5432/second_brain?sslmode=disable"
export EMBEDDING_API_KEY="sk-..."

go run ./cmd/server/
# 기동 시 migrations/ 의 SQL 마이그레이션이 자동 적용됩니다.
```

### 수집 데몬 (`cmd/collector`)

```bash
export FILESYSTEM_PATH="/path/to/docs"
export FILESYSTEM_ENABLED=true

go run ./cmd/collector/
```

---

## 코드 스타일

- `gofmt -w .` 로 포맷을 맞춥니다.
- `go vet ./...` 경고가 없어야 합니다.
- 그 외에는 표준 Go 관례(Effective Go, Go Code Review Comments)를 따릅니다.
- 주석과 변수명은 영어로 작성합니다.

---

## 테스트

```bash
# 기본 테스트
go test ./...

# 경쟁 조건 검사 포함
go test -race ./...

# CI와 동일한 전체 hygiene 검사
make check
```

PR을 열기 전에 `make check` 가 통과하는지 확인하세요.

---

## 커밋 규칙

[Conventional Commits](https://www.conventionalcommits.org/) 형식을 사용합니다.

```
<type>: <subject>

[optional body]
```

| 타입 | 용도 |
|------|------|
| `feat` | 새로운 기능 |
| `fix` | 버그 수정 |
| `docs` | 문서 변경 |
| `chore` | 빌드, 의존성, 설정 변경 |

- subject(제목)는 명령형 현재시제 영어로 작성합니다 (예: `add hybrid search endpoint`).
- 본문(body)이 필요한 경우 영어로 작성합니다.
- 예시: `feat: add per-chunk embedding pipeline`

---

## PR 절차

1. `main` 에서 짧은 수명의 브랜치를 만듭니다.
2. 작업 후 `make check` 를 실행하여 모든 검사를 통과하는지 확인합니다.
3. PR을 열면 pre-push hook이 자동으로 검증을 수행합니다.
4. 브랜치는 머지 후 즉시 삭제합니다 (1–2일 이내 머지를 목표로 합니다).
5. 긴급 우회가 필요한 경우 `git push --no-verify` 를 사용할 수 있지만, 그에 따른 책임은 우회한 사람에게 있습니다.

---

## 라이선스

이 저장소에 기여를 제출하면 해당 기여가 [MIT License](LICENSE) 하에 배포되는 것에 동의하는 것으로 간주합니다.
