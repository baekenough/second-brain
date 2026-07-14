---
name: feedback-pii-sink-sweep
description: PII sink-sweep 누락 — 이슈가 지목한 필드만 수정하고 파일명/로그/에러/문서필드 등 다른 sink에 원본 잔존
metadata:
  type: feedback
---

Pattern: "PII sink-sweep 누락" — 2026-07-13 FSD 세션에서 동일 클래스 2회 발생.

1) v0.21.2 #164: 이슈가 지목한 sidecar 필드만 해시화, 같은 요청 경로의 파일명·문서 Title/Content 폴백·destPath 로그는 평문 잔존 (deep-verify CRITICAL).
2) v0.22.0 #166: PII 제거 도구 자신이 dry-run 포함 전 모드에서 원본 번호를 stdout/stderr 로그로 출력 (CWE-532, deep-verify BROKEN).

**Why:** 구현 에이전트는 이슈 본문/프롬프트가 지목한 위치만 수정하는 literalism 경향. PII의 sink는 저장 필드만이 아니라 파일명, 로그, 에러 메시지(예: os.PathError는 경로를 Error()에 내장), 문서 Title/메타데이터, 경로 문자열 전부.

**How to apply:** PII-adjacent 구현/도구 작성 프롬프트에 반드시 sink-sweep 체크리스트 포함 — "해당 데이터가 등장할 수 있는 모든 sink(파일명·로그·에러 메시지·문서 필드·메타데이터·경로 문자열)를 스윕하고 각 sink에 대한 부정 테스트(원본 값 부재 단언) 작성". deep-verify 적대 리뷰를 PII 작업에는 생략하지 말 것 (2회 연속 릴리스 전 차단 실적).

[[feedback-deep-verify-high-surface]]
