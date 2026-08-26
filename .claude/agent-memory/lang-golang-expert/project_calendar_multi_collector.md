---
name: project-calendar-multi-collector
description: 캘린더 수집기 복수 캘린더 지원(2026-08-26) — source_id 네임스페이스 규칙과 collector_state 워터마크 트레이드오프
metadata:
  type: project
---

`internal/collector/calendar.go` / `internal/config/config.go`에 복수 캘린더 수집 추가 (commit 63f7349, PR #224 브랜치 `feature/ubuntu1-stack-migration`).

**Why:** `CALENDAR_ID=primary` 단일값만 수집하던 구조라 회사 캘린더(`baeksy@agilesoda.ai`, accessRole=freeBusyReader)가 수집되지 않았음. 실측 결과 freeBusyReader는 events API에서 403이 아니라 **HTTP 200 + summary/description 빈 값**으로 응답 — 권한 문제를 에러가 아닌 "빈 콘텐츠"로 감지해야 함.

**How to apply:**
- `CALENDAR_ID`는 콤마 구분 복수값 지원(하위호환: 단일값 그대로 동작). `parseCalendarIDs()`가 trim/중복제거/기본값 처리.
- **summary trim 후 빈 문자열 이벤트는 skip** — SMS 수집기의 "source file is empty" 경고와 같은 톤. 버그 아님, 권한 신호. 다른 소스에 비슷한 "권한은 있는데 콘텐츠가 안 오는" 패턴이 또 나오면 이 접근(에러 대신 콘텐츠 공백 감지)을 재사용할 것.
- **source_id 규칙**: 첫 번째(레거시) 캘린더만 `calendar:<eventID>` 유지, 이후 캘린더는 `calendar:<calID>:<eventID>`로 네임스페이스. [[project_second_brain]]의 SMS source_id 재키잉 실패 이력(#144, 두 번 NO-GO → 결국 별도 마이그레이션 019)을 참고해, **처음부터 기존 키를 건드리지 않는 설계**를 택함. 복수 소스 지원을 추가할 때 기존 source_id 스킴을 유지하면서 확장하는 패턴으로 재사용 가능.
- **collector_state 워터마크는 (instance_id, source_type) 단위**이지 하위 항목(캘린더/계정) 단위가 아님. 마이그레이션 없이 복수 하위소스를 다룰 때는: 하나라도 성공하면 nil 반환(부분 성공 허용, 워터마크 전진) vs 전부 실패해야 error 반환(재시도) — 이번엔 후자를 택함(전부 실패 시에만 에러). 트레이드오프: 특정 하위소스가 간헐적으로 실패/복구를 반복하면 그 하위소스의 다운타임 구간 업데이트가 누락될 수 있음(bounded gap) — 이는 "다른 정상 소스까지 막는 것보다 낫다"는 판단으로 수용. 같은 패턴(단일 워터마크로 복수 하위소스 관리)이 다른 수집기에도 나타나면 이 트레이드오프 문서(calendar.go Collect() 주석)를 참조할 것.

go test ./... (필터 없음) 1268 PASS 0 FAIL 확인 후 커밋+push 완료. 배포는 미실행(사용자가 직접 진행 예정) — 회사 캘린더 공유 권한이 "모든 일정 세부정보 보기"로 올라가면 코드 변경 없이 자동 수집 시작.
