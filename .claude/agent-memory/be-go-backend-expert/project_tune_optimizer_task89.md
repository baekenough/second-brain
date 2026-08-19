---
name: tune-optimizer-task89
description: Part D Task 8/9 — 좌표탐색·승격게이트·cmd/tune 배치와 PII-free 관측에서 계획서와 갈라진 판단들(게이트 사유 enum+Describe 분리, TUNE_ENABLED가 롤백까지 게이트, span은 성공 시에만)
metadata:
  type: project
---

Part D Task 8(좌표 탐색 + 승격 게이트)·Task 9(관측) 구현에서 내린, 계획서 문면과 갈라지거나 코드만 봐서는 이유가 안 보이는 판단들.

**1. 게이트 사유는 enum 코드로 반환하고, 사람이 읽을 문장은 `tune.Describe(code, GateInput)`가 따로 만든다.**
계획서는 `Eligible`/`PassesPromotion`이 `(bool, string)`을 주고 그 문자열이 DB `gate_reason`("holdout distinct queries 12 < 30")에 들어간다고 썼는데, 같은 문자열이 Langfuse `tune.gate_reason` 속성으로도 나간다. 자유 텍스트를 span에 실으면 사용자 입력이 섞여 들어갈 경로가 열리므로, 반환값은 닫힌 집합(`ok`/`holdout_too_small`/`ndcg_gain_below_threshold`/`fp_penalty_regression`)으로 두고 숫자가 붙은 문장은 Postgres로만 보낸다.
**How to apply:** 게이트 사유를 늘릴 때 상수를 추가하고 `Describe`의 switch도 같이 채운다. span에는 절대 `Describe` 결과를 싣지 않는다.

**2. `TUNE_ENABLED`는 `-rollback`까지 게이트한다.**
"플래그 미설정 시 아무것도 실행되지 않는다"를 문자 그대로 지켰다. 롤백만 열어두면, 튜닝을 켠 적 없는 호스트에서 이 바이너리가 live 설정을 바꿀 수 있다. 대신 값싼 롤백 경로는 이 바이너리가 아니라 "플래그 끄기 + active 행 방치 = 컴파일 기본값"이다.
**How to apply:** 운영자 안내는 `TUNE_ENABLED=true tune -rollback` 한 줄로.

**3. 플래그 off는 exit 0(경고 로그).** 주 1회 cron이 의도적으로 꺼둔 기능 때문에 매주 에러를 메일링하면 알림이 무시되기 시작한다.

**4. Task 3이 붙여둔 evidence span은 Task 9에서 의미가 바뀌었다.**
이름 `feedback.evidence` → `feedback.evidence.recorded`, 속성 5개(`query_hash`/rank/layer/split/thumbs) → 3개(`document_id`/`thumbs`/`split`), 그리고 **`query_hash`도 span에서 제거**했다(로컬 로그에는 남긴다 — Langfuse 보존기간이 로그보다 길다). span은 upsert 성공 후에만 생성한다: 이름이 DB에 대한 주장이므로 저장 실패한 요청이 타임라인에 있으면 거짓이 된다. 결과적으로 span이 DB 요청을 감싸지 않으므로 DB 지연 시간은 이 span에 안 잡힌다(의도).

**5. `weights.action.applied` span은 `internal/store/weights_history.go`가 아니라 `cmd/tune`에서 낸다.** 계획서 Task 9 "호출 계약"은 store 안을 지목했지만, Task 9의 편집 대상 파일 목록에 store가 없고 store에 otel 의존을 새로 들이는 대가가 정보 이득보다 크다. promote/rollback 직후 호출 지점에서 같은 정보가 나온다.

**6. holdout은 `tune.Judge` 한 함수에서만, 정확히 2회 만진다.** baseline→candidate 순서. 실패한 평가도 예산을 쓰므로 에러 시 재시도하지 않고 런을 끝낸다(재시도 루프는 holdout 튜닝과 구분되지 않는다). `Coordinate` 시그니처에는 `*dataset.HoldoutSet` 자리가 아예 없다.

**7. 좌표 탐색 시작점은 격자에 스냅한다.** `SearchWeights{}.Defaults()`의 `SummaryVec=0`은 "커버리지 게이트에 위임"이라는 별도 의미라, 스냅하지 않으면 최적화기가 측정한 적 없는 설정을 시작점으로 보고하게 된다. 레인 끄기는 격자 밖 후보 1개(`DisableSummaryVec=true`)로 마지막에 한 번만 평가한다.

관련: [[feedback-loop-partd]], [[store-sql-test-harness]]
