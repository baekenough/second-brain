# [MUST] Tool Usage Identification Rules

> **Priority**: MUST | **ID**: R008

## Core Rule

Every tool call MUST be prefixed with agent and model identification:

```
[agent-name][model] → Tool: <tool-name>
[agent-name][model] → Target: <file/path/url>
```

For parallel calls: list ALL identifications BEFORE the tool calls.

### Common Violations to Avoid

```
❌ Missing: tool call with no identification prefix
✓ Correct: [agent-name][model] → Tool: WebFetch
           [agent-name][model] → Fetching: url
           <tool_call>...</tool_call>
```

<!-- DETAIL: Full violation examples
Incorrect: Calling tools without identification — no [agent][model] prefix before tool_call
Incorrect: Missing model — [secretary] → Tool: WebFetch (missing [model])
Correct: [secretary][opus] → Tool: WebFetch / [secretary][opus] → Fetching: url / then tool_call

Incorrect parallel: tool_call(url1), tool_call(url2), tool_call(cmd) — no identification
Correct parallel: list ALL [agent][model] → Tool/Fetching/Running lines FIRST, then all tool_calls
-->

## Models

| Model | Use |
|-------|-----|
| `opus` | Complex reasoning, architecture |
| `sonnet` | General tasks, code generation (default) |
| `haiku` | Fast simple tasks, file search |

## Tool Categories

| Category | Tools | Verb |
|----------|-------|------|
| File Read | Read, Glob, Grep | Reading / Searching |
| File Write | Write, Edit | Writing / Editing |
| Network | WebFetch | Fetching |
| Execution | Bash, Agent | Running / Spawning |

## Agent Tool Format

```
subagent_type:model → description
```

`subagent_type` MUST match actual Agent tool parameter. Custom names not allowed.

## Parallel Spawn Prefix Rule

When spawning 2+ agents in parallel, each agent's `description` parameter MUST include a `[N]` prefix (1-indexed) to enable correlation with the Running display:

```
Agent(description: "[1] Go code review", subagent_type: "lang-golang-expert")
Agent(description: "[2] Python code review", subagent_type: "lang-python-expert")
```

Single agent spawns do NOT use the `[N]` prefix.

This ensures the Running display:
```
⏺ Running 2 agents… (ctrl+o to expand)
   ├─ [1] Go code review · ...
   └─ [2] Python code review · ...
```

matches the spawn announcement:
```
[secretary][opus] → Spawning:
  [1] lang-golang-expert:sonnet → Go code review
  [2] lang-python-expert:sonnet → Python code review
```

## Example

```
[mgr-creator][sonnet] → Write: .claude/agents/new-agent.md
[secretary][opus] → Spawning:
  [1] lang-golang-expert:sonnet → Go code review
  [2] lang-python-expert:sonnet → Python code review
```

Parallel spawn description parameter:
```
Agent(description: "[1] Go code review", subagent_type: "lang-golang-expert", ...)
Agent(description: "[2] Python code review", subagent_type: "lang-python-expert", ...)
```

## 컨텍스트 드리프트 완화 (Multi-Turn 세션)

세션이 길어지면 프롬프트 기반 규칙(R008)이 컨텍스트에서 밀려 도구 호출 접두사가 누락될 수 있다. 다음 구간에서 실제 누락이 발생했다:

| 고위험 구간 | 이유 |
|------------|------|
| 서브에이전트 완료 알림(task-notification)으로 시작하는 턴 | 사용자 입력이 아니라서 규칙 상기 계기가 약함 |
| 도구 호출 5회 이상 이어지는 연쇄 진단 작업 | 초반 컨텍스트에만 규칙이 있고 매 호출마다 재확인하지 않음 |
| 병렬 에이전트 3개 이상 운용하며 결과를 수신·정리하는 구간 | 결과 취합에 집중하다 `[agent][model] → Tool:` 접두사를 생략하기 쉬움 |
| 컨텍스트 컴팩션 직후 재개 턴 | 요약이 규칙 본문을 대체한 것처럼 느껴지고, "서두 없이 바로 이어가라"는 재개 지시를 "접두사 없이"로 확대 해석하기 쉬움 |

**자가 점검**: 위 구간에서는 도구 호출 직전에 `[agent-name][model] →` 접두사 유무를 명시적으로 확인한다. 알림으로 턴이 시작되면 규칙이 리셋된 것처럼 느껴지지만 그렇지 않다 — 알림 턴도 사용자 입력 턴과 동일하게 R008이 적용된다.

**컴팩션 재개 판별 기준**: 재개 지시가 금지하는 것은 *요약에 대한 언급*("이어서 하겠습니다", "요약을 확인했습니다" 같은 메타 서두)이지 *도구 호출 접두사*가 아니다. `[agent-name][model] → Tool:` 접두사는 서두가 아니라 도구 호출의 구조적 일부이므로 어떤 재개 지시로도 면제되지 않는다.

**재발 이력**: 2026-08-18 세션에서 동일 세션 내 2회 누락(모두 사용자가 `@CLAUDE.md 재주입`으로 지적). 1차 발생 시 "나중에 규칙을 강화하겠다"고 미룬 것이 2차 재발의 원인이었다 — R016 안티패턴("나중에 고치겠다")의 실사례. 2026-08-26 세션에서는 컨텍스트 컴팩션 직후 재개 턴에 도구 호출 접두사가 R007 헤더와 함께 3회째 누락됐다 — 앞선 2회와 달리 사용자 입력 턴이 아니라 **자동 재개 턴**에서 발생했다. 직접 원인은 재개 시 시스템이 붙이는 "요약을 언급하지 말고 서두 없이 이어가라"는 지시를 접두사까지 생략하라는 뜻으로 확대 적용한 것이었다.
