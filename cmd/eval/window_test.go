package main

import (
	"context"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/baekenough/second-brain/internal/timeutil"
)

// --window=plan 은 골든 후보 화면이 쓰는 것과 같은 파서를 써야 한다. 여기서
// 두 경로가 갈라지면, 사람이 화면에서 1~2위로 본 문서가 평가에서는 묻히는
// 지금의 불일치가 이름만 바꾼 채 그대로 남는다.
func TestPlanWindowMatchesDeterministicParser(t *testing.T) {
	asOf := time.Date(2026, 9, 21, 9, 0, 0, 0, timeutil.KST())
	resolve := planWindowResolver(asOf)

	gotFrom, gotTo, ok := resolve("어제 뭐 했지")
	if !ok {
		t.Fatal("period phrase was not matched")
	}
	wantFrom, wantTo, _, wantOK := intent.DeterministicWindow("어제 뭐 했지", asOf.In(timeutil.KST()))
	if !wantOK || !gotFrom.Equal(wantFrom) || !gotTo.Equal(wantTo) {
		t.Fatalf("window = [%s, %s), want [%s, %s)", gotFrom, gotTo, wantFrom, wantTo)
	}
	// 어제(2026-09-20) 하루의 KST 반열린 구간이어야 한다.
	if gotFrom.In(timeutil.KST()).Format(time.RFC3339) != "2026-09-20T00:00:00+09:00" ||
		gotTo.In(timeutil.KST()).Format(time.RFC3339) != "2026-09-21T00:00:00+09:00" {
		t.Fatalf("unexpected KST day bounds: [%s, %s)", gotFrom, gotTo)
	}
}

// 기준 시각을 바꾸면 같은 문구가 다른 창으로 해석돼야 한다 — --as-of 가
// 실제로 먹히는지의 증거다.
func TestPlanWindowAnchorsToAsOf(t *testing.T) {
	early := planWindowResolver(time.Date(2026, 3, 5, 12, 0, 0, 0, timeutil.KST()))
	late := planWindowResolver(time.Date(2026, 9, 21, 12, 0, 0, 0, timeutil.KST()))
	earlyFrom, _, okEarly := early("오늘 일정")
	lateFrom, _, okLate := late("오늘 일정")
	if !okEarly || !okLate {
		t.Fatal("period phrase was not matched")
	}
	if earlyFrom.Equal(lateFrom) {
		t.Fatalf("--as-of ignored: both resolved to %s", earlyFrom)
	}
}

// 기간 표현이 없는 질의는 시간창 없이 검색해야 한다. 여기서 임의의 창을
// 만들어 붙이면 평가가 운영보다 좁은 코퍼스를 재게 된다.
func TestPlanWindowLeavesUndatedQueriesUnconstrained(t *testing.T) {
	resolve := planWindowResolver(time.Date(2026, 9, 21, 12, 0, 0, 0, timeutil.KST()))
	if _, _, ok := resolve("보험 약관 정리해줘"); ok {
		t.Fatal("a query with no period phrase received a window")
	}
}

// 해석된 창은 실제로 검색 질의의 occurred_at 범위까지 전달돼야 한다.
// 시간창을 "정렬 힌트" 로 흘려보내면 후보에 들어오지도 못한 문서를 정렬할 수
// 없으므로 아무것도 달라지지 않는다.
func TestWindowReachesTheSearchQuery(t *testing.T) {
	asOf := time.Date(2026, 9, 21, 9, 0, 0, 0, timeutil.KST())
	stub := newTracingStub(false)
	pairs := []store.EvalPair{
		{Query: "어제 통화", RelevantDocIDs: []string{labelDocID.String()}},
		{Query: "보험 약관", RelevantDocIDs: []string{labelDocID.String()}},
	}
	evaluatePairs(context.Background(), stub, pairs, evalRunOptions{window: planWindowResolver(asOf)})
	close(stub.lastQuery)

	windowed, unwindowed := 0, 0
	for q := range stub.lastQuery {
		switch {
		case q.OccurredFrom != nil && q.OccurredTo != nil:
			windowed++
			if !q.OccurredFrom.Before(*q.OccurredTo) {
				t.Fatalf("window is not a half-open range: [%s, %s)", q.OccurredFrom, q.OccurredTo)
			}
		case q.OccurredFrom == nil && q.OccurredTo == nil:
			unwindowed++
		default:
			t.Fatalf("half-set window: from=%v to=%v", q.OccurredFrom, q.OccurredTo)
		}
	}
	if windowed != 1 || unwindowed != 1 {
		t.Fatalf("windowed=%d unwindowed=%d, want 1 and 1", windowed, unwindowed)
	}
}

// 기본값(--window=none)은 예전 그대로 시간창 없이 검색한다.
func TestDefaultModeSendsNoWindow(t *testing.T) {
	stub := newTracingStub(false)
	evaluatePairs(context.Background(), stub,
		[]store.EvalPair{{Query: "어제 통화", RelevantDocIDs: []string{labelDocID.String()}}},
		evalRunOptions{})
	close(stub.lastQuery)
	for q := range stub.lastQuery {
		if q.OccurredFrom != nil || q.OccurredTo != nil {
			t.Fatalf("default mode applied a window: from=%v to=%v", q.OccurredFrom, q.OccurredTo)
		}
	}
}

// 시간창 모드는 설정 정체성의 일부다. plan 실행이 none 실행의 baseline 과
// 같은 해시로 묶이면, 시간창 덕에 오른 점수를 검색 품질 개선으로 읽게 된다.
func TestWindowModeFormsASeparateBaseline(t *testing.T) {
	asOf := time.Date(2026, 9, 21, 9, 0, 0, 0, timeutil.KST())
	base := map[string]any{"protocol": "x"}
	none := map[string]any{"protocol": "x"}

	applyWindowProfile(none, windowModeNone, asOf)
	if digest(none) != digest(base) {
		t.Fatal("default mode changed the config hash and invalidated existing baselines")
	}

	plan := map[string]any{"protocol": "x"}
	applyWindowProfile(plan, windowModePlan, asOf)
	if digest(plan) == digest(base) {
		t.Fatal("plan mode reused the default baseline")
	}

	// 기준 날짜가 다르면 시간창도 다르므로 또 다른 baseline 이어야 한다.
	otherDay := map[string]any{"protocol": "x"}
	applyWindowProfile(otherDay, windowModePlan, asOf.AddDate(0, 0, -1))
	if digest(otherDay) == digest(plan) {
		t.Fatal("a different as-of date reused the same baseline")
	}

	// 같은 날 안에서 시각만 다른 실행은 같은 시간창을 만든다 — 해시도 같아야
	// 한다. 초 단위를 넣으면 매 실행이 고유해져 baseline 이 영영 안 맞는다.
	sameDay := map[string]any{"protocol": "x"}
	applyWindowProfile(sameDay, windowModePlan, asOf.Add(7*time.Hour))
	if digest(sameDay) != digest(plan) {
		t.Fatal("same KST day produced different baselines")
	}
}
