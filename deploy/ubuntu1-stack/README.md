# second-brain — ubuntu1 스택 (macmini → ubuntu1 이전, 2026-08-25)

2026-08-25에 second-brain 애플리케이션 스택을 macmini(arm64)에서 ubuntu1(x86_64)로
이전했다. 이 디렉토리는 ubuntu1에 이미 배치되어 운영 중인 산출물을 버전 관리
대상으로 반영한 것이다 — 여기 있는 파일들을 수정한다고 ubuntu1이 바로 갱신되지
않는다(별도 배포 절차 필요, 아래 "기동 방법" 참고).

## 구성

- ubuntu1에서 도는 것: **server / collector / web / neo4j / mcp / eval-runner**
  — compose 프로젝트 이름 `second-brain-ubuntu1`, 작업 디렉토리
  `~/second-brain-app`. (이 README는 한동안 mcp/eval-runner를 "미이전"으로
  적고 있었는데, 실제로는 이미 이전되어 떠 있다 — 2026-08-27 재배포 작업
  중 `docker ps`로 확인, 아래 TODO 섹션도 정정.)
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
| `verify-mounts.sh` | `~/bin/verify-mounts.sh` |
| `systemd/sb-data-sync.service` | `~/.config/systemd/user/sb-data-sync.service` |
| `systemd/sb-data-sync.timer` | `~/.config/systemd/user/sb-data-sync.timer` |
| `systemd/sb-permcheck.service` | `~/.config/systemd/user/sb-permcheck.service` |
| `systemd/sb-permcheck.timer` | `~/.config/systemd/user/sb-permcheck.timer` |
| `systemd/cloudflared-second-brain.service` | `/etc/systemd/system/cloudflared-second-brain.service` |
| `cloudflared/config-second-brain.yml` | `~/.cloudflared/config-second-brain.yml` |

`verify-mounts.sh` + `sb-permcheck.timer`는 2026-08-27에 추가됐다 — bind mount
쓰기 가능 여부를 컨테이너 안에서 직접 실측(touch/rm)해 10분 주기로 검사하고
실패 시 `journalctl --user -u sb-permcheck` / `systemctl --user status
sb-permcheck.service`에 남긴다. 자세한 배경은 아래 "함정 1" 참고.
2026-08-29에 compose `env_file:` 목록의 존재·비어있지 않음 점검이
추가됐다(내용은 읽지 않는다). 자세한 배경은 아래 "함정 5" 참고.

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

배포 직후에는 bind mount 쓰기 테스트를 실행해 실제로 쓸 수 있는지 확인한다
(권한 숫자만으로는 부족하다 — 아래 "함정 1" 참고):

```bash
~/bin/verify-mounts.sh; echo "exit=$?"
```

`sb-permcheck.timer`가 10분마다 같은 검사를 자동 반복하므로, 배포를
깜빡 잊고 이 단계를 건너뛰어도 늦어도 10분 안에 `journalctl --user -u
sb-permcheck`에 FAIL로 드러난다.

## 이전 과정에서 실제로 부딪힌 함정 5가지

**재발 방지를 위해 반드시 기록한다.**

### 1. 파일 권한 (macOS → Linux) — 2026-08-27 근본 해결

컨테이너는 원래 `uid=10001(appuser)`로 실행되는데, rsync로 옮긴 자격증명
파일과 bind mount 소스는 전부 `uid=1000` 소유라 group 비트(setgid+rwx)로
우회해야 했다. Docker Desktop for Mac은 VirtioFS가 uid를 매핑해줘서 이
충돌이 드러나지 않지만, Linux bind mount는 호스트 uid를 그대로 노출한다.

**사고 (2026-08-26)**: 서버 bind mount 권한 장애로 통화 녹음 전송이
16시간 지연됐다(재시도만 반복, 발현이 느려 늦게 발견). **근본 원인은
`sb-data-sync.timer`(10분 주기 rsync pull)였다** — rsync -a가 기존
디렉토리라도 매 실행마다 mode를 원본(macmini, group 비트 없는 `755`)으로
되돌린다. 수동으로 `chgrp 10001` + `chmod g+rwxs`를 맞춰도 다음 sync
주기(최대 10분 뒤)에 조용히 원복된다 — **재배포 때문이 아니라 정상 운영
중에도 상시 재현되는 구조였다.** 2026-08-27 실측으로 확인
(`systemctl --user start sb-data-sync.service` 전후 `ls -ldn` 비교 →
매번 `drwxrwsr-x` → `drwxr-xr-x`로 되돌아감).

**근본 해결**: `docker-compose.ubuntu1.yml`의 `server`/`collector` 서비스에
`user: "1000:1000"`을 추가해 컨테이너 uid를 호스트 `baekenough`(uid=gid=1000,
bind mount 소유자)와 맞췄다. owner rwx 비트만으로 접근이 성립하므로 group
비트가 sync에 의해 매번 초기화돼도 더 이상 기능에 영향을 주지 않는다
(2026-08-27 `sb-data-sync.service`를 강제 실행해 drift를 재현한 뒤에도
쓰기 테스트가 통과함을 확인). `web`/`neo4j`/`mcp`/`eval-runner`는 이
bind mount를 쓰지 않아 변경하지 않았다.

부작용 확인: 이미지 내부 `/app`(server 바이너리, migrations)은
`root:root` 소유지만 world-readable이라 uid 1000 실행에 영향 없음.
`/data/drive`(GDrive 컬렉터 전용, `appuser:appgroup`=10001 소유)는
`FILESYSTEM_ENABLED`가 꺼져 있어 미사용 확인 후 변경.

옛 group 우회(자격증명 4개 파일 `chgrp 10001` + `chmod 640`,
`sync-second-brain-data.sh` 안의 `chmod -R g+rX stage/`)는 방어적으로
그대로 남겨뒀다 — uid 정렬로 무해해졌지만 group 기반 접근이 필요한 다른
프로세스가 생기면 여전히 의미가 있다.

**검증 자동화**: 권한 숫자가 맞아 보여도 uid가 다르면 못 쓴다(이번 사고의
핵심 교훈). `verify-mounts.sh`가 컨테이너 안에서 실제 touch/rm을 실행해
`sb-permcheck.timer`로 10분마다 검사한다. 배포 직후에도 수동 실행 권장:

```bash
~/bin/verify-mounts.sh; echo "exit=$?"
```

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

### 5. gitignore 대상 env 파일이 소스 동기화로 유실 — 2026-08-29

**사고**: 2026-08-27 재배포 때 ubuntu1의 `~/second-brain-app/web/` 트리를
git 추적 파일만으로 통째 교체했는데, 그 과정에서 gitignore 대상인
`web/.env.local`이 함께 사라졌다(증거: `web/` 안 모든 파일의 mtime이
`2026-08-27 03:26`로 동일하고 내용물이 추적 파일뿐이었다 —
`node_modules`·`.next`도 없음).

**왜 즉시 안 드러났는가**: 당시 web 컨테이너는 이미 기동 중이었고, 실행
중인 프로세스는 시작 시점에 물었던 환경변수를 계속 들고 있다 —
`docker compose up -d`가 컨테이너를 재생성하지 않는 한 파일 유실은 아무
증상도 내지 않는다. 이틀 뒤인 2026-08-29, 다음 배포에서 `docker compose
... up -d`가 프로젝트 전체에 대해 `env file
/home/baekenough/second-brain-app/web/.env.local not found`로 실패하고
나서야 발견됐다. **재배포 시점까지 잠복하는 결함이라 사고 당시 로그만
봐서는 절대 못 잡는다.**

**ubuntu1에 존재해야 하지만 git에는 없는 파일**:
- `~/second-brain-app/.env.local`
- `~/second-brain-app/web/.env.local`

둘 다 `.gitignore` 대상(시크릿)이라 이 리포에 없는 것이 정상이다 — 문제는
"리포에 없다"가 아니라 "소스 동기화가 이 파일들까지 지우거나 덮을 수
있다"는 점이다.

**지침**: 소스 동기화(재배포)는 git 추적 대상만 명시적으로 한정할 것 —
예를 들어 `cmd/ internal/ migrations/ go.mod go.sum Dockerfile`처럼
동기화할 경로를 나열하고, `web/` 트리 전체를 통째로 교체하지 말 것.
rsync를 쓴다면 `--delete` 사용에 특히 주의(대상 디렉토리에만 국한, 상위
디렉토리 전체에 걸지 말 것). git 기반으로 교체하는 경우에도 "추적
파일만 남기고 나머지 삭제" 방식은 gitignore 파일을 함께 날린다는 점을
명심할 것.

**검증 자동화**: `verify-mounts.sh`가 이제 compose 파일의 `env_file:`
목록을 추출해 각 파일의 존재·비어있지 않음을 10분마다 점검한다(내용은
읽지 않는다 — 시크릿이다). `sb-permcheck.timer`로 함정 1의 bind mount
점검과 같은 주기로 함께 돈다.

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

- ~~mcp 서비스 미이전~~ → 이전 완료, `second-brain-ubuntu1-mcp-1` 컨테이너
  running (2026-08-27 확인). `brain-mcp` ingress가 여전히 502면 컨테이너가
  아니라 cloudflared ingress 설정/터널 쪽 문제일 가능성이 높다 — 이번
  작업 범위 밖이라 별도 확인 필요.
- ~~eval-runner 미이전~~ → 이전 완료, `second-brain-ubuntu1-eval-runner-1`
  컨테이너 running (2026-08-27 확인).
- macmini의 OneDrive 의존을 걷어내면 macmini를 완전히 뺄 수 있다 (rclone
  등으로 ubuntu1이 직접 OneDrive를 받는 방안)

## 관련 문서

- [`deploy/postgres-ubuntu1/`](../postgres-ubuntu1/) — 이 스택이 참조하는
  postgres 컴포즈 프로젝트 (2026-08-19 이전 완료)
