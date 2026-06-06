---
name: feedback-deep-verify-high-surface
description: deep-verify HIGH 판정은 새 공격 표면(외부 요청) 도입 시 실제 취약점일 가능성이 높음
metadata:
  type: feedback
---

새 기능이 **외부 outbound 요청** 또는 **사용자 제어 입력을 처리하는 경로**를 추가할 때, deep-verify의 HIGH 판정은 FP가 아닌 실제 취약점일 가능성이 높다.

**Why:** v0.11.0 #72 refetch 워커: DB-저장 URL을 외부에 직접 fetch → SSRF + Slack token redirect-leak. deep-verify가 HIGH로 판정했고 실제 취약점이었음. 반면 v0.3.0(기존 코드 패스) HIGH 4건은 전부 FP였음.

**How to apply:**
- 기존 코드 패스 HIGH → 증거 기반 cross-verify 후 FP 가능성 고려 (기존 feedback-deep-verify-high-fp 참조)
- **신규 기능(outbound HTTP / 사용자 URL 처리) HIGH → 우선 실제 취약점으로 간주하고 수정 착수**
- 판별 기준: "이 기능이 새로운 attack surface를 도입하는가?" YES → HIGH를 실제로 취급

**수정 패턴 (refetch 워커 사례):**
1. 소스별 host allowlist (Slack/Discord 도메인 화이트리스트)
2. `CheckRedirect`: host 재검증 + auth header strip + hop 수 제한(3회)
3. `ResponseHeaderTimeout` 추가

[[feedback-deep-verify-high-fp]]
