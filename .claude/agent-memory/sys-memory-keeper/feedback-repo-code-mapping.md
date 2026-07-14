---
name: feedback-repo-code-mapping
description: 버그리포트/이슈 생성 전 해당 코드가 실제로 그 레포에 있는지 파일 단위로 매핑할 것
metadata:
  type: feedback
---

이슈를 만들기 전에 referenced 파일/심볼이 해당 레포에 실제로 존재하는지 Explore/Grep으로 먼저 확인한다.

**Why:** 2026-06-01 세션에서 Windows 환경의 nested MCP 버그리포트 10건을 second-brain 이슈로 분해했으나, memory_integrated.py(load_dotenv override, REFLECT_MODEL 하드코딩, cp949 이모지 등) 관련 5건은 ontology-rag/llm-memory Python MCP 서버 소속이었다. second-brain은 순수 Go 프로젝트(Python 파일 0개). 잘못된 레포에 이슈 5건이 생성되어 not-planned 처리 비용이 발생했다.

**How to apply:** 버그리포트의 파일명·함수명·패키지명을 Explore/Grep으로 레포에서 검색 → 존재하지 않으면 다른 레포 소속으로 분류. second-brain은 Go만 있으므로 `.py` 파일 언급 이슈는 즉시 외부 레포로 이관.
