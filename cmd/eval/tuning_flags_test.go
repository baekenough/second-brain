package main

import (
	"testing"

	"github.com/baekenough/second-brain/internal/model"
)

// defaultTuningFlags 는 플래그를 하나도 주지 않은 실행의 값이다. flag 패키지의
// 기본값과 같아야 하며, 이 조합은 반드시 검증을 통과하고 프로필에 아무 키도
// 남기지 않아야 한다.
func defaultTuningFlags() model.SearchTuning {
	return model.SearchTuning{
		MergeMode:         model.MergeAsymmetric,
		RerankBlend:       model.RerankBlendReplace,
		RerankBlendWeight: model.DefaultRerankBlendWeight,
		RerankInput:       model.RerankInputHead,
		RecencyAlpha:      model.DefaultRecencyAlpha,
	}
}

func TestValidateTuningFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*model.SearchTuning)
		wantErr bool
	}{
		{"플래그 없는 기본 실행", func(*model.SearchTuning) {}, false},
		{"유효한 실험 조합", func(t *model.SearchTuning) {
			t.RerankOverfetch = 50
			t.MergeMode = model.MergeSymmetric
			t.RerankBlend = model.RerankBlendRRF
			t.RerankInput = model.RerankInputBestChunk
			t.RecencyHalfLifeDays = 30
		}, false},
		{"--merge 오타", func(t *model.SearchTuning) { t.MergeMode = "symetric" }, true},
		{"--rerank-blend 오타", func(t *model.SearchTuning) { t.RerankBlend = "RRF " }, true},
		{"--rerank-input 오타", func(t *model.SearchTuning) { t.RerankInput = "bestchunk" }, true},
		{"--rerank-overfetch 음수", func(t *model.SearchTuning) { t.RerankOverfetch = -1 }, true},
		{"--rerank-blend-weight 0", func(t *model.SearchTuning) { t.RerankBlendWeight = 0 }, true},
		{"--recency-halflife-days 음수", func(t *model.SearchTuning) { t.RecencyHalfLifeDays = -1 }, true},
		{"--recency-alpha 범위 밖", func(t *model.SearchTuning) { t.RecencyAlpha = 1.5 }, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tuning := defaultTuningFlags()
			tc.mutate(&tuning)
			err := validateTuningFlags(tuning)
			if tc.wantErr && err == nil {
				t.Errorf("잘못된 값이 통과했다; 평가가 켜지지도 않은 실험을 켰다고 보고하게 된다: %+v", tuning)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("유효한 조합이 거부됐다: %v", err)
			}
		})
	}
}

// TestApplyTuningProfile_DefaultLeavesHashUntouched 는 노브를 켜지 않은 실행이
// 지금까지 쌓인 baseline 과 계속 비교된다는 것을 고정한다. 기본값에 흔적을
// 남기면 검색 동작이 하나도 바뀌지 않았는데도 전부 해시 불일치가 된다 —
// applyWindowProfile 이 windowModeNone 에서 키를 넣지 않는 것과 같은 이유다.
func TestApplyTuningProfile_DefaultLeavesHashUntouched(t *testing.T) {
	t.Parallel()

	profile := map[string]any{"limit": 10}
	before := digest(profile)
	applyTuningProfile(profile, defaultTuningFlags().Normalized())
	if got := digest(profile); got != before {
		t.Errorf("기본값 실행이 config_hash 를 바꿨다: %d 개 키 추가됨", len(profile)-1)
	}
}

func TestApplyTuningProfile_NonDefaultSplitsBaseline(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mutate   func(*model.SearchTuning)
		wantKeys []string
	}{
		{"오버페치", func(t *model.SearchTuning) { t.RerankOverfetch = 50 }, []string{"rerank_overfetch"}},
		{"대칭 융합", func(t *model.SearchTuning) { t.MergeMode = model.MergeSymmetric }, []string{"merge_mode"}},
		{
			name:     "리랭크 합산은 가중치까지 함께 남긴다",
			mutate:   func(t *model.SearchTuning) { t.RerankBlend = model.RerankBlendRRF },
			wantKeys: []string{"rerank_blend", "rerank_blend_weight"},
		},
		{"리랭커 입력", func(t *model.SearchTuning) { t.RerankInput = model.RerankInputBestChunk }, []string{"rerank_input"}},
		{
			name:     "최신성 감쇠는 alpha 까지 함께 남긴다",
			mutate:   func(t *model.SearchTuning) { t.RecencyHalfLifeDays = 30 },
			wantKeys: []string{"recency_halflife_days", "recency_alpha"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tuning := defaultTuningFlags()
			tc.mutate(&tuning)

			profile := map[string]any{"limit": 10}
			before := digest(profile)
			applyTuningProfile(profile, tuning.Normalized())

			if digest(profile) == before {
				t.Fatalf("노브를 켰는데 config_hash 가 그대로다; 실험 점수가 기본값 baseline 과 비교된다")
			}
			for _, key := range tc.wantKeys {
				if _, ok := profile[key]; !ok {
					t.Errorf("프로필에 %q 키가 없다: %+v", key, profile)
				}
			}
			if len(profile) != 1+len(tc.wantKeys) {
				t.Errorf("기대하지 않은 키가 추가됐다: %+v", profile)
			}
		})
	}
}

// TestApplyTuningProfile_UnusedWeightsStayOut 은 효과가 없는 값이 baseline 을
// 가르지 않는지 본다. blend=replace 실행에 가중치 키가 들어가면 쓰이지도 않는
// 숫자 때문에 비교 대상이 사라진다.
func TestApplyTuningProfile_UnusedWeightsStayOut(t *testing.T) {
	t.Parallel()

	tuning := defaultTuningFlags()
	tuning.RerankBlendWeight = 3.0 // replace 실행이므로 효과 없음
	tuning.RecencyAlpha = 0.9      // 반감기 0 이므로 효과 없음

	profile := map[string]any{}
	applyTuningProfile(profile, tuning.Normalized())
	if len(profile) != 0 {
		t.Errorf("효과 없는 값이 프로필에 들어갔다: %+v", profile)
	}
}
