package intent_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/timeutil"
)

// DeterministicPlan 은 LLMPlanner 의 LLM 없는 선처리를 밖으로 꺼낸 것이다.
// 오프라인 도구(cmd/eval --plan-sources)가 운영과 같은 창·소스 집합을 쓰려면
// 이 둘이 한 구현을 공유해야 하고, LLMPlanner 의 동작은 한 글자도 달라지면
// 안 된다. LLM 클라이언트가 없는 플래너로 Plan 을 돌리면 결정론 경로에서
// 끝난 질문만 OriginDeterministic 이 나오므로, 같은 골든 표 전체에서 두 경로를
// 대조할 수 있다.
func TestDeterministicPlan_MatchesLLMPlannerPreFastPath(t *testing.T) {
	t.Parallel()
	now := planNowUTC.In(timeutil.KST())
	for _, c := range planGolden {
		c := c
		t.Run(c.question, func(t *testing.T) {
			t.Parallel()
			viaPlanner := newPlanner(t, nil, planNowUTC).Plan(context.Background(), c.question)
			direct, ok := intent.DeterministicPlan(c.question, now)

			if ok != (viaPlanner.Origin == intent.OriginDeterministic) {
				t.Fatalf("DeterministicPlan ok=%v but Plan origin=%v", ok, viaPlanner.Origin)
			}
			if !ok {
				return
			}
			// LLMPlanner 는 자기 Limit 만 더한다 — 그 밖의 모든 필드가 같아야 한다.
			if direct.Limit != 0 {
				t.Errorf("DeterministicPlan 이 Limit(%d)을 채웠다; 호출자의 몫이다", direct.Limit)
			}
			direct.Limit = planLimit
			if !reflect.DeepEqual(direct, viaPlanner) {
				t.Errorf("LLMPlanner 결과가 DeterministicPlan 과 다르다:\n direct: %+v\n planner: %+v", direct, viaPlanner)
			}
			if c.deterministic {
				if !sameSourceSet(direct.SourceTypes, c.wantSources) {
					t.Errorf("SourceTypes = %v, want %v", direct.SourceTypes, c.wantSources)
				}
			}
		})
	}
}

// 결정론 경로가 거절하는 질문(복합 기록 질문)은 ok=false 여야 한다 — 호출자는
// 이때 소스 제약을 만들면 안 된다.
func TestDeterministicPlan_DeclinesWhenNoPhrase(t *testing.T) {
	t.Parallel()
	now := planNowUTC.In(timeutil.KST())
	if _, ok := intent.DeterministicPlan("보험 약관 정리해줘", now); ok {
		t.Fatal("기간 표현이 없는 질문이 결정론 계획으로 해석됐다")
	}
}
