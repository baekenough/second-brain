---
name: session-2026-04-14-slack-collection
description: 2026-04-14 세션 — #biz-sales-account 수집 확인, DM 비수집 설계, Search API 예시
type: project
---

## 세션 요약

**날짜**: 2026-04-14
**주요 작업**: Slack 채널 수집 상태 모니터링, DM 프라이버시 설계 논의, Search API 사용 예시 제공

## 완료된 작업

1. **Search API 사용 예시 제공** — `/api/v1/search` (POST, Bearer auth) curl/Python/jq 예시. 주요 파라미터: `query`, `source_type`, `exclude_source_types`, `limit`, `sort`(relevance/recent), `include_deleted`.

2. **#biz-sales-account 수집 완료 확인** — 봇 초대 후 60초 watcher 감지 → 약 4분 24초 소요 → 5365건 전원 수집. conversations.replies rate limit 8건 발생했으나 backoff 재시도로 자동 복구.

3. **DM 비수집 설계 확인** — `internal/collector/slack.go:184`의 `types=public_channel,private_channel` 하드코딩이 DM/mpim을 원천 배제. 의도적 프라이버시 보호 설계임을 확인.

4. **클로드-써치 봇 DM 대응 설계 가이드** — vibers-brain 앱과 봇 앱 분리, scope 레벨 물리적 차단, `channel_type == "im"` 이중 필터, collector 코드에 `IsIM || IsMpIM` 거부 가드 추가 방안 제시.

5. **Monitor 툴 실시간 로그 추적** — `kubectl -n vibers-brain logs` 스트림을 Monitor 툴로 추적하여 수집 완료 시점 확인.

## 핵심 결정/발견

- 사용자는 개인 DM 수집을 원하지 않음 (명시적 확인) → feedback-slack-dm-privacy.md 저장
- 운영 서버는 k8s 네임스페이스 `vibers-brain`에서 실행 중. 사용자가 "로컬"이라 부르는 것은 `kubectl port-forward` 때문.
- #biz-sales-account는 스레드가 매우 많아 rate limit이 빈번히 발생하는 채널 — 향후 재수집 시 여유 타임아웃 필요.

## 오픈 아이템

- BUG-006 (Slack rate limit backoff): 현재 backoff는 되고 있으나 코드 공식 지원 여부 재확인 필요
- 클로드-써치 봇 실제 구현은 이번 세션에서 진행되지 않음 (설계 논의만)
