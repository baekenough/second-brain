# [MUST] Agent Identification Rules

> **Priority**: MUST | **ID**: R007

## Core Rule

Every response MUST start with agent identification:

```
┌─ Agent: {agent-name} ({agent-type})
├─ Skill: {skill-name} (if applicable)
└─ Task: {brief-task-description}
```

Default (no specific agent): `┌─ Agent: claude (default)`

## Simplified Format

For brief responses: `[mgr-creator] Creating agent structure...`
With skill: `[fe-vercel-agent → react-best-practices] Analyzing...`

## Routing & Skill Context

When the orchestrator uses a routing skill, identification should reflect the active context:

```
┌─ Agent: claude (secretary-routing)
├─ Skill: secretary-routing
└─ Task: route agent management request
```

| Context | Identification |
|---------|---------------|
| No routing active | `claude (default)` |
| secretary-routing | `claude (secretary-routing)` |
| dev-lead-routing | `claude (dev-lead-routing)` |
| de-lead-routing | `claude (de-lead-routing)` |
| qa-lead-routing | `claude (qa-lead-routing)` |
| Skill invocation | `claude → {skill-name}` |

## Skill Invocation Format

When the orchestrator invokes a skill via the Skill tool, the skill name MUST be integrated into the identification block — NOT displayed as a separate tool call.

```
┌─ Agent: claude → {skill-name}
└─ Task: {brief-task-description}
```

### Common Violations

```
Incorrect: Skill as separate display
   ┌─ Agent: claude (default)
   └─ Task: research topic analysis

   Skill(research)    ← separate, disconnected

Correct: Skill integrated into identification
   ┌─ Agent: claude → research
   └─ Task: research topic analysis

Correct: With sub-skill
   ┌─ Agent: claude → research
   ├─ Skill: result-aggregation
   └─ Task: aggregate team findings
```

## When to Display

| Situation | Display |
|-----------|---------|
| Agent-specific task | Full header |
| Using skill | Include skill name |
| General conversation | "claude (default)" |
| Long tasks | Show progress with agent context |
| Skill invocation | Integrated `claude → {skill-name}` format |

## 컨텍스트 드리프트 완화 (Multi-Turn 세션)

세션이 길어지면 프롬프트 기반 규칙(R007)이 컨텍스트에서 밀려 누락될 수 있다. 다음 구간에서 실제 누락이 발생했다:

| 고위험 구간 | 이유 |
|------------|------|
| 서브에이전트 완료 알림(task-notification)으로 시작하는 턴 | 사용자 입력이 아니라서 규칙 상기 계기가 약함 |
| 도구 호출 5회 이상 이어지는 연쇄 진단 작업 | 초반 컨텍스트에만 규칙이 있고 재확인이 없음 |
| 병렬 에이전트 3개 이상 결과 수신·정리 구간 | 결과 취합에 집중하다 헤더 작성을 건너뛰기 쉬움 |

**자가 점검**: 위 구간에서는 응답을 시작하기 전에 헤더 유무를 명시적으로 확인한다. 알림으로 턴이 시작되면 규칙이 리셋된 것처럼 느껴지지만 그렇지 않다 — 알림 턴도 사용자 입력 턴과 동일하게 R007이 적용된다.

**재발 이력**: 2026-08-18 세션에서 동일 세션 내 2회 누락(모두 사용자가 `@CLAUDE.md 재주입`으로 지적). 1차 발생 시 "나중에 규칙을 강화하겠다"고 미룬 것이 2차 재발의 원인이었다 — R016 안티패턴("나중에 고치겠다")의 실사례.
