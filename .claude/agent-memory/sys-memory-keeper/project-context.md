---
name: project-context
description: second-brain 프로젝트 기본 컨텍스트 — 인프라, 계정, 접근 정보
type: project
---

## 프로젝트 정보

- **경로**: `/Users/sangyi/workspace/baekenough/second-brain`
- **언어**: Go (백엔드 RAG/검색 시스템)
- **GitHub remote**: `git@github.com:baekenough/second-brain.git` (origin)
- **기본 브랜치**: main

## 인프라

### 데이터베이스
- **컨테이너**: `second-brain-postgres-1`
- **이미지**: pgvector/pg16
- **접속**: user=brain, db=second_brain
- **주요 테이블**: documents (source_type, collected_at, content, embedding 등)

### 서버
- **포트**: :9200 (Service/Ingress로 노출)
- **운영 환경**: k8s 네임스페이스 `second-brain`, deployment 이름 `second-brain`. 로그 확인: `kubectl -n second-brain logs <pod-name>`. 사용자는 `kubectl port-forward`로 접근하므로 "로컬"이라 부를 수 있으나 실제로는 k8s.
- **수집 간격**: ConfigMap `second-brain-config`의 `COLLECT_INTERVAL: 5m` (코드 default는 10m이나 ConfigMap이 우선 적용됨 — 변경 시 ConfigMap 직접 수정 필요)
- **Slack 채널 watcher**: `SlackChannelWatcher` goroutine, 60초 polling으로 신규 채널 감지 → `CollectChannel(since=zero)` 자동 호출
- **배포 절차**: `eval $(minikube docker-env) && docker build -t second-brain:dev .` → `kubectl -n second-brain rollout restart deployment/second-brain`
- **pod 내부**: `curl` 없음 — `wget` 사용 또는 `kubectl exec` 후 wget으로 API 확인
- **[DEPRECATED] 네이티브 바이너리**: `/tmp/second-brain-server` — 일회성 디버깅 전용, 정식 배포 아님
- **정식 배포 경로**: Docker 이미지 → minikube deployment (아래 배포 지침 참조)

### 배포 지침 (Docker + minikube)

모든 서비스는 Docker + minikube로 배포. 네이티브 바이너리 실행은 디버깅 전용.

- **로컬 개발**: `docker-compose.yml`
- **스테이징/프로덕션**: minikube + `k8s/` 디렉토리 Kubernetes 매니페스트
- **이미지 태그**: `second-brain:dev` (로컬), `second-brain:v0.1.0` (릴리즈)
- **서버 실행 요청 시**: docker/kubectl 명령어로 응답 (nohup/바이너리 제안 금지)
- **근거**: 2026-04-12 사용자 명시 지침 — feedback-docker-minikube.md 참조

### minikube VM
- **메모리**: 8 GB (2026-04-13 docker update --memory 8g 적용)
- **CPU 상한**: 4 vCPU

### API 인증
- **방식**: Bearer 토큰 (`API_KEY` 환경변수)
- **공개 엔드포인트**: `/health` 만 인증 제외
- **관리**: `.env` (gitignored) + out-of-band kubectl patch

### cloudflared tunnel
- **상태**: Quick Tunnel 활성 (재기동 시 URL 변경됨)
- **현재 URL**: `https://influenced-computational-smaller-sharp.trycloudflare.com` → `localhost:9200`

### 주요 API 엔드포인트
- `POST /api/v1/collect/trigger` — 수동 수집 트리거
- `POST /api/v1/collect/slack/channel` — 단일 Slack 채널 강제 full-history 수집 (body: `{channel_id}` or `{channel_name}`)
- `POST /api/v1/search` — 검색 (Bearer auth 필수; body: `{query, source_type, exclude_source_types, limit, sort, include_deleted}`)
- `GET /api/v1/stats` — 수집 통계

### Slack 수집 설계 원칙

- **DM 비수집 설계**: `internal/collector/slack.go:184`에서 `users.conversations` 호출 시 `types=public_channel,private_channel`로 하드코딩. DM/mpim은 애초에 후보 채널 목록에 포함되지 않음. **의도적 프라이버시 보호 설계** — 이 필터를 변경하면 개인 DM이 수집 대상에 들어올 수 있으므로 주의.
- **봇/앱 분리 원칙**: second-brain-collector 앱과 클로드-써치 응답 봇 앱은 반드시 분리. second-brain 앱에 `im:history`/`mpim:history` scope 절대 부여 금지 (물리적 차단).
- **스레드 많은 채널 특성**: `conversations.replies` Tier 3 rate limit이 발생할 수 있음 (예: #biz-sales-account). backoff 재시도로 자동 복구되며 수집은 완료됨.

### 채널별 수집 특성

- **#biz-sales-account** (C08U4PFFT46): 스레드 매우 많음. 초회 수집 약 4분 24초, 5365건. rate limit backoff 발생하나 정상 완료.

## GitHub 계정

| 계정 | 상태 | 용도 |
|------|------|------|
| sang2-tpm | active | 현재 gh CLI 사용 계정 (baekenough org 소속) |
| baekenough | inactive | 이전 개인 계정 |

gh 계정 전환: `gh auth switch --user baekenough`

## Google Drive 설정

- **로컬 sync 폴더**: `~/Google Drive/공유 드라이브/Vibers.AI`
- **수집 방식**: FS 스캔 (Drive API 전체 수집 금지)
- **네이티브 파일 추출**: `.gdoc/.gsheet/.gslides/.gform` → Drive API `files.export`
- **활성화 조건**: `gcloud auth application-default login --scopes=https://www.googleapis.com/auth/drive.readonly`

## 외부 연동 상태

| 연동 | 상태 | 비고 |
|------|------|------|
| Slack | 활성 | xoxb-8730276019027-... (사용자 제공), bot scope: `channels:read` + `groups:read`. watcher 60s polling 활성. auto-join 비활성 |
| Google Drive | 활성 | FS 스캔 (~/Google Drive/공유 드라이브/Vibers.AI) |
| GitHub | 미활성 (0건) | token 미제공 |
| Notion | 미활성 | API 키 미제공 |
| OpenAI 임베딩 | 임시 | ChatGPT Codex OAuth JWT (8일 만료) — 영구 sk-proj-... 교체 필요 (P0) |

## 커밋 컨벤션

Conventional Commits: `feat:`, `fix:`, `docs:`, `chore:`
브랜치: main 직접 커밋 또는 단기 feature 브랜치 (1-2일 내 머지)
언어: 커밋/PR 본문 한국어 허용 (MUST-language-policy.md R000)
