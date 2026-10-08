package main

import (
	"math"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
)

func TestValidatePostFusionFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*model.SearchTuning)
		wantErr bool
	}{
		{"기본 실행", func(*model.SearchTuning) {}, false},
		{"모든 노브", func(t *model.SearchTuning) {
			t.SourceStratifyK = 5
			t.CollapseContactDay = true
			t.CollapseExpandMax = 2
			t.WindowBucketDiversify = true
			t.MMRLambda = 0.7
			t.RerankInput = model.RerankInputYAML
		}, false},
		{"--source-stratify-k 음수", func(t *model.SearchTuning) { t.SourceStratifyK = -1 }, true},
		{"--source-stratify-k 상한 초과", func(t *model.SearchTuning) { t.SourceStratifyK = model.MaxSourceStratifyK + 1 }, true},
		{"--collapse-expand-max 단독", func(t *model.SearchTuning) { t.CollapseExpandMax = 5 }, true},
		{"--collapse-expand-max 0", func(t *model.SearchTuning) {
			t.CollapseContactDay = true
			t.CollapseExpandMax = 0
		}, true},
		{"--collapse-expand-max 상한 초과", func(t *model.SearchTuning) {
			t.CollapseContactDay = true
			t.CollapseExpandMax = model.MaxCollapseExpandMax + 1
		}, true},
		{"--mmr-lambda 1", func(t *model.SearchTuning) { t.MMRLambda = 1 }, false},
		{"--mmr-lambda 범위 밖", func(t *model.SearchTuning) { t.MMRLambda = 1.5 }, true},
		{"--mmr-lambda 음수", func(t *model.SearchTuning) { t.MMRLambda = -0.1 }, true},
		{"--mmr-lambda NaN", func(t *model.SearchTuning) { t.MMRLambda = math.NaN() }, true},
		{"--rerank-input 오타", func(t *model.SearchTuning) { t.RerankInput = "yml" }, true},
		{"--rerank-input=yaml + --rerank-call-context", func(t *model.SearchTuning) {
			t.RerankInput = model.RerankInputYAML
			t.RerankCallContext = true
		}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tuning := defaultTuningFlags()
			tc.mutate(&tuning)
			err := validateTuningFlags(tuning)
			if tc.wantErr != (err != nil) {
				t.Errorf("wantErr=%v, err=%v (%+v)", tc.wantErr, err, tuning)
			}
		})
	}
}

func TestValidateRerankTuning(t *testing.T) {
	t.Parallel()
	yaml := defaultTuningFlags()
	yaml.RerankInput = model.RerankInputYAML
	if err := validateRerankTuning(yaml, false); err == nil {
		t.Error("--rerank-input=yaml without rerank was accepted")
	}
	if err := validateRerankTuning(yaml, true); err != nil {
		t.Errorf("--rerank-input=yaml with rerank rejected: %v", err)
	}
	// 기존 값은 이 검사 이전 동작을 유지한다.
	best := defaultTuningFlags()
	best.RerankInput = model.RerankInputBestChunk
	if err := validateRerankTuning(best, false); err != nil {
		t.Errorf("best_chunk without rerank must stay accepted: %v", err)
	}
	if err := validateRerankTuning(defaultTuningFlags(), false); err != nil {
		t.Errorf("default rejected: %v", err)
	}
}

func TestApplyTuningProfile_PostFusionKeys(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		mutate   func(*model.SearchTuning)
		wantKeys map[string]any
	}{
		{"보조 검색", func(t *model.SearchTuning) { t.SourceStratifyK = 5 },
			map[string]any{"source_stratify_k": 5}},
		{"접기는 펼침 수까지", func(t *model.SearchTuning) { t.CollapseContactDay = true },
			map[string]any{"collapse_contact_day": true, "collapse_expand_max": model.DefaultCollapseExpandMax}},
		{"날짜 버킷", func(t *model.SearchTuning) { t.WindowBucketDiversify = true },
			map[string]any{"window_bucket_diversify": true}},
		{"MMR 은 창 크기까지", func(t *model.SearchTuning) { t.MMRLambda = 0.5 },
			map[string]any{"mmr_lambda": 0.5, "mmr_window": search.MMRWindow, "mmr_rank_k": search.MMRRankK}},
		{"YAML 입력은 형식 판까지", func(t *model.SearchTuning) { t.RerankInput = model.RerankInputYAML },
			map[string]any{"rerank_input": model.RerankInputYAML, "rerank_yaml_version": search.RerankYAMLVersion}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tuning := defaultTuningFlags()
			tc.mutate(&tuning)
			profile := map[string]any{"limit": 10}
			applyTuningProfile(profile, tuning.Normalized())
			if len(profile) != 1+len(tc.wantKeys) {
				t.Fatalf("unexpected keys: %+v", profile)
			}
			for k, v := range tc.wantKeys {
				if profile[k] != v {
					t.Errorf("profile[%q] = %v, want %v", k, profile[k], v)
				}
			}
		})
	}
}
