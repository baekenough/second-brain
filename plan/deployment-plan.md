# Deployment Plan

Last updated: 2026-04-13

---

## 개요

MacBook Pro M4 Max (현재) → Mac mini (영구 운영) 이식 로드맵.
Phase 0-1은 현재 MacBook에서, Phase 2-7은 Mac mini 수령 후 진행한다.

---

## Phase 0 — 즉시 조치 (현재 MacBook에서)

- [ ] 미커밋 변경 커밋 + push
  - `cmd/server/main.go`, `internal/collector/filesystem.go`, `go.mod`, `go.sum`, `internal/collector/extractor/`, `internal/collector/gdrive_export.go`, `web/eslint.config.mjs` 포함
  - 커밋 메시지 예시: `feat: add API key auth middleware, extractor, gdrive export`
- [ ] OpenAI 영구 API Key 발급
  - platform.openai.com → API Keys → Create new secret key
  - 기존 임시 키(`sk-proj-...` 만료 예정)와 교체
  - Secret 주입: `kubectl -n second-brain create secret generic openai-secret --from-literal=OPENAI_API_KEY=sk-proj-xxx --dry-run=client -o yaml | kubectl apply -f -`
- [ ] cloudflared 설치 및 Quick Tunnel 실행
  ```bash
  brew install cloudflared
  cloudflared tunnel --url http://localhost:9200
  ```
  - 발급된 `https://*.trycloudflare.com` URL 기록 (팀 공유용)
  - Quick Tunnel은 재시작 시 URL이 바뀌므로 영구 사용 시 Named Tunnel 전환 필요
- [ ] API 토큰 인증 미들웨어 확인
  - Bearer 토큰 방식 (`API_KEY` 환경변수)
  - `Authorization: Bearer <API_KEY>` 헤더 없는 요청 401 응답 확인
  - Secret: `kubectl -n second-brain create secret generic api-key-secret --from-literal=API_KEY=<token>`
- [ ] Slack auto-join 활성화
  - Slack App OAuth 스코프에 `channels:join` 추가
  - 수집기 코드에서 공개 채널 자동 join 로직 활성화 여부 확인

---

## Phase 1 — 측정 (2주)

- [ ] `resource-verification.md` 플랜 실행
  - metrics-server 활성화
  - minikube VM 로깅 백그라운드 시작
  - DB 사이즈 로깅 백그라운드 시작
- [ ] Day 0: 초기 full scan 트리거
  ```bash
  kubectl -n second-brain exec deployment/second-brain -- \
    curl -s -X POST http://localhost:8080/api/v1/collect \
    -H "Content-Type: application/json" \
    -d '{"source":"all"}'
  ```
- [ ] Day 0: 피크 자원 + scan 소요 시간 기록
- [ ] Day 1-13: 일일 스냅샷 (`resource-verification.md § 3-7` 스크립트)
- [ ] Day 14: 결과 표 완성
- [ ] OpenAI 월 비용 확정 (platform.openai.com Usage 대시보드)
- [ ] 병목 식별 후 `sizing-reference.md` 기준 SKU 결정

---

## Phase 2 — Mac mini 발주

- [ ] `sizing-reference.md` § RAM / 저장소 / CPU 매핑 기준 SKU 선택
- [ ] Apple Store 또는 공식 리셀러 발주
- [ ] 수령 예상일 확인 (통상 1-5 영업일)
- [ ] 수령 후 Phase 3 진행

---

## Phase 3 — Mac mini 초기 설정

- [ ] macOS 업데이트 (최신 Sequoia 이상)
- [ ] Homebrew 설치
  ```bash
  /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
  ```
- [ ] 필수 도구 설치
  ```bash
  brew install git kubectl minikube cloudflared
  brew install --cask docker          # Docker Desktop
  # 또는 Colima (경량 대안)
  brew install colima
  ```
- [ ] Google Drive Desktop 설치 + 계정 로그인
  - 대용량 공유 드라이브 전체 동기화 대기 (수 시간 ~ 1일)
  - 동기화 완료 전 Phase 5 mount 진행 불가
- [ ] Git SSH 키 설정
  ```bash
  ssh-keygen -t ed25519 -C "second-brain-macmini"
  cat ~/.ssh/id_ed25519.pub   # GitHub에 등록
  ```
- [ ] minikube 자원 설정
  ```bash
  minikube config set memory 8192
  minikube config set cpus 4
  minikube config set driver docker
  ```

---

## Phase 4 — 데이터 및 시크릿 이식

### 4-1. Postgres 덤프 (MacBook에서)

```bash
kubectl -n second-brain exec statefulset/postgres -- \
  pg_dump -U brain second_brain > second-brain-backup.sql
```

덤프 파일 크기 확인:

```bash
ls -lh second-brain-backup.sql
```

### 4-2. 시크릿 목록 기록

아래 값을 안전한 저장소(1Password 등)에 기록한다.

| 항목 | 설명 | 위치 |
|---|---|---|
| Slack Bot Token | `xoxb-...` 형태 | Slack App 관리 페이지 |
| OpenAI API Key | `sk-proj-...` 영구 키 | platform.openai.com |
| API_KEY Bearer | second-brain 내부 인증 토큰 | 자체 생성 |
| cliproxy-auth.json | CliProxy OAuth 자격증명 | 파일 백업 |
| Google 서비스 계정 JSON (있으면) | Drive API 인증 | GCP 콘솔 |

### 4-3. Mac mini로 파일 전송

```bash
# MacBook → Mac mini (같은 네트워크 내)
scp second-brain-backup.sql user@mac-mini.local:~/
scp ~/.config/cliproxy-auth.json user@mac-mini.local:~/
```

---

## Phase 5 — Mac mini 배포

### 5-1. 리포 클론

```bash
git clone git@github.com:baekenough/second-brain.git
cd second-brain
```

### 5-2. minikube 시작

```bash
minikube start
eval $(minikube docker-env)
```

### 5-3. 이미지 빌드

```bash
# second-brain 백엔드
docker build -t second-brain:dev .

# vibers-web 프론트엔드
docker build -t vibers-web:dev -f web/Dockerfile web/
```

### 5-4. Drive mount

Google Drive Desktop 동기화가 완료된 후 실행한다.

```bash
minikube mount \
  --uid=10001 \
  --gid=10001 \
  "~/Google Drive/공유 드라이브/Vibers.AI:/mnt/drive" &
```

mount PID 기록:

```bash
echo $! > ~/minikube-mount.pid
```

### 5-5. 쿠버네티스 리소스 배포

```bash
kubectl apply -k deploy/k8s/
```

### 5-6. 시크릿 주입 (out-of-band)

```bash
# cliproxy OAuth
kubectl -n second-brain create secret generic cliproxy-auth \
  --from-file=auth.json=~/cliproxy-auth.json

# Slack Bot Token
kubectl -n second-brain create secret generic slack-secret \
  --from-literal=SLACK_BOT_TOKEN=xoxb-xxx

# OpenAI API Key
kubectl -n second-brain create secret generic openai-secret \
  --from-literal=OPENAI_API_KEY=sk-proj-xxx

# API Key (Bearer)
kubectl -n second-brain create secret generic api-key-secret \
  --from-literal=API_KEY=<token>
```

### 5-7. Pod ready 대기

```bash
kubectl -n second-brain wait --for=condition=ready pod --all --timeout=120s
kubectl get pods -n second-brain
```

### 5-8. Postgres 복원

```bash
# DB 생성 확인
kubectl -n second-brain exec statefulset/postgres -- \
  psql -U brain -c "SELECT 1 FROM pg_database WHERE datname='second_brain';"

# 복원
cat ~/second-brain-backup.sql | \
  kubectl -n second-brain exec -i statefulset/postgres -- \
  psql -U brain -d second_brain
```

### 5-9. 첫 수집 트리거 + 로그 확인

```bash
# port-forward (별도 터미널)
kubectl -n second-brain port-forward svc/second-brain 9200:8080 &

# 수집 트리거
curl -sS -X POST http://localhost:9200/api/v1/collect \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <API_KEY>" \
  -d '{"source":"all"}' | jq .

# 로그 스트리밍
kubectl -n second-brain logs -f deployment/second-brain
```

---

## Phase 6 — 운영 전환

### 6-1. cloudflared launchd 서비스 (자동 재시작)

```bash
sudo cloudflared service install
# Named Tunnel 사용 시: cloudflared tunnel run <tunnel-name>
sudo launchctl start com.cloudflare.cloudflared
```

Quick Tunnel (임시) launchd 예시 (`~/Library/LaunchAgents/cloudflared-tunnel.plist`):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>cloudflared-tunnel</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/cloudflared</string>
    <string>tunnel</string>
    <string>--url</string>
    <string>http://localhost:9200</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>/tmp/cloudflared.log</string>
  <key>StandardErrorPath</key>
  <string>/tmp/cloudflared.err</string>
</dict>
</plist>
```

```bash
launchctl load ~/Library/LaunchAgents/cloudflared-tunnel.plist
```

### 6-2. minikube mount launchd 서비스

`~/Library/LaunchAgents/minikube-mount.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>minikube-mount</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/minikube</string>
    <string>mount</string>
    <string>--uid=10001</string>
    <string>--gid=10001</string>
    <string>/Users/username/Google Drive/공유 드라이브/Vibers.AI:/mnt/drive</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>/tmp/minikube-mount.log</string>
</dict>
</plist>
```

### 6-3. minikube launchd (부팅 시 자동 시작)

`~/Library/LaunchAgents/minikube-start.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>minikube-start</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/minikube</string>
    <string>start</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>StandardOutPath</key>
  <string>/tmp/minikube-start.log</string>
</dict>
</plist>
```

### 6-4. port-forward launchd

`~/Library/LaunchAgents/vibers-portforward.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>vibers-portforward</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/kubectl</string>
    <string>-n</string>
    <string>second-brain</string>
    <string>port-forward</string>
    <string>svc/second-brain</string>
    <string>9200:8080</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
</dict>
</plist>
```

### 6-5. Postgres 일일 백업 cron

```bash
# crontab -e
0 3 * * * kubectl -n second-brain exec statefulset/postgres -- \
  pg_dump -U brain second_brain > \
  /Volumes/external/second-brain-backup-$(date +\%Y\%m\%d).sql
```

### 6-6. Docker log 로테이션

`~/.docker/daemon.json` (또는 Docker Desktop preferences):

```json
{
  "log-driver": "json-file",
  "log-opts": {
    "max-size": "100m",
    "max-file": "3"
  }
}
```

### 6-7. sleep 방지 및 정전 복구 설정

```bash
# Mac mini sleep 완전 비활성화
sudo pmset -a sleep 0 displaysleep 0 disksleep 0

# 정전 후 자동 부팅
sudo pmset -a autorestart 1

# 설정 확인
pmset -g
```

### 6-8. 재부팅 후 자동 복구 드라이 런

재부팅 후 다음 순서로 서비스가 자동 복구되는지 확인한다.

1. minikube start (launchd)
2. minikube mount (launchd, minikube 시작 후)
3. cloudflared tunnel (launchd)
4. port-forward (launchd)
5. Pod 상태: `kubectl get pods -n second-brain`

---

## Phase 7 — 품질 개선 (1-2주 후)

- [ ] Chunks 테이블 도입 + 청크 임베딩
  - 현재 8 KB 문자 절단 제거
  - `chunks (document_id, chunk_index, content, embedding)` 스키마
  - 검색을 chunks 기준으로 전환
- [ ] `extraction_failures` 재시도 테이블
  - 파싱 실패 문서 별도 추적
  - 재시도 cron (지수 백오프)
- [ ] Slack rate limit 지수 백오프
  - 현재 단순 sleep → `x-rate-limit-retry-after` 헤더 파싱
  - Tier 3 (50 req/min) 기준 동적 조절
- [ ] per-channel `since` 타임스탬프
  - 새 채널 초대 시 역사 손실 방지
  - `channel_cursors (channel_id, last_ts)` 테이블

---

## 참조

- `resource-verification.md` — 2주 측정 플랜
- `sizing-reference.md` — Mac mini SKU 결정 기준
