# Deployment Runbook

> **대상 서버**: `baekenough-ubuntu24` (minikube + docker driver)
> **SSH alias**: `ubuntu24_home_server-ext`
> **외부 URL**: `https://second-brain.baekenough.com`
> **네임스페이스**: `second-brain`
> **책임자**: @sang2-tpm (or rotation)
> **최종 갱신**: 2026-04-14
> **관련 이슈**: #6 #21 #28 #29 #34 #35 #36 #37

---

## 목차

1. [전제 조건](#1-전제-조건)
2. [배포 아키텍처 요약](#2-배포-아키텍처-요약)
3. [배포 절차](#3-배포-절차)
   - [Step 1: 로컬 — 태그 생성](#step-1-로컬--태그-생성)
   - [Step 2: 서버 — 코드 동기화](#step-2-서버--코드-동기화)
   - [Step 3: 서버 — Secret 동기화](#step-3-서버--secret-동기화)
   - [Step 4: 서버 — 이미지 빌드](#step-4-서버--이미지-빌드)
   - [Step 5: 서버 — 개별 매니페스트 적용](#step-5-서버--개별-매니페스트-적용)
   - [Step 6: 서버 — Rollout 재시작](#step-6-서버--rollout-재시작)
   - [Step 7: 검증](#step-7-검증)
4. [DO NOT — 금지 사항](#4-do-not--금지-사항)
5. [Rollback](#5-rollback)
6. [문제 해결](#6-문제-해결)
7. [환경 변수 레퍼런스](#7-환경-변수-레퍼런스)
8. [Filesystem Collector 활성화 (선택)](#8-filesystem-collector-활성화-선택)
9. [관련 문서](#9-관련-문서)

---

## 1. 전제 조건

배포 전 아래 항목을 **모두** 확인한다. 하나라도 누락되면 Step 1로 진행하지 않는다.

| 항목 | 확인 방법 | 기대 결과 |
|------|-----------|-----------|
| SSH alias 설정 | `ssh ubuntu24_home_server-ext echo ok` | `ok` 출력 |
| minikube 실행 중 | `ssh ubuntu24_home_server-ext minikube status` | `host: Running` |
| cliproxy 인증 파일 존재 | `ssh ubuntu24_home_server-ext ls ~/.cli-proxy-api/` | `codex-*.json` 파일 확인 |
| 서버 `.env` 최신 상태 | 서버 `~/second-brain/.env` 수동 확인 | 필수 키 누락 없음 |
| 로컬 git 상태 clean | `git status` | `nothing to commit` |
| main 브랜치 최신 | `git log origin/main..HEAD` | 차이 없음 (또는 배포할 커밋만 존재) |

---

## 2. 배포 아키텍처 요약

```
로컬 머신
  └── git tag → git push origin <tag>
        │
        ▼
baekenough-ubuntu24 (minikube + docker driver)
  ├── minikube namespace: second-brain
  │   ├── Deployment/second-brain      (port 9200, NodePort 30920)
  │   ├── Deployment/second-brain-web  (port 3000, NodePort 30300)
  │   ├── StatefulSet/postgres
  │   ├── ConfigMap/second-brain-config
  │   └── Secret/second-brain-secret
  └── nginx reverse proxy → https://second-brain.baekenough.com
```

**이미지 태그 규칙**

| 이미지 | 태그 | 예시 |
|--------|------|------|
| `second-brain` | 배포 태그 또는 `dev` | `second-brain:v0.1.5` |
| `second-brain-web` | 배포 태그 또는 `dev` | `second-brain-web:v0.1.5` |

> minikube docker 환경 내부에 이미지를 빌드하므로 레지스트리 push 불필요.
> `imagePullPolicy: IfNotPresent` — 이미지 태그가 같으면 pull 시도 없음.
> **따라서 동일 태그 재배포 시 반드시 `--no-cache` 빌드 후 `rollout restart` 필수.**

---

## 3. 배포 절차

### Step 1: 로컬 — 태그 생성

```bash
# 1-1. main 브랜치 최신화
git checkout main
git pull origin main

# 1-2. 변경 사항 커밋 (없으면 생략)
# 커밋 메시지 규약: conventional commits
# feat: / fix: / docs: / chore: / refactor: / perf:
git add -A
git commit -m "feat: <변경 요약>"

# 1-3. 태그 생성 (semver: vMAJOR.MINOR.PATCH)
export TAG=v0.1.6   # 실제 버전으로 교체
git tag -a ${TAG} -m "release ${TAG}"

# 1-4. 태그 포함 push
git push origin main
git push origin ${TAG}

# 1-5. 생성 확인
git tag -l | tail -5
```

> **태그 메시지 예시**: `release v0.1.5 — add Discord bot WebSocket keepalive`

---

### Step 2: 서버 — 코드 동기화

```bash
# 서버 접속
ssh ubuntu24_home_server-ext

# 프로젝트 디렉토리로 이동
cd ~/second-brain   # 실제 서버 경로에 맞게 조정

# 원격 변경 사항 가져오기
git fetch --tags origin

# 배포할 태그로 checkout
export TAG=v0.1.6   # 로컬과 동일한 버전
git checkout ${TAG}

# 현재 상태 검증
git log --oneline -3
git tag --points-at HEAD
```

**검증 기준**: `git tag --points-at HEAD` 출력이 `${TAG}` 여야 한다.

---

### Step 3: 서버 — Secret 동기화

> **중요**: Secret은 `.env` 파일에서 직접 생성한다.
> kustomize(`kubectl apply -k`)는 Secret을 더미 값으로 덮어쓰므로 **절대 사용 금지** (#29).
>
> **권장 (#36)**: 수동 kubectl 명령 대신 `scripts/sync-env.sh`를 사용한다.
> drift 방지를 위해 `.env` 수정 후에는 **반드시** 이 스크립트를 실행한다.

```bash
# 3-1. 자동화 스크립트 사용 (권장)
cd ~/second-brain
bash scripts/sync-env.sh          # 기본: second-brain 네임스페이스
# 또는 rollout restart까지 한번에:
AUTO_ROLLOUT=true bash scripts/sync-env.sh

# 3-2. 수동 방법 (스크립트 사용 불가 시)
# .env 파일 위치 확인 (서버 홈 디렉토리 또는 프로젝트 루트)
ls -la ~/.env 2>/dev/null || ls -la ~/second-brain/.env

# .env에서 second-brain-secret 생성/갱신
kubectl -n second-brain create secret generic second-brain-secret \
  --from-env-file=.env \
  --dry-run=client -o yaml | kubectl apply -f -

# 3-3. 현재 Secret 키 목록 확인 (값은 출력되지 않음)
kubectl -n second-brain get secret second-brain-secret \
  -o jsonpath='{.data}' | python3 -c \
  "import sys, json; d=json.load(sys.stdin); [print(k) for k in d.keys()]"

# 3-4. out-of-band 키 별도 주입 (필요시)
# API_KEY, SLACK_BOT_TOKEN 등은 .env에 없는 경우 patch로 주입
# kubectl patch secret second-brain-secret -n second-brain \
#   --type=merge -p '{"stringData":{"API_KEY":"<값>"}}'

# 3-5. cliproxy-auth Secret 갱신 (auth.json이 변경된 경우만)
kubectl -n second-brain create secret generic cliproxy-auth \
  --from-file=auth.json=~/.cli-proxy-api/codex-<날짜>.json \
  --dry-run=client -o yaml | kubectl apply -f -
```

---

### Step 4: 서버 — 이미지 빌드

> **`--no-cache` 필수**: 캐시된 레이어로 인해 스테일 코드가 이미지에 포함되는 사고 방지 (#28).
> minikube docker 환경에서 빌드해야 k8s Pod가 이미지를 인식한다.

```bash
# 4-1. minikube docker 환경 활성화 (셸 세션당 1회)
eval $(minikube docker-env)

# 4-2. 활성화 확인
docker info | grep "Name:"
# 출력에 minikube 포함되어야 함

# 4-3. Backend 이미지 빌드 (--no-cache 필수)
export TAG=v0.1.6
cd ~/second-brain   # 프로젝트 루트 (Dockerfile 위치)

docker build \
  --no-cache \
  --platform linux/amd64 \
  -t second-brain:${TAG} \
  -t second-brain:dev \
  -f Dockerfile \
  .

# 4-4. Web 이미지 빌드 (--no-cache 필수)
docker build \
  --no-cache \
  --platform linux/amd64 \
  -t second-brain-web:${TAG} \
  -t second-brain-web:dev \
  -f web/Dockerfile \
  web/

# 4-5. 이미지 확인
docker images | grep second-brain
# 예상 출력:
# second-brain      v0.1.6   <ID>   2 minutes ago   ...
# second-brain      dev      <ID>   2 minutes ago   ...
# second-brain-web  v0.1.6   <ID>   1 minute ago    ...
# second-brain-web  dev      <ID>   1 minute ago    ...
```

> **빌드 소요 시간**: Backend ~3-5분, Web ~2-3분 (초기 deps 다운로드 포함).
> `--no-cache`를 사용하더라도 go mod download는 Docker layer cache가 아닌
> Go module cache를 사용하므로 재사용 가능 (정상 동작).

---

### Step 5: 서버 — 개별 매니페스트 적용

> **kustomize 전체 적용(`kubectl apply -k`) 금지** — Secret 덮어쓰기 사고 (#29).
> 개별 파일만 선택적으로 적용한다.

```bash
cd ~/second-brain/deploy/k8s

# 5-1. Namespace (없는 경우만)
kubectl apply -f namespace.yaml

# 5-2. ConfigMap 갱신 (환경변수 변경 있을 때만)
kubectl apply -f second-brain-configmap.yaml

# 5-3. PersistentVolume (선택 — Drive 마운트 환경에서만 적용)
# second-brain-pv.yaml은 kustomization.yaml에서 제외됨. 필요 시 수동 적용.
# 자세한 내용: §8 "Filesystem Collector 활성화"
# kubectl apply -f second-brain-pv.yaml

# 5-4. Deployment 스펙 변경 (리소스 한도, probe 등 변경 있을 때만)
kubectl apply -f second-brain-deployment.yaml
kubectl apply -f second-brain-web-deployment.yaml

# 5-5. Service (포트 변경 있을 때만)
kubectl apply -f second-brain-service.yaml
kubectl apply -f second-brain-web-service.yaml

# 5-6. Postgres (StatefulSet 스펙 변경 있을 때만 — 주의: 데이터 보존 확인)
# kubectl apply -f postgres-statefulset.yaml
# kubectl apply -f postgres-service.yaml
```

> **일반 배포** (코드 변경만): Step 5 전체 생략 가능. Step 6 rollout restart만으로 충분.
> **스펙 변경 배포** (리소스, probe, 환경변수): 해당 파일만 선택적 적용.

---

### Step 6: 서버 — Rollout 재시작

```bash
# 6-1. Backend rollout restart
kubectl rollout restart deployment/second-brain -n second-brain

# 6-2. Web rollout restart
kubectl rollout restart deployment/second-brain-web -n second-brain

# 6-3. Rollout 완료 대기 (최대 3분)
kubectl rollout status deployment/second-brain -n second-brain --timeout=180s
kubectl rollout status deployment/second-brain-web -n second-brain --timeout=180s

# 기대 출력:
# deployment "second-brain" successfully rolled out
# deployment "second-brain-web" successfully rolled out
```

---

### Step 7: 검증

```bash
# 7-1. Pod 상태 확인
kubectl get pods -n second-brain
# 모든 Pod STATUS: Running, READY: 1/1

# 7-2. Backend 헬스체크
curl -sf http://$(minikube ip):30920/health
# 또는 서비스 내부에서:
kubectl exec -n second-brain deploy/second-brain -- \
  wget -qO- http://localhost:9200/health

# 7-3. Web 헬스체크
curl -sf http://$(minikube ip):30300/

# 7-4. 외부 URL 검증
curl -sf https://second-brain.baekenough.com/health

# 7-5. 최신 이미지 사용 확인
kubectl get pod -n second-brain \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.containerStatuses[*].imageID}{"\n"}{end}'

# 7-6. 최근 로그 확인 (오류 없음 확인)
kubectl logs -n second-brain deploy/second-brain --tail=30
kubectl logs -n second-brain deploy/second-brain-web --tail=30
```

**검증 완료 기준**:

| 항목 | 기대 값 |
|------|---------|
| Pod READY | `1/1` (모든 Pod) |
| `/health` 응답 | HTTP 200 |
| 외부 URL 응답 | HTTP 200 |
| 로그 FATAL/PANIC | 없음 |

---

## 4. DO NOT — 금지 사항

> 아래 사항은 **오늘(2026-04-14) 실제 사고**를 근거로 작성했다.
> 규칙이 아니라 사고 방지 체크리스트다.

---

### 절대 금지 (Absolute Prohibitions)

| # | 금지 행동 | 근거 이슈 | 대안 |
|---|-----------|-----------|------|
| 1 | `kubectl apply -k deploy/k8s/` | #29 | 개별 파일 선택 적용 (Step 5) |
| 2 | `docker build` (no-cache 없이) | #28 | `--no-cache` 플래그 필수 |
| 3 | `.env` git commit | 보안 | `.gitignore` 확인 후 서버 직접 편집 |
| 4 | `git push --force` to main | 이력 파괴 | 절대 금지, 예외 없음 |
| 5 | `git commit --no-verify` | CI 우회 | 훅 우회 없이 정상 커밋 |

---

### 상세 설명

#### ❌ `kubectl apply -k deploy/k8s/` 금지

```bash
# 이 명령 절대 사용 금지
kubectl apply -k deploy/k8s/   # NEVER
```

**사고 경위** (#29): `kustomization.yaml`에 `second-brain-secret.example.yaml`이 포함되어 있지 않더라도,
kustomize가 Secret 리소스를 다른 방식으로 처리하거나 향후 포함될 경우 `.env`의 실제 값이
더미 값(`CHANGE_ME`)으로 **덮어써진다**. Secret 갱신은 반드시 Step 3의 `--from-env-file` 방식을 사용한다.

#### ❌ Docker 캐시 빌드 금지

```bash
# 캐시 사용 빌드 — 금지
docker build -t second-brain:dev .   # NEVER (without --no-cache)
```

**사고 경위** (#28): `imagePullPolicy: IfNotPresent`와 결합되면, 태그가 같아도
캐시된 레이어에서 빌드된 구 버전 바이너리가 실행된다. 코드 변경이 반영되지 않는다.

#### ❌ Dockerfile 미커밋 상태 배포 금지

**사고 경위** (#35): 로컬에서 `Dockerfile`을 수정 후 커밋 없이 서버에서 `git checkout <tag>` 하면
서버의 Dockerfile은 수정 전 상태다. **항상 Dockerfile 변경은 커밋 → 태그 → 서버 체크아웃** 순서로.

---

## 5. Rollback

### 즉각 롤백 (이전 revision으로)

```bash
# Backend 롤백
kubectl rollout undo deployment/second-brain -n second-brain

# Web 롤백
kubectl rollout undo deployment/second-brain-web -n second-brain

# 롤백 상태 확인
kubectl rollout status deployment/second-brain -n second-brain
kubectl rollout status deployment/second-brain-web -n second-brain

# revision 이력 확인
kubectl rollout history deployment/second-brain -n second-brain
kubectl rollout history deployment/second-brain-web -n second-brain
```

### 특정 태그로 재배포 (완전 롤백)

```bash
# 1. 이전 태그로 서버에서 코드 체크아웃
ssh ubuntu24_home_server-ext
cd ~/second-brain
export PREV_TAG=v0.1.5   # 롤백할 버전
git checkout ${PREV_TAG}

# 2. 이미지 재빌드 (--no-cache 필수)
eval $(minikube docker-env)
docker build --no-cache --platform linux/amd64 \
  -t second-brain:${PREV_TAG} -t second-brain:dev \
  -f Dockerfile .
docker build --no-cache --platform linux/amd64 \
  -t second-brain-web:${PREV_TAG} -t second-brain-web:dev \
  -f web/Dockerfile web/

# 3. Rollout restart
kubectl rollout restart deployment/second-brain -n second-brain
kubectl rollout restart deployment/second-brain-web -n second-brain

# 4. 검증 (Step 7 동일)
kubectl get pods -n second-brain
curl -sf https://second-brain.baekenough.com/health
```

### Postgres Rollback 주의사항

> StatefulSet/postgres는 **데이터 보존** 우선. Deployment rollout undo 대상이 아니다.
> Postgres 관련 문제는 롤백이 아닌 마이그레이션 검토 또는 백업 복원으로 처리.

---

## 6. 문제 해결

### Pod CrashLoopBackOff

```bash
# 1. Pod 이름 확인
kubectl get pods -n second-brain

# 2. 로그 확인 (이전 컨테이너 포함)
kubectl logs -n second-brain <pod-name> --previous
kubectl logs -n second-brain <pod-name>

# 3. Pod 상세 정보 (이벤트 포함)
kubectl describe pod -n second-brain <pod-name>

# 4. 환경변수 확인 (Secret/ConfigMap 주입 여부)
kubectl exec -n second-brain <pod-name> -- env | sort
```

**원인별 체크리스트**:

| 증상 | 확인 항목 | 조치 |
|------|-----------|------|
| `database connection refused` | `DATABASE_URL` Secret 키 | Step 3 재실행 |
| `migrations: no such file` | `MIGRATIONS_DIR` ConfigMap | `/app/migrations` 설정 확인 |
| `permission denied /app/second-brain` | 이미지 빌드 오류 | `--no-cache` 재빌드 |
| `OOMKilled` | 메모리 리소스 한도 | deployment resources.limits 상향 |

---

### Rollout Stuck (진행 중단)

```bash
# 현재 상태 확인
kubectl rollout status deployment/second-brain -n second-brain

# 이벤트 확인
kubectl describe deployment second-brain -n second-brain | tail -20

# ReplicaSet 상태 확인
kubectl get rs -n second-brain

# 강제 재시작 (마지막 수단)
kubectl delete pod -n second-brain -l app=second-brain
```

---

### LLM 401 Unauthorized (#36)

```bash
# 원인: cliproxy 인증 만료 또는 auth.json 미주입
# 1. cliproxy auth 파일 확인
ls -la ~/.cli-proxy-api/

# 2. Pod 내부 auth.json 존재 확인
kubectl exec -n second-brain deploy/second-brain -- \
  ls -la /etc/cliproxy/

# 3. cliproxy-auth Secret 갱신 후 재시작
kubectl -n second-brain create secret generic cliproxy-auth \
  --from-file=auth.json=~/.cli-proxy-api/codex-<날짜>.json \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl rollout restart deployment/second-brain -n second-brain
```

---

### Embedding 404 — `/v1/embeddings` 미지원 (#34)

```bash
# 원인: cliproxy가 /v1/embeddings 엔드포인트 미지원
# EMBEDDING_API_URL을 실제 OpenAI API로 변경하거나
# EMBEDDING_API_KEY를 직접 주입

# 현재 ConfigMap 확인
kubectl get configmap second-brain-config -n second-brain -o yaml | grep EMBEDDING

# Secret patch로 EMBEDDING_API_KEY 주입
kubectl patch secret second-brain-secret -n second-brain \
  --type=merge -p '{"stringData":{"EMBEDDING_API_KEY":"sk-..."}}'
kubectl rollout restart deployment/second-brain -n second-brain
```

---

### Discord Bot 오프라인

```bash
# 1. WebSocket 연결 로그 확인
kubectl logs -n second-brain deploy/second-brain | grep -i discord

# 2. DISCORD_TOKEN 주입 여부 확인 (값 비공개)
kubectl -n second-brain get secret second-brain-secret \
  -o jsonpath='{.data}' | python3 -c \
  "import sys, json, base64; d=json.load(sys.stdin); \
   print('DISCORD_TOKEN:', 'EXISTS' if 'DISCORD_TOKEN' in d else 'MISSING')"

# 3. 토큰 갱신이 필요한 경우
kubectl patch secret second-brain-secret -n second-brain \
  --type=merge -p '{"stringData":{"DISCORD_TOKEN":"<새 토큰>"}}'
kubectl rollout restart deployment/second-brain -n second-brain
```

---

### minikube 재시작 후 이미지 소실

```bash
# minikube 재시작 후 docker 환경 재설정 필요
eval $(minikube docker-env)
docker images | grep second-brain

# 이미지 없으면 Step 4 (이미지 빌드)부터 재실행
```

---

## 7. 환경 변수 레퍼런스

### ConfigMap (`second-brain-config`) — 공개 설정값

| 변수 | 기본값 | 설명 |
|------|--------|------|
| `PORT` | `9200` | Backend HTTP 포트 |
| `COLLECT_INTERVAL` | `5m` | 수집 주기 |
| `MAX_EMBED_CHARS` | `8000` | 임베딩 최대 문자 수 |
| `EMBEDDING_API_URL` | `https://api.openai.com/v1` | Embedding API 엔드포인트 |
| `EMBEDDING_MODEL` | `text-embedding-3-small` | 임베딩 모델 |
| `CLIPROXY_AUTH_FILE` | `/etc/cliproxy/auth.json` | cliproxy 인증 파일 경로 |
| `FILESYSTEM_PATH` | `/data/drive` | 파일시스템 수집 경로 |
| `FILESYSTEM_ENABLED` | `false` | 파일시스템 수집 활성화 (Drive 마운트 있는 환경에서만 `true`) |
| `MIGRATIONS_DIR` | `/app/migrations` | DB 마이그레이션 경로 |

### Secret (`second-brain-secret`) — 민감 정보 (.env 관리)

| 변수 | 필수 | 설명 |
|------|------|------|
| `DATABASE_URL` | 필수 | PostgreSQL DSN |
| `POSTGRES_PASSWORD` | 필수 | Postgres 비밀번호 |
| `API_KEY` | 권장 | `/api/v1/*` Bearer 토큰 |
| `GITHUB_TOKEN` | 선택 | GitHub 수집 PAT |
| `SLACK_BOT_TOKEN` | 선택 | Slack 수집 토큰 |
| `SLACK_TEAM_ID` | 선택 | Slack 팀 ID |
| `NOTION_TOKEN` | 선택 | Notion 수집 토큰 |
| `EMBEDDING_API_KEY` | 선택 | Embedding API 키 (cliproxy 미사용 시) |
| `DISCORD_TOKEN` | 선택 | Discord 봇 토큰 |
| `GDRIVE_CREDENTIALS_JSON` | 선택 | Google Drive 서비스 계정 JSON |

> **주의**: Secret 값은 이 문서에 절대 포함하지 않는다.
> 실제 값은 서버 `~/.env` 또는 1Password에서 관리한다.

### Web ConfigMap (Deployment 인라인)

| 변수 | 값 | 설명 |
|------|-----|------|
| `BRAIN_API_URL` | `http://second-brain:9200` | Backend 서비스 URL (클러스터 내부) |
| `NEXT_PUBLIC_APP_URL` | `http://localhost:3000` | 공개 App URL |
| `PORT` | `3000` | Next.js 포트 |
| `HOSTNAME` | `0.0.0.0` | 바인딩 호스트 |

---

## 8. Filesystem Collector 활성화 (선택)

기본 배포는 Drive 수집기를 비활성화한다 (`FILESYSTEM_ENABLED=false`, `emptyDir` 볼륨).
Linux 서버에는 Google Drive Desktop이 없으므로 기본값은 disabled이며 pod는 정상 기동한다.

### 전제 조건

호스트에 Google Drive가 실제로 마운트되어 있어야 한다:

```bash
ls /mnt/drive   # 파일이 보여야 함 — 빈 디렉토리이면 활성화 불필요
```

### 활성화 절차

```bash
# 1. PersistentVolume 적용 (선택, hostPath 직접 사용 시 불필요)
cd ~/second-brain/deploy/k8s
kubectl apply -f second-brain-pv.yaml

# 2. Deployment에 hostPath 볼륨 주입 (emptyDir → hostPath 교체)
kubectl -n second-brain patch deployment/second-brain --type=json -p='[
  {"op":"replace","path":"/spec/template/spec/volumes/0",
   "value":{"name":"drive","hostPath":{"path":"/mnt/drive","type":"Directory"}}}]'

# 3. FILESYSTEM_ENABLED 활성화
kubectl -n second-brain set env deployment/second-brain FILESYSTEM_ENABLED=true

# 4. Rollout 재시작 및 검증
kubectl -n second-brain rollout restart deployment/second-brain
kubectl -n second-brain rollout status deployment/second-brain --timeout=120s
kubectl logs -n second-brain deploy/second-brain --tail=20 | grep filesystem
```

### 비활성화 (원복)

```bash
# 1. 볼륨을 emptyDir로 복원
kubectl -n second-brain patch deployment/second-brain --type=json -p='[
  {"op":"replace","path":"/spec/template/spec/volumes/0",
   "value":{"name":"drive","emptyDir":{}}}]'

# 2. 플래그 비활성화
kubectl -n second-brain set env deployment/second-brain FILESYSTEM_ENABLED=false

# 3. Rollout 재시작
kubectl -n second-brain rollout restart deployment/second-brain
```

> **주의**: `second-brain-pv.yaml`은 kustomization.yaml에서 제외되어 있다.
> `kubectl apply -k`로 적용해도 PV는 생성되지 않는다. 수동 `kubectl apply -f second-brain-pv.yaml` 필요.

---

## 9. 관련 문서

| 문서 | 경로 | 설명 |
|------|------|------|
| Architecture | `ARCHITECTURE.md` §9 | 배포 아키텍처 |
| Architecture (영문) | `ARCHITECTURE.en.md` §9 | Deployment section |
| K8s README | `deploy/k8s/README.md` | 매니페스트 설명 |
| Secret Example | `deploy/k8s/second-brain-secret.example.yaml` | Secret 키 목록 (값 없음) |

### 관련 이슈 요약

| 이슈 | 제목 | 이 Runbook의 대응 |
|------|------|-------------------|
| #6 | Drive hostPath PV 선택형 전환 | §8 Filesystem Collector 활성화, ConfigMap default off |
| #21 | 초기 배포 셋업 | 전체 절차 정립 |
| #28 | Docker 캐시 스테일 이미지 | Step 4 `--no-cache` 필수화 |
| #29 | kustomize Secret 덮어쓰기 | Step 5 개별 적용, DO NOT §1 |
| #34 | cliproxy `/v1/embeddings` 404 | 문제 해결 §Embedding 404 |
| #35 | Dockerfile 미커밋 배포 | DO NOT §5, Step 1 커밋 우선 |
| #36 | cliproxy 인증 만료 LLM 401 | 문제 해결 §LLM 401 |
| #37 | 시행착오 반복 — Runbook 필요 | 이 문서 전체 |

---

*이 Runbook은 실제 사고(#28 #29 #35 #36)를 근거로 작성되었다.*
*배포 절차 변경 시 이 파일을 먼저 업데이트하고 PR에 포함한다.*
