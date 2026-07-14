---
name: feedback-workflow-schema-design
description: Workflow 에이전트에 StructuredOutput schema 요구 시 누락 실패 패턴 — 출력이 긴 단계는 text 반환으로 설계
metadata:
  type: feedback
---

lang-/be- 계열 전문가 에이전트를 Workflow agent로 사용할 때 StructuredOutput(schema)을 요구하면 자기 워크플로에 빠져 마지막 StructuredOutput 호출을 누락해 전체 워크플로가 실패할 수 있다.

**Why:** 2026-06-01 세션 auto-dev 파이프라인에서 verify/review 단계에 schema 요구 시 여러 차례 누락 실패 관찰. 에이전트가 자체 워크플로(코드 분석, 파일 읽기 등)를 수행하다 마지막 structured return을 잊는 패턴.

**How to apply:**
- verify/review처럼 출력이 긴 단계는 schema 없이 text 반환으로 설계
- 구현 단계는 파일이 디스크에 남으므로 schema 없어도 재실행 가능
- schema가 꼭 필요한 경우 프롬프트 마지막에 "MUST call StructuredOutput as final step" 강조 필수
