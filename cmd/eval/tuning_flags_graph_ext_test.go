package main

import (
	"math"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
)

// --graph-hub-damping / --graph-expand-boost / --rrf-missing-rank 의 검증·프로필 규칙.

func TestValidateTuningFlags_GraphExtensions(t *testing.T) {
	t.Parallel()
	graph := func(t *model.SearchTuning) {
		t.EntityKeywordMode = model.EntityKeywordsSparse
		t.GraphWeight = 0.5
	}
	tests := []struct {
		name    string
		mutate  func(*model.SearchTuning)
		wantErr bool
	}{
		{"허브 감쇠 + 그래프 레인", func(t *model.SearchTuning) { graph(t); t.GraphHubDamping = true }, false},
		{"허브 감쇠인데 그래프 레인 없음", func(t *model.SearchTuning) { t.GraphHubDamping = true }, true},
		{"허브 감쇠 + 키워드만(가중치 0)", func(t *model.SearchTuning) {
			t.EntityKeywordMode = model.EntityKeywordsSparse
			t.GraphHubDamping = true
		}, true},
		{"확장 부스트 0.3", func(t *model.SearchTuning) { t.GraphExpandBoost = 0.3 }, false},
		{"확장 부스트 1", func(t *model.SearchTuning) { t.GraphExpandBoost = 1 }, false},
		{"확장 부스트 1 초과", func(t *model.SearchTuning) { t.GraphExpandBoost = 1.2 }, true},
		{"확장 부스트 음수", func(t *model.SearchTuning) { t.GraphExpandBoost = -0.1 }, true},
		{"확장 부스트 NaN", func(t *model.SearchTuning) { t.GraphExpandBoost = math.NaN() }, true},
		{"rrf cutoff", func(t *model.SearchTuning) { t.RRFMissingRank = model.RRFMissingRankCutoff }, false},
		{"rrf 오타", func(t *model.SearchTuning) { t.RRFMissingRank = "cut-off" }, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tuning := defaultTuningFlags()
			tc.mutate(&tuning)
			err := validateTuningFlags(tuning)
			if tc.wantErr && err == nil {
				t.Errorf("효과 없는/잘못된 조합이 통과했다: %+v", tuning)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("유효한 조합이 거부됐다: %v", err)
			}
		})
	}
}

func TestApplyTuningProfile_GraphExtensions(t *testing.T) {
	t.Parallel()
	profileOf := func(mutate func(*model.SearchTuning)) map[string]any {
		tuning := defaultTuningFlags()
		mutate(&tuning)
		profile := map[string]any{}
		applyTuningProfile(profile, tuning.Normalized())
		return profile
	}

	damped := profileOf(func(t *model.SearchTuning) {
		t.EntityKeywordMode = model.EntityKeywordsSparse
		t.GraphWeight = 0.5
		t.GraphHubDamping = true
	})
	if damped["graph_hub_damping"] != true || damped["graph_weight"] != 0.5 {
		t.Errorf("허브 감쇠 프로필: %+v", damped)
	}
	undamped := profileOf(func(t *model.SearchTuning) {
		t.EntityKeywordMode = model.EntityKeywordsSparse
		t.GraphWeight = 0.5
	})
	if _, ok := undamped["graph_hub_damping"]; ok || digest(undamped) == digest(damped) {
		t.Error("감쇠 on/off 가 같은 baseline 계열이다")
	}

	if p := profileOf(func(t *model.SearchTuning) { t.GraphExpandBoost = 0.4 }); len(p) != 1 || p["graph_expand_boost"] != 0.4 {
		t.Errorf("확장 부스트 프로필: %+v", p)
	}
	if p := profileOf(func(t *model.SearchTuning) { t.RRFMissingRank = model.RRFMissingRankCutoff }); len(p) != 1 || p["rrf_missing_rank"] != model.RRFMissingRankCutoff {
		t.Errorf("rrf cutoff 프로필: %+v", p)
	}

	// 효과 없는 값은 프로필에 들어가지 않는다.
	inert := profileOf(func(t *model.SearchTuning) {
		t.GraphHubDamping = true // 그래프 레인 없음
		t.RRFMissingRank = model.RRFMissingRankZero
	})
	if len(inert) != 0 {
		t.Errorf("효과 없는 값이 프로필에 들어갔다: %+v", inert)
	}
}
