---
name: feedback-slack-dm-privacy
description: Slack DM/개인 대화는 second-brain에 수집하지 않음 — 사용자 명시 원칙
type: feedback
---

개인 Slack 대화(DM, MPIM)는 second-brain에 수집하지 않는다.

**Why:** 사용자가 "개인적인 대화내용들은 수집하고싶지 않아"라고 명시함 (2026-04-14). 업무 채널 정보와 달리 개인 DM은 프라이버시 민감 데이터임.

**How to apply:**
- Slack collector/봇/앱 관련 변경 요청 시 DM 수집 경로 여부를 먼저 확인
- `internal/collector/slack.go:184`의 `types=public_channel,private_channel` 필터는 건드리지 않음 — 이게 DM 차단의 핵심
- second-brain 앱에 `im:history`/`mpim:history` OAuth scope 추가 제안 금지
- 클로드-써치 응답 봇 등 별도 봇 앱 설계 시 collector 앱과 scope 분리를 기본값으로 제안
- 신규 코드에서 `channel_type == "im"` 또는 `IsIM || IsMpIM` 거부 가드를 추가하는 방향 권장
