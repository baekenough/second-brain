---
name: askeval-fake-embedder-fixture-fragility
description: askeval의 해시 bigram 가짜 임베더는 노이즈에 민감해 ctm류 픽스처 저작 시 의미론적 타당성만으로는 부족하다; windowAround #267 후속 tail-trim 버그 수정 요약도 포함
metadata:
  type: project
---

## windowAround #267 후속 버그 (2026-09-24, PR 준비 중, HEAD 7c67638)

`internal/api/ask_context.go`의 `windowAround`가 근거 구간(evidence span)을
먼저 budget 전체로 채운 뒤 `"[앞부분 생략]"`/`"[뒷부분 생략]"` 마커를
덧붙이는 순서였음 — 마커 바이트만큼 budget을 초과하면 `clipAskText`가
**끝에서부터** 잘라냈고, 근거가 문서 끝(통화 녹취록의 결론 문장)에 있으면
그 안전망이 근거 자체를 삭제했다. 수정: 마커 바이트를 창 계산 **이전에**
예약(`evidencePrefixMarker`/`evidenceSuffixMarker`), rune 경계 보정을
바깥으로 늘리는 대신(grow) 안쪽으로 줄이는(shrink) 방향으로 변경해 근거
구간을 절대 침범하지 않도록 구조적으로 보장. `askPassage`(어휘 폴백)와
`evidencePassage`의 청크 전용 폴백은 이미 마커를 먼저 예약하고 있어 동일
버그가 없었음 — 세 경로 모두 확인 후 하나만 고쳤다는 점이 재확인 포인트.

## fake 임베더의 노이즈 민감성 — 픽스처 저작 시 반드시 실측할 것

`internal/askeval/corpus.go`의 `hashedEmbedder`는 문자 bigram을 해시해
256차원 벡터에 누적한 뒤 L2 정규화하는 가짜 임베더다(`hashEmbed`). 두 청크가
**대부분 동일한 filler 텍스트**(`ctm-*` 픽스처의 반복 문장)를 공유하면,
실제 근거(fact) 텍스트가 기여하는 bigram 비중이 아주 작아 두 청크의 유사도
점수가 거의 동률(margin ~0.001~0.004)이 된다. 이 상태에서는 **의미론적으로
멀쩡한 픽스처 변경(예: 마무리 인사 문장 하나를 지우는 것)만으로도 어느
청크가 이기는지가 뒤집힐 수 있다** — 실제로 `ctm-01`의 content를 그대로
복사해 끝의 wrap-up 문장만 지웠더니 fact-chunk가 1등에서 2등으로 밀려
`context_hit=false`가 나왔다(품질 결함이 아니라 fake 임베더 아티팩트).

**대응**: 새 `call_transcript_mid_late`류 픽스처를 추가/수정할 때는
`cp.chunkVector(qvec, model.SearchQuery{}, N)`를 직접 호출해 fact 청크가
명확한 margin(ctm-01 기준 참고치 ~0.004 이상)으로 1등을 하는지 실측
확인해야 한다 — "의미론적으로 맞는 것 같다"는 근거가 되지 않는다. 이번에
쓴 튜닝 손잡이: 두 filler 문단의 반복 횟수 비율(앞 문단 45회 vs 뒷
문단 20회, 원래 둘 다 45회였음) — 뒷문단(=fact가 속한 청크)의 filler
비중을 줄이면 fact 텍스트의 상대적 bigram 비중이 올라가 margin이
커진다. `eval/ask/fixtures/ctm-06-tail-conclusion.json`이 이 결과물이다.

**관련**: [[project_search_rrf_relevance]] (다른 RRF/랭킹 미세조정 이력),
askeval 관련 첫 memory 항목.

## no_evidence/irrelevant_evidence 카테고리는 구조적으로 실패 불가 (2026-09-24, #266 리뷰)

`scriptedCompleter.synthesize`(`internal/askeval/llm.go`)는 `Gold.SupportSpans`가
비어 있으면(모든 `ne-*`/`ie-*` 픽스처가 그렇다 — `answerable:false`에
claims/spans 자체를 선언하지 않음) `for i, span := range f.Gold.SupportSpans`
루프가 0회 순회해 **항상** `abstentionAnswer`를 반환한다. 실측: `ne-01`
코퍼스에 질문과 정확히 매칭되는 문서("화성 이주 프로젝트 예산은
10억원으로 책정되었다")를 실제로 추가해 실제 파이프라인이 그 문서를
retrieval·synthesis 프롬프트에 넣도록 유도해도 오라클은 여전히 무조건
기권 문구만 반환해 여전히 PASS였다(`go run ./cmd/askeval --fixtures
<수정된 ne-01>` 재현). 즉 이 두 카테고리는 "무관한/상충하는 근거가
프롬프트에 있어도 합성 단계가 이를 근거로 답을 지어내지 않는가"라는,
카테고리 자체가 표방하는 목적(`docs/ask-evaluation-protocol.md`의
`irrelevant_evidence` 설명: "synthesis reaches Stage 3 but must still
abstain, not fabricate")을 **원천적으로 측정할 수 없다** — 가짜 LLM은
gold span이 없으면 지어낼 능력 자체가 없기 때문이다. 실패 가능한 유일한
경로는 하니스 배관(finish_reason/citation_status 분류 로직) 자체의
버그뿐이다. 이는 버그가 아니라 문서화되지 않은 설계 한계이므로, 이
카테고리들의 "PASS"를 "실제 LLM이 무관한 근거로 환각을 만들지 않는다는
증거"로 읽으면 안 된다.

**관련**: [[project_ask_citation_validator_edge_cases]] (같은 리뷰의
Part B, 검증기 자체 버그).

## ctm-01~05 하한 0.02 통일 — 이슈 #272 (2026-09-24, PR 준비 중)

ctm-06와 같은 기법으로 ctm-01~05도 재작성해 `wantCTMVectorMargin` 하한을
6건 모두 0.02로 통일했다(기존 0.0008~0.005). 재현한 핵심 함정 두 가지:

1. **head/tail에 서로 다른 filler를 쓰는 것만으로는 부족하다.** 처음 시도한
   ctm-01 head filler("회의 초반에는 팀별 역할 분담 현황을 공유했고...")는
   단독으로 질의와 코사인 0.59를 찍었다 — 48차원 해시 공간에서는 특정
   문장의 bigram 분포가 순전히 우연으로 질의와 크게 겹칠 수 있다(하나의
   문장을 반복만 하면 L2 정규화 후 벡터가 "그 한 문장의 순수한 프로필"이
   되므로 오히려 tail의 여러 문장이 섞인 벡터보다 우연한 정렬 위험이 더
   크다). alias 문장을 늘려 tail을 보강해도 head의 우연한 정렬을 못
   이겼다(margin이 음수로 악화됨 — alias 반복이 tail 자체의 정규화 방향을
   흐리는 부작용도 있었다). **해결**: head filler 후보 풀을 여러 개
   준비해 각 fixture의 실제 folded query에 대해 `cosineSim`을 실측하고
   가장 낮은 것을 채택(0.09~0.21로 낮춤) — "의미론적으로 무관해 보인다"는
   감으로 고르면 안 된다, 반드시 실측.
2. **reps 개수를 후보 문장 길이에 맞춰 재계산해야 한다.** 헤드 문장 길이가
   후보마다 다른데 reps=50 고정으로 반복하면 총 rune 수가 chunker
   TargetSize(2000)+tail 임계값(~1937) 밑으로 떨어져 head/tail이 병합돼
   **청크가 1개**가 되고(`chunks=1`), gold span을 가진 유일한 청크가
   runnerUp 없이 margin=+Inf로 잘못 통과한다 — 이는 "항상 통과하는
   깨진 측정"이었다. `reps = (targetRunes - introRunes) / (sentRunes+1)`로
   문장 길이에 맞춰 동적 계산해야 chunks=2가 안정적으로 유지된다.

결과: 6개 모두 margin 0.28~0.40(전부 0.02 대비 15배 이상 버퍼). tail
구조는 ctm-06과 달리 마무리 문장(wrap-up)을 유지해 "정답이 문서의 리터럴
마지막 바이트가 아님" 불변 조건([[project_ask_citation_validator_edge_cases]]
이 아니라 `TestRun_CallTranscriptMidLate_MatchedChunkEvidence`의 독스트링이
명시)을 그대로 보존했다 — ctm-06만 tail 없이 마지막 바이트 케이스를
전담한다.

**#267 대조군 재현(내부 접근 제한 하에서)**: `internal/api/ask_context.go`의
`askPassage`는 다른 패키지의 비공개 함수라 import 불가 — 대신 알고리즘을
읽기 전용으로 포팅해(`windowRunes := (budget - 2*len(marker)) / utf8.UTFMax`,
이 `/UTFMax` 나눗셈을 빠뜨리면 window가 사실상 문서 전체를 덮어버려
오탐(pre-#267도 "찾음"으로 나옴) — 실제로 한 번 이 실수를 했다) 6개
fixture 모두에서 pre-#267 경로가 gold fact를 못 찾음(`false`)을 확인,
현재 파이프라인(`go run ./cmd/askeval`)은 43/43 전부 통과로 대조. 포팅
스크립트는 검증 후 삭제(영구 테스트 아님, 파일 스코프 제약으로 internal/api
자체는 건드리지 않음).

**결함 상태 재현 검증**: `wantCTMVectorMargin` 하한을 0.02로 올린 뒤,
ctm-01을 git HEAD의 구버전(margin 0.0041)으로 일시 교체해
`TestRun_CTMFakeVectorMargin`이 실제로 FAIL하는지 확인(FAIL 메시지에
fixture ID·양쪽 점수 정상 출력) 후 원복 — "늘 통과하는 검증"이 아님을
증명하는 절차. [[feedback_test_filter_silent_skip]]의 "정적 검증 통과 ≠
실행 가능" 원칙과 같은 계열.

**관련**: [[project_search_rrf_relevance]], 이슈 #266/#267/#272.
