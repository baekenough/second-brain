---
name: session-2026-04-13-org-migration
description: 조직 마이그레이션 세션 — 프로젝트 경로/GitHub remote 변경, API 인증, Slack 수집, 웹 UI 개선
type: project
---

## 세션 개요

- **날짜**: 2026-04-13
- **주요 작업**: 조직 이관(baekenough → Vibers-ai), API 토큰 인증, Slack 수집 개선, 웹 UI 필터/URL 유지, 운영 현황 점검

## 주요 변경

### 인프라
- **프로젝트 경로 이동**: `~/workspace/baekenough/vibers-brain` → `~/workspace/vibers-brain`
- **GitHub remote 변경**: `git@github.com:sang2-tpm/vibers-brain.git` → `git@github.com:Vibers-ai/vibers-brain.git`
- **gh CLI 활성 계정**: `sang2-tpm` (baekenough → sang2-tpm 전환)
- **minikube VM 메모리**: 7 GB → 8 GB (`docker update --memory 8g` in-place 적용)
- **cloudflared Quick Tunnel**: `https://influenced-computational-smaller-sharp.trycloudflare.com` → `localhost:9200` (재기동 시 URL 변경됨)

### 코드 변경 (커밋 4개)

**feat(backend)**:
- API 토큰 미들웨어: Bearer 헤더 + ConstantTimeCompare, `/health` 만 공개
- `exclude_source_types` 쿼리 파라미터 필터 추가
- `GET /api/v1/stats` 엔드포인트 신규
- Slack `oldest` 음수 버그 수정
- conversations.replies thread 중복 제거 (dedup)
- hybridSearch sort 분기 (하이브리드/벡터 결과 정렬 구분)
- Upsert COALESCE embedding 보존 (재수집 시 기존 임베딩 유지)

**feat(web)**:
- filter 반응성 개선
- count badges 표시
- 24h 타임스탬프 포맷
- URL persist (Suspense + searchParams)
- api-docs 페이지 추가
- 마크다운/xlsx/text 분기 렌더
- 통합 prose 스타일

**chore(deploy)**:
- API_KEY/EMBEDDING_API_KEY/SLACK_BOT_TOKEN 매니페스트에서 제거 → out-of-band patch 관리
- web Deployment에 API_KEY env 주입

**docs**:
- `.env.example` API_KEY 추가
- `plan/resource-verification.md`, `plan/deployment-plan.md`, `plan/sizing-reference.md` 작성

## 운영 상태 스냅샷 (세션 종료 시점)

| 항목 | 상태 |
|------|------|
| Postgres docs | 5,965건 (filesystem 4154 + slack 1811 + github 0) |
| Slack 봇 | xoxb-8730276019027-... (사용자 제공), 1개 채널 초대됨 |
| Slack auto-join | 비활성 (코드 helper만 유지, 실제 호출은 주석 처리) |
| API_KEY | .env 저장 (gitignored), Pod Secret 동기화 완료 |
| 임베딩 | ChatGPT Codex OAuth JWT (8일 만료) — 임시 방편 |

## P0 이슈 (다음 세션 시작점)

1. **OpenAI JWT 8일 만료**: 프로덕션 진입 전 영구 `sk-proj-...` API key 교체 필수
2. **8KB 임베딩 절단**: 긴 문서 꼬리 손실 (BUG-003) — chunks 테이블 마이그레이션 선행 필요
3. **Slack rate limit backoff 부재**: 대량 수집 시 429 에러 위험
4. **minikube hostPath 호스트 종속**: Linux 서버 이주 시 rclone mount 전환 필요
5. **9p mount 한글 긴 파일명 실패**: 255 byte 초과 파일명 lstat 실패

## 추가 작업 (같은 날 이후 세션)

### Slack 채널 watcher + 단일 채널 강제 수집

1. **SlackChannelWatcher** (`internal/collector/slack_watcher.go` 신규)
   - 60초 polling, 신규 채널 감지 시 `CollectChannel(since=zero)` 호출
   - 첫 tick: seen 등록만(재시작 폭주 방지)
   - `ListMemberChannels`: `users.conversations` API로 봇 멤버 채널만 조회

2. **단일 채널 강제 수집 API** (`POST /api/v1/collect/slack/channel`)
   - 파일: `internal/api/collect_channel.go` (신규), `internal/api/router.go` 수정
   - body: `{channel_id}` 또는 `{channel_name}` 중 하나
   - 봇 kick 권한 없거나 seen 캐시 우회가 필요한 채널에 사용
   - `#ax-in-bbq` 채널 결과: 4건 → 25건 (신규 21건 수집)

3. **Slack OAuth scope 이슈 해결**
   - `users.conversations`는 `channels:read` + `groups:read` 둘 다 필요
   - 기존 봇 scope 부족 → `groups:read` 추가 + Reinstall to Workspace로 해결

4. **수집 주기**: 코드 default `1h` → `10m` 변경했으나, ConfigMap `COLLECT_INTERVAL: 5m`이 우선 적용됨 (사용자가 5분 유지 선택)

5. **배포**: rollout restart 2회 적용 완료

## 다음 세션 즉시 작업 후보

- OpenAI API key 교체 (가장 시급)
- chunks 테이블 마이그레이션 + 청크 단위 임베딩 구현
- Slack rate limit 지수 backoff 추가
- GitHub 수집 활성화 (현재 0건)
