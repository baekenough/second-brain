package api

import (
	"context"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/timeutil"
)

// planTestWindow returns a half-open KST window [from, to) for the given
// "YYYY-MM-DD" dates — the shape intent.QueryPlan carries.
func planTestWindow(t *testing.T, from, to string) (*time.Time, *time.Time) {
	t.Helper()
	f, err := time.ParseInLocation("2006-01-02", from, timeutil.KST())
	if err != nil {
		t.Fatalf("parse %q: %v", from, err)
	}
	tt, err := time.ParseInLocation("2006-01-02", to, timeutil.KST())
	if err != nil {
		t.Fatalf("parse %q: %v", to, err)
	}
	return &f, &tt
}

// TestAssembleRetrieval_PlanWindowAndSourcesReachObservedLane is the whole
// point of the planner: whatever it decided must arrive at the store as a
// candidate-set constraint. A plan that is computed and then dropped is
// indistinguishable from no plan at all — that was the original defect
// (windows were computed, then translated into a sort hint).
func TestAssembleRetrieval_PlanWindowAndSourcesReachObservedLane(t *testing.T) {
	t.Parallel()

	from, to := planTestWindow(t, "2026-08-20", "2026-08-21")
	searcher := &recordingSearcher{}
	params := intent.Params{RawQuery: "내일 일정 알려줘", Kind: intent.KindGeneral}
	plan := intent.QueryPlan{
		OccurredFrom: from,
		OccurredTo:   to,
		SourceTypes:  []model.SourceType{model.SourceCalendar},
		Limit:        8,
		Reason:       "내일(8/20) 캘린더만 조회",
		Origin:       intent.OriginDeterministic,
	}

	if _, err := assembleRetrieval(context.Background(), searcher, params, plan, 8, 3); err != nil {
		t.Fatalf("assembleRetrieval() error = %v", err)
	}
	if len(searcher.calls) != 2 {
		t.Fatalf("got %d searches, want 2", len(searcher.calls))
	}

	obs := searcher.calls[0]
	if obs.OccurredFrom == nil || !obs.OccurredFrom.Equal(*from) {
		t.Errorf("observed OccurredFrom = %v, want %v", obs.OccurredFrom, *from)
	}
	if obs.OccurredTo == nil || !obs.OccurredTo.Equal(*to) {
		t.Errorf("observed OccurredTo = %v, want %v", obs.OccurredTo, *to)
	}
	got := obs.IncludeSourceTypes()
	if len(got) != 1 || got[0] != model.SourceCalendar {
		t.Errorf("observed include set = %v, want [calendar]", got)
	}
	if obs.Limit != 8 {
		t.Errorf("observed Limit = %d, want 8", obs.Limit)
	}
}

// TestAssembleRetrieval_SourceOnlyPlan covers golden row #11 — a plan with no
// window at all. Spec §5.1 flags this shape as the one that exposes the chunk
// lanes, because the "window set" gate that accidentally hid the leak is off.
func TestAssembleRetrieval_SourceOnlyPlan(t *testing.T) {
	t.Parallel()

	searcher := &recordingSearcher{}
	plan := intent.QueryPlan{
		SourceTypes: []model.SourceType{model.SourceCallTranscript, model.SourceCallLog},
		Limit:       8,
		Reason:      "통화 기록만 조회",
		Origin:      intent.OriginLLM,
	}

	if _, err := assembleRetrieval(context.Background(), searcher, intent.Params{RawQuery: "홍길동이랑 통화한 내용"}, plan, 8, 3); err != nil {
		t.Fatalf("assembleRetrieval() error = %v", err)
	}

	obs := searcher.calls[0]
	if obs.OccurredFrom != nil || obs.OccurredTo != nil {
		t.Errorf("observed query carries a window [%v, %v); the plan had none", obs.OccurredFrom, obs.OccurredTo)
	}
	if got := obs.IncludeSourceTypes(); !sameSourceTypeSet(got, plan.SourceTypes) {
		t.Errorf("observed include set = %v, want %v", got, plan.SourceTypes)
	}
}

// TestAssembleRetrieval_FallbackPlanIsUnconstrained: spec §6.1 defines the
// fallback as exactly the pre-planner behaviour, not a new failure mode.
func TestAssembleRetrieval_FallbackPlanIsUnconstrained(t *testing.T) {
	t.Parallel()

	searcher := &recordingSearcher{}
	plan := intent.QueryPlan{Limit: 8, Reason: "시간·소스 제약 없이 전체 검색", Origin: intent.OriginFallback}

	if _, err := assembleRetrieval(context.Background(), searcher, intent.Params{RawQuery: "질문"}, plan, 8, 3); err != nil {
		t.Fatalf("assembleRetrieval() error = %v", err)
	}

	obs := searcher.calls[0]
	if obs.OccurredFrom != nil || obs.OccurredTo != nil {
		t.Errorf("fallback plan produced a window [%v, %v)", obs.OccurredFrom, obs.OccurredTo)
	}
	if got := obs.IncludeSourceTypes(); len(got) != 0 {
		t.Errorf("fallback plan produced an include set %v", got)
	}
}

// TestAssembleRetrieval_PlanOwnsTheWindow_NotParams pins spec §3.2's split:
// intent.Params shapes RANKING, the plan decides RETRIEVAL. Two components
// writing the same field is how the window and the prompt drifted apart
// before; there must be exactly one source of truth.
func TestAssembleRetrieval_PlanOwnsTheWindow_NotParams(t *testing.T) {
	t.Parallel()

	stale, staleTo := planTestWindow(t, "2020-01-01", "2020-01-02")
	searcher := &recordingSearcher{}
	params := intent.Params{
		RawQuery:     "오늘 일정",
		Kind:         intent.KindTemporal,
		Confidence:   1,
		OccurredFrom: stale,
		OccurredTo:   staleTo,
	}
	plan := intent.QueryPlan{Limit: 8, Reason: "제약 없음", Origin: intent.OriginFallback}

	if _, err := assembleRetrieval(context.Background(), searcher, params, plan, 8, 3); err != nil {
		t.Fatalf("assembleRetrieval() error = %v", err)
	}

	obs := searcher.calls[0]
	if obs.OccurredFrom != nil || obs.OccurredTo != nil {
		t.Errorf("intent.Params' window leaked into the query: [%v, %v)", obs.OccurredFrom, obs.OccurredTo)
	}
}

// TestAssembleRetrieval_RankingShapingSurvivesAPlan: the plan constrains the
// candidate set; entity/exact-token weights still shape the ordering within it.
func TestAssembleRetrieval_RankingShapingSurvivesAPlan(t *testing.T) {
	t.Parallel()

	from, to := planTestWindow(t, "2026-08-19", "2026-08-20")
	searcher := &recordingSearcher{}
	params := intent.Params{RawQuery: "오늘 홍길동 통화", Kind: intent.KindEntity, Confidence: 0.9}
	plan := intent.QueryPlan{OccurredFrom: from, OccurredTo: to, Limit: 8, Reason: "오늘", Origin: intent.OriginDeterministic}

	if _, err := assembleRetrieval(context.Background(), searcher, params, plan, 8, 3); err != nil {
		t.Fatalf("assembleRetrieval() error = %v", err)
	}

	obs := searcher.calls[0]
	if obs.Weights.EntityWeight != 1.5 {
		t.Errorf("EntityWeight = %v, want 1.5", obs.Weights.EntityWeight)
	}
	if obs.OccurredFrom == nil {
		t.Error("plan window was dropped when entity shaping applied")
	}
}

// TestAssembleRetrieval_PlanLimitZeroFallsBackToTopK keeps a malformed or
// zero-valued plan from asking the store for nothing.
func TestAssembleRetrieval_PlanLimitZeroFallsBackToTopK(t *testing.T) {
	t.Parallel()

	searcher := &recordingSearcher{}
	plan := intent.QueryPlan{Reason: "제약 없음", Origin: intent.OriginFallback}

	if _, err := assembleRetrieval(context.Background(), searcher, intent.Params{RawQuery: "질문"}, plan, 8, 3); err != nil {
		t.Fatalf("assembleRetrieval() error = %v", err)
	}
	if got := searcher.calls[0].Limit; got != 8 {
		t.Errorf("observed Limit = %d, want the topK default 8", got)
	}
}

// TestAssembleRetrieval_InsightOnlyPlanStillSkipsObservedLane documents the
// contract the planner relies on: even if an insight-only plan somehow arrives
// here, the observed lane is skipped rather than widened to the whole corpus.
func TestAssembleRetrieval_InsightOnlyPlanStillSkipsObservedLane(t *testing.T) {
	t.Parallel()

	searcher := &recordingSearcher{}
	plan := intent.QueryPlan{SourceTypes: []model.SourceType{model.SourceInsight}, Limit: 8, Reason: "추론", Origin: intent.OriginLLM}

	if _, err := assembleRetrieval(context.Background(), searcher, intent.Params{RawQuery: "질문"}, plan, 8, 3); err != nil {
		t.Fatalf("assembleRetrieval() error = %v", err)
	}
	if len(searcher.calls) != 1 {
		t.Fatalf("got %d searches, want 1 (insight lane only)", len(searcher.calls))
	}
	if searcher.calls[0].SourceType == nil || *searcher.calls[0].SourceType != model.SourceInsight {
		t.Errorf("the single search was not the insight lane: %+v", searcher.calls[0])
	}
}

func sameSourceTypeSet(got, want []model.SourceType) bool {
	if len(got) != len(want) {
		return false
	}
	counts := make(map[model.SourceType]int, len(got))
	for _, st := range got {
		counts[st]++
	}
	for _, st := range want {
		counts[st]--
	}
	for _, n := range counts {
		if n != 0 {
			return false
		}
	}
	return true
}
