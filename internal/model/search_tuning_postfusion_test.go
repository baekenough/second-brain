package model

import (
	"math"
	"testing"
)

func TestSearchTuningNormalized_PostFusionDefaultsOff(t *testing.T) {
	n := SearchTuning{}.Normalized()
	if n.SourceStratifyK != 0 || n.CollapseContactDay || n.CollapseExpandMax != 0 ||
		n.WindowBucketDiversify || n.MMRLambda != 0 {
		t.Fatalf("zero tuning enabled a post-fusion knob: %+v", n)
	}
}

func TestSearchTuningNormalized_PostFusionRanges(t *testing.T) {
	tests := []struct {
		name string
		in   SearchTuning
		want func(SearchTuning) bool
	}{
		{"stratify negative", SearchTuning{SourceStratifyK: -3}, func(n SearchTuning) bool { return n.SourceStratifyK == 0 }},
		{"stratify capped", SearchTuning{SourceStratifyK: 99}, func(n SearchTuning) bool { return n.SourceStratifyK == MaxSourceStratifyK }},
		{"expand max without collapse", SearchTuning{CollapseExpandMax: 5}, func(n SearchTuning) bool { return n.CollapseExpandMax == 0 }},
		{"expand max default", SearchTuning{CollapseContactDay: true}, func(n SearchTuning) bool { return n.CollapseExpandMax == DefaultCollapseExpandMax }},
		{"expand max capped", SearchTuning{CollapseContactDay: true, CollapseExpandMax: 99}, func(n SearchTuning) bool { return n.CollapseExpandMax == MaxCollapseExpandMax }},
		{"mmr > 1", SearchTuning{MMRLambda: 1.5}, func(n SearchTuning) bool { return n.MMRLambda == 0 }},
		{"mmr NaN", SearchTuning{MMRLambda: math.NaN()}, func(n SearchTuning) bool { return n.MMRLambda == 0 }},
		{"mmr valid", SearchTuning{MMRLambda: 0.4}, func(n SearchTuning) bool { return n.MMRLambda == 0.4 }},
		{"yaml input kept", SearchTuning{RerankInput: RerankInputYAML}, func(n SearchTuning) bool { return n.RerankInput == RerankInputYAML }},
		{"yaml drops call context", SearchTuning{RerankInput: RerankInputYAML, RerankCallContext: true}, func(n SearchTuning) bool { return !n.RerankCallContext }},
		{"best_chunk keeps call context", SearchTuning{RerankInput: RerankInputBestChunk, RerankCallContext: true}, func(n SearchTuning) bool { return n.RerankCallContext }},
	}
	for _, tc := range tests {
		if n := tc.in.Normalized(); !tc.want(n) {
			t.Errorf("%s: %+v", tc.name, n)
		}
	}
}

func TestEnvSearchTuning_PostFusion(t *testing.T) {
	t.Setenv("SEARCH_SOURCE_STRATIFY_K", "4")
	t.Setenv("SEARCH_COLLAPSE_CONTACT_DAY", "true")
	t.Setenv("SEARCH_COLLAPSE_EXPAND_MAX", "2")
	t.Setenv("SEARCH_WINDOW_BUCKET_DIVERSIFY", "true")
	t.Setenv("SEARCH_MMR_LAMBDA", "0.6")
	t.Setenv("SEARCH_RERANK_INPUT", "yaml")
	got := EnvSearchTuning()
	if got.SourceStratifyK != 4 || !got.CollapseContactDay || got.CollapseExpandMax != 2 ||
		!got.WindowBucketDiversify || got.MMRLambda != 0.6 || got.RerankInput != RerankInputYAML {
		t.Fatalf("env not applied: %+v", got)
	}

	t.Setenv("SEARCH_SOURCE_STRATIFY_K", "x")
	t.Setenv("SEARCH_COLLAPSE_CONTACT_DAY", "")
	t.Setenv("SEARCH_COLLAPSE_EXPAND_MAX", "7")
	t.Setenv("SEARCH_WINDOW_BUCKET_DIVERSIFY", "maybe")
	t.Setenv("SEARCH_MMR_LAMBDA", "3")
	t.Setenv("SEARCH_RERANK_INPUT", "")
	got = EnvSearchTuning()
	if got.SourceStratifyK != 0 || got.CollapseContactDay || got.CollapseExpandMax != 0 ||
		got.WindowBucketDiversify || got.MMRLambda != 0 || got.RerankInput != RerankInputHead {
		t.Fatalf("invalid env not ignored: %+v", got)
	}
}

func TestSearchTuning_PlanAwareKnobs(t *testing.T) {
	if n := (SearchTuning{}).Normalized(); n.ScheduleIntentBoost != 0 || n.PlanSourceSpillK != 0 {
		t.Fatalf("zero tuning enabled a plan-aware knob: %+v", n)
	}
	for in, want := range map[float64]float64{-1: 0, 0.4: 0.4, 1: 1, 1.01: 0, math.NaN(): 0} {
		if got := (SearchTuning{ScheduleIntentBoost: in}).Normalized().ScheduleIntentBoost; got != want {
			t.Errorf("boost %v → %v, want %v", in, got, want)
		}
	}
	for in, want := range map[int]int{-1: 0, 3: 3, 99: MaxPlanSourceSpillK} {
		if got := (SearchTuning{PlanSourceSpillK: in}).Normalized().PlanSourceSpillK; got != want {
			t.Errorf("spill %d → %d, want %d", in, got, want)
		}
	}
	t.Setenv("SEARCH_SCHEDULE_INTENT_BOOST", "0.3")
	t.Setenv("SEARCH_PLAN_SOURCE_SPILL_K", "5")
	if got := EnvSearchTuning(); got.ScheduleIntentBoost != 0.3 || got.PlanSourceSpillK != 5 {
		t.Fatalf("env not applied: %+v", got)
	}
	t.Setenv("SEARCH_SCHEDULE_INTENT_BOOST", "2")
	t.Setenv("SEARCH_PLAN_SOURCE_SPILL_K", "-3")
	if got := EnvSearchTuning(); got.ScheduleIntentBoost != 0 || got.PlanSourceSpillK != 0 {
		t.Fatalf("invalid env not ignored: %+v", got)
	}
}
