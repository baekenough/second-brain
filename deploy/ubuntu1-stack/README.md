# second-brain — ubuntu1 스택 (macmini → ubuntu1 이전, 2026-08-25)

2026-08-25에 second-brain 애플리케이션 스택을 macmini(arm64)에서 ubuntu1(x86_64)로
이전했다. 이 디렉토리는 ubuntu1에 이미 배치되어 운영 중인 산출물을 버전 관리
대상으로 반영한 것이다 — 여기 있는 파일들을 수정한다고 ubuntu1이 바로 갱신되지
않는다(별도 배포 절차 필요, 아래 "기동 방법" 참고).

## 구성

- ubuntu1에서 도는 것: **server / collector / web / neo4j** — compose 프로젝트
  이름 `second-brain-ubuntu1`, 작업 디렉토리 `~/second-brain-app`.
- **postgres는 이 스택에 없다.** 별도 compose 프로젝트(`~/second-brain-postgres`,
  자세한 내용은 [`deploy/postgres-ubuntu1/`](../postgres-ubuntu1/))로 이미 운영
  중이며, `.env.local`의 `DATABASE_URL`이 Tailscale IP로 그 컨테이너를 직접
  가리킨다.
- macmini에 남은 역할: **OneDrive 클라이언트 + `com.sangyi.secondbrain.onedrive-bridge`
  LaunchAgent** — 파일 게이트웨이 전용. second-brain 컨테이너 5개는 전부
  `docker stop` 상태이며, 볼륨(`second-brain-local_pgdata` = postgres 이전
  롤백본, `second-brain-local_neo4jdata`)은 보존되어 있다.

## 파일 목록

| 파일 | ubuntu1 원본 경로 |
|------|-------------------|
| `docker-compose.yml` | `~/second-brain-app/docker-compose.ubuntu1.yml` |
| `sync-second-brain-data.sh` | `~/bin/sync-second-brain-data.sh` |
| `systemd/sb-data-sync.service` | `~/.config/systemd/user/sb-data-sync.service` |
| `systemd/sb-data-sync.timer` | `~/.config/systemd/user/sb-data-sync.timer` |
| `systemd/cloudflared-second-brain.service` | `/etc/systemd/system/cloudflared-second-brain.service` |
| `cloudflared/config-second-brain.yml` | `~/.cloudflared/config-second-brain.yml` |

`cloudflared/config-second-brain.yml`은 터널 ID와 credentials **경로**만
담고 있다. credentials json(`ee3b9a8b-....json`) 자체는 시크릿이라 이 리포에
가져오지 않았다.

## 기동 방법

```bash
docker compose --env-file .env.local -f docker-compose.ubuntu1.yml up -d
```

`--env-file`이 필수인 이유: `NEO4J_PASSWORD`는 `${VAR}` 치환이라 `env_file:`
(컨테이너 내부 주입)이 아니라 `--env-file`(compose 자체의 변수 치환)로만
온다. 이 플래그를 빠뜨리면 변수가 빈 값으로 치환되어 조용히 잘못된 상태로
기동하거나 즉시 실패한다. 과거 맥미니에서 동일한 실수로 볼륨이 잘못된 기본
경로에 생성돼 collector가 완전히 멈춘 사고가 있었다
([`feedback_macmini_compose_envfile_flag`]).

## 이전 과정에서 실제로 부딪힌 함정 4가지

**재발 방지를 위해 반드시 기록한다.**

### 1. 파일 권한 (macOS → Linux)

컨테이너는 `uid=10001(appuser)`로 실행되는데, rsync로 옮긴 자격증명 파일은
`uid=1000` 소유의 `600`이라 읽지 못했다. Docker Desktop for Mac은 VirtioFS가
uid를 매핑해줘서 이 충돌이 드러나지 않지만, Linux bind mount는 호스트 uid를
그대로 노출한다.

해결: 자격증명 4개 파일(`gmail/credentials.json`, `gmail/token.json`,
`calendar/credentials.json`, `calendar/token.json`)에 `chgrp 10001` +
`chmod 640`. 일반 사용자는 자신이 속하지 않은 그룹으로 chgrp할 수 없어
`sudo`가 필요하다. **rsync가 매번 권한을 원복시키므로 동기화 스크립트
(`sync-second-brain-data.sh`)가 이 단계를 매 실행마다 반복 수행한다.**

### 2. openrsync 비호환

macmini의 rsync는 openrsync(protocol 29, rsync 2.6.9 호환)라 `--info=stats2`
같은 rsync 3.x 전용 옵션을 주면 원격이 usage를 출력하고 죽는다. 증상은
"connection unexpectedly closed (0 bytes received)"이고 **전송은 한 바이트도
일어나지 않는다.** `-az`만 사용할 것.

### 3. 기동 순서

server/collector가 neo4j보다 먼저 뜨면 `connection refused`로 그래프 투영과
그래프 읽기 API가 **비활성 상태로 고정된다(자동 재연결 없음)**. neo4j가
healthy가 된 뒤 `docker compose restart server collector`로 복구한다.
`depends_on`을 걸지 않는 것은 의도된 설계다(neo4j가 죽어도 검색/ask는 살아야
한다) — 대신 기동 후 그래프 관련 로그를 확인하는 절차가 필요하다.

```bash
docker compose --env-file .env.local -f docker-compose.ubuntu1.yml up -d
# neo4j healthy 확인 후:
docker compose --env-file .env.local -f docker-compose.ubuntu1.yml restart server collector
```

### 4. stage/sms는 빈 껍데기

macmini의 `stage/sms/*.xml`은 `stat` 크기 0인 파일이다(`du`는 285MB로
보고하지만 실제 내용은 없다). SMS는 현재 휴대폰 앱이 서버로 직접 push하는
경로로 수집되며 이 파일들은 레거시다. collector 로그의 "sms: source file is
empty — possible OneDrive materialization/bridge failure" 경고는 정상
동작이다.

## 데이터 동기화

ubuntu1의 systemd user 타이머 `sb-data-sync.timer`가 10분 주기로 macmini에서
`stage/call`, `stage/sms`, `secretary/gmail`, `secretary/calendar`를 pull
한다.

- ubuntu1 → macmini SSH는 `llm-memory-hub` 별칭(Tailscale 100.100.80.74)을
  재사용한다. 원래 llm-memory 허브용으로 만들어진 경로다.
- `loginctl show-user baekenough`의 `Linger=yes`라서 로그아웃 상태에서도
  타이머가 돈다.

## cloudflared 터널

- 터널 ID `ee3b9a8b-8f78-4085-897a-dcf79d70b9c4`(이름 `second-brain`)를
  macmini에서 그대로 승계했으므로 DNS 레코드 변경이 필요 없었다.
- systemd 시스템 서비스 `cloudflared-second-brain.service`로 등록되어 있다.
- ingress:
  - `brain.baekenough.com` → 8081 (server)
  - `sb.baekenough.com` → 3000 (web, Cloudflare Access 뒤)
  - `brain-mcp.baekenough.com` → 8090 (mcp, **현재 미기동** — macmini에서도
    이미 리스너가 없던 죽은 ingress라 항목만 남겨두었다)
- 모든 컨테이너 포트는 `127.0.0.1`에만 바인딩된다. 터널이 같은 호스트에서
  접근하므로 충분하고, 개인 데이터가 LAN에 노출되지 않아야 한다는 이
  프로젝트의 원칙과 일치한다(원본 macmini 설정은 server가 `0.0.0.0:8081`
  이었는데 이전하면서 좁혔다).

## 아직 남은 것 (TODO)

- mcp 서비스 미이전 (`brain-mcp` ingress가 502를 반환한다)
- eval-runner 미이전
- macmini의 OneDrive 의존을 걷어내면 macmini를 완전히 뺄 수 있다 (rclone
  등으로 ubuntu1이 직접 OneDrive를 받는 방안)

## 관련 문서

- [`deploy/postgres-ubuntu1/`](../postgres-ubuntu1/) — 이 스택이 참조하는
  postgres 컴포즈 프로젝트 (2026-08-19 이전 완료)
