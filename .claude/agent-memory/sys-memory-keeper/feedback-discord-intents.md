---
name: discord-gateway-intents
description: discordgo Gateway 설정 시 IntentsGuilds 필수 포함, state cache 미작동 시 모든 메시지 silent drop
type: feedback
---

discordgo 기반 봇에서 `IntentsGuilds`를 반드시 포함할 것. 누락 시 모든 메시지 silent drop.

**Why:** 2026-04-14 second-brain 봇 배포 시 `IntentsGuildMessages | IntentsGuildMessageReactions`만 설정해서 `GUILD_CREATE` 이벤트 미수신 → `sess.State.Channel()` 항상 error → `handleMessageCreate`가 모든 메시지 silent skip → 사용자가 "봇이 응답 안함"으로 반복 신고. 로그에 아무것도 안 나와 디버깅이 매우 어려웠음. v0.1.14 hotfix로 `IntentsGuilds` 추가 + REST fallback 패턴 적용.

**How to apply:**
1. `sess.Identify.Intents |= discordgo.IntentsGuilds` 필수 추가
2. `s.State.Channel(id)` 는 항상 REST fallback(`s.Channel(id)`)으로 래핑하는 `resolveChannel` 패턴 사용
3. state miss 시 warn 로그 남겨 regression 조기 감지
4. MessageCreate 핸들러 진입부에 "event received" 수준 debug 로그로 수신 여부 확인 용이하게 구성
