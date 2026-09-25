---
name: project-upstream-error-body-288
description: #288 2·3항(v0.25.4 PR-A) — internal/httperr 로 외부 API 오류 본문 정제·성공 본문 상한, actions 본문 8KiB·note NUL 거부; 계획의 internal/upstream 대신 httperr 인 이유
metadata:
  type: project
---

#288 2·3항은 계획(deep-plan-203222)의 `internal/upstream` 대신 **`internal/httperr`** 로 구현했다(2026-09-25). 이유: #297(logsafe, whisper 로그)을 다른 에이전트가 별도 worktree 에서 main 기준 독립 브랜치로 동시에 구현해서, 패키지 이름이 겹치지 않게 오케스트레이터가 지시했다. #297 쪽 whisper W13/W14(API 오류 본문)가 이 헬퍼를 재사용하려면 병합 후 정리해야 할 수 있다.

**Why:** 스택 PR squash 충돌을 피하려고 두 PR 을 독립 브랜치로 나눴다.

**How to apply:**
- `StatusError.Error()` 는 기존 접두사를 그대로 유지한다("embed API status 429 (error.type=…, code=…)"). `rerank API status %d` 는 본문을 파싱하지 않아 문구가 정확히 같다(rerank_test 가 고정).
- type/code 정규식은 `^[a-z][a-z_.-]{0,63}$` 로 **숫자를 뺐다**. 숫자를 허용하면 프록시가 type 자리에 전화번호를 되돌려 줄 때 그대로 통과한다.
- embed 배치 상한의 float 당 바이트는 **48**(처음엔 32). OpenAI 응답은 들여쓰기 + float 한 줄에 하나 + float64 최단 표기(~20자)라 실측 2칸 30.4B, 4칸+CRLF 39.4B/float 다. 32 면 정상 응답이 거부되고 비재시도라 백필이 매 틱 실패한다(보안 리뷰 지적).
- embed 재시도 판정은 `classifyStatus`(상태 코드만)로 모았다. 달라진 점 하나: 비-200 응답의 본문 읽기 오류는 이제 상태 코드대로 판정한다(예전에는 읽기 오류면 4xx 도 재시도했다).
- llm 은 `ErrBodyTooLarge` 를 비재시도 조건에 추가했다(`isClientError` 는 타입 단언이라 errors.Is 를 따로 걸었다).
- D4(note_enrichment LLM 응답 200B 로그)는 처음에 보고만 했다가, 오케스트레이터가 #297 이 그 파일을 건드리지 않는다고 확인한 뒤 PR-A 에 넣었다(`response_len` 만 남김). Go 1.26 encoding/json 오류가 담는 것: SyntaxError 는 문자 1개, UnmarshalTypeError 는 JSON 종류와 스키마 필드 이름(숫자 리터럴 없음).
