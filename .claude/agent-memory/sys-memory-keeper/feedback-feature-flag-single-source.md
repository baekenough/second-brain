---
name: feedback-feature-flag-single-source
description: 비용 발생 기능(LLM 호출 등)의 feature flag는 단일 진입점, default OFF, 모든 코드 경로 커버해야 함
metadata:
  type: feedback
---

비용을 발생시키는 신규 기능(LLM API 호출, 외부 서비스 연동 등)에 feature flag를 도입할 때 반드시 단일 flag, default-safe(OFF), 전체 코드 경로 커버를 보장해야 한다.

**Why:** v0.13.0 #77 entity extraction 초기 구현 — ENTITY_EXTRACTION_ENABLED가 backfill worker만 게이팅하고 inline scheduler path는 무조건 실행(default ON). LLM 비용이 사용자 모르게 발생하는 구조였음. 수정: 단일 flag로 inline + backfill 모두 게이팅, default OFF. search read-path는 항상 ON(비용 없음).

**How to apply:** 신규 LLM/외부 호출 기능 추가 시:
1. env var 하나로 ON/OFF — 두 곳에 나뉘어 있으면 구조 결함
2. default = OFF (opt-in). 사용자가 명시적으로 켜야 비용 발생
3. 코드 경로 grep: flag 변수를 참조하지 않는 실행 경로가 있으면 누락
4. read-path(조회만)와 write-path(LLM 호출)를 flag 적용 범위에서 분리 고려
