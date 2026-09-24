---
name: ask-citation-validator-edge-cases
description: internal/api/ask_citation.go validateAskCitations 실측 버그 2건(하이픈 인접 bare 인용 오탐 invalid, 좁은 거부구문 allowlist로 오탐 missing) — 2026-09-24 커밋 0b1a673로 수정 완료
metadata:
  type: project
---

## 수정 완료 (2026-09-24, 커밋 0b1a673, feature/v0.23.0)

이 문서가 기록한 버그 1(하이픈 인접 bare 인용 오탐 invalid)과 버그 2(좁은
거부구문 allowlist로 오탐 missing), 그리고 별도로 발견된 빈 ID
(`[근거](/documents/)` → 오탐 missing)까지 3건 모두
`internal/api/ask_citation.go`에서 같은 커밋으로 수정됨. 각 수정은
revert-check(구 코드에서 실패 확인 후 픽스 적용)를 거친
`internal/api/ask_citation_test.go` 테스트로 고정됨
(`TestValidateAskCitations_TrailingProseAfterBareID`,
`TestValidateAskCitations_EmptyDocumentsLink`,
`TestIsAskAbstentionAnswer_ParaphrasedRefusals`). 버그 1은 정규식을
`[A-Za-z0-9-]+` 그리디 캡처 대신 고정 8-4-4-4-12 UUID 형태로 앵커링하고,
"/documents/" 출현 위치를 단일 스캔으로 순회해 유효/malformed를 한 번에
판정하도록 재구성해 해결(부수 효과로 host 포함 전체 URL도 정상 인식하게
됨 — `reCitationShapedLink` 루프의 `HasPrefix`를 `Contains`로 완화).
버그 2는 동사 어간(확인/판단/파악/언급/기재/나와) + 부정 어미 패턴에
"제공된 정보/문서/자료/기록/내용" 언급 요구를 결합해 해결(일반 부정문
오탐 방지). 이 문서의 "제안" 섹션들은 실제로 채택된 접근과 대체로
일치한다.

## 실측 방법 (2026-09-24, #268 리뷰, 수정 전 기록)

`validateAskCitations`(`internal/api/ask_citation.go`)와 의존 타입
(`askPromptManifest` 등, `ask_evidence.go`)을 스크래치패드에 그대로
복사해 독립 Go 모듈로 만들고(`google/uuid`만 의존, 오프라인 모듈
캐시로 해결됨), 현실적인 한국어 `/ask` 답변 문자열을 넣어 status를
검증했다. 리포 파일은 건드리지 않았다(read-only 리뷰).

## 버그 1: 공백 없이 하이픈이 붙은 bare 인용 → 오탐 invalid

`reDocumentsLink = regexp.MustCompile(`/documents/([A-Za-z0-9-]+)`)`의
문자 클래스가 `-`를 포함하므로, markdown 링크가 아닌 **bare** 참조
(`/documents/<uuid>`가 `[근거](...)` 괄호 밖에 맨 텍스트로 등장 —
독스 코멘트가 명시적으로 유효한 인용 형태로 인정: "whether it sits
inside a markdown link's URL... or appears bare in the text") 바로
뒤에 공백 없이 하이픈으로 시작하는 한국어 단어가 붙으면(예: `/documents/
<uuid>-요약 문서 참고`) 정규식이 그 트레일링 하이픈까지 토큰에 흡수한다.
그런데 `strings.TrimRight(token, ".,;:!?)]。）")`의 트림 문자셋에는
`-`가 없어서 하이픈이 안 잘리고 `uuid.Parse`가 실패 →
`MalformedLinks++` → 전체 답변 status가 `invalid`로 오탐된다. 실제
markdown 링크 형태(`[근거](/documents/<uuid>)`)는 괄호가 매치를
끊어주므로 영향받지 않는다 — bare 참조 + 하이픈 인접이라는 좁지만
실재하는 조합에서만 터진다.

재현(스크래치패드 probe 출력):
```
answer: "근거: /documents/11111111-1111-1111-1111-111111111111-요약 문서 참고"
status=invalid cited=[] unknown=[] malformed=1
```

**제안**: TrimRight 문자셋에 `-`를 추가하거나(단, 그러면 실제 UUID의
트레일링 하이픈은 UUID 문법상 존재할 수 없으므로 안전), 혹은 정규식
캡처를 UUID 정규 형식(`[0-9a-fA-F]{8}-...`고정 길이)으로 앵커링해
애초에 여분 문자를 흡수하지 않게 한다.

## 버그 2: askRefusalPhrases allowlist가 좁아 자연스러운 거부 표현을 놓침 → 오탐 missing

`askRefusalPhrases`(`ask_citation.go`)는 4개 문구뿐이다: "답변할 수
없습니다", "알 수 없습니다", "확인할 수 없습니다", "찾을 수 없습니다"
— 전부 "~할 수 없다" 양태 표현이다. 시스템 프롬프트 규칙 2가 정확히
"제공된 정보로는 답변할 수 없습니다"라는 고정 문구를 지시하지만, 실제
LLM은 이를 자주 다른 문법으로 의역한다. 다음 세 변형을 실측했고 전부
인용 0건 상태에서 `askCitationMissing`(규칙6 위반 뉘앙스)으로 오탐됐다
— 마땅히 `askCitationAbstained`(정상적인 기권)여야 한다:

```
"제공된 정보로는 확인되지 않습니다."           → status=missing (피동형, "확인할 수 없습니다"와 문법이 다름)
"제공된 정보만으로는 판단하기 어렵습니다."       → status=missing
"질문하신 내용은 문서에 언급되어 있지 않습니다." → status=missing
```

반대로 거부 문구 + 실제 인용이 함께 있는 정상 케이스("해당 내용은
확인되지 않습니다 [근거](/documents/<real-id>)")는 올바르게
`valid`로 처리됨(인용이 있으면 우선순위상 valid로 감 — 이 조합
자체는 버그 아님).

**영향**: `citation_status=missing`은 클라이언트에 "모델이 근거 없이
사실을 주장했다"는 신호로 노출되는데, 실제로는 모델이 정상적으로
기권했을 뿐이다 — 의미를 오보한다(사용자 과업 설명의 "A false missing
misreports"). 히스토리 재생 로직(`recentAskHistory`)은 missing과
abstained를 둘 다 포함시키므로 재생 자체는 안 깨지지만, citation
품질 지표·알림이 이 필드를 신뢰한다면 실제보다 "미인용" 비율이
부풀려진다.

**제안**: 정확한 문구 매칭 대신 형태소/어미 패턴("~않습니다",
"~어렵습니다", "~없습니다" 계열 + "확인/판단/파악/언급" 등 핵심
동사 어근)으로 완화하거나, 최소한 실측된 3개 변형을 allowlist에
추가한다.

## 확인 완료 — 버그 아님 (오탐 가설 기각)

- **budget-omitted 문서 인용 → invalid**: 의도된 설계(#268 finding
  F2, 기존 테스트에도 고정됨). `buildBudgetedAskMessages`의 생략
  분기(`"[입력 예산으로 추가 문서 생략]\n"`, `ask_context.go` L361)는
  생략된 문서의 ID/제목을 프롬프트에 전혀 적지 않는다 — 모델이 그
  ID를 정당하게 볼 방법이 없으므로 조작된 ID와 동일하게 invalid
  처리하는 것이 맞다.
- 전체 URL(호스트 포함), 대문자 UUID, 중복 인용, markdown 링크 중간
  개행, 후행 마침표/괄호/한국어 따옴표 — 전부 정상적으로 valid 처리됨.
  `google/uuid.Parse`는 대소문자 혼용을 문제없이 처리.
- `[추론]` 절 안의 인용은 `InferredCitedIDs`로 올바르게 별도 플래그.

**관련**: [[project_askeval_fake_embedder_fixture_fragility]] (같은
리뷰의 Part A, askeval 하네스 자체의 설계 한계).
