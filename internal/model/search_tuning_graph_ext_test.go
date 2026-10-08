package model

import (
	"math"
	"testing"
)

// GraphHubDamping / GraphExpandBoost / RRFMissingRank 노브의 정규화·환경변수 규칙.

func TestSearchTuning_GraphHubDampingNormalized(t *testing.T) {
	t.Parallel()
	if (SearchTuning{GraphHubDamping: true}).Normalized().GraphHubDamping {
		t.Error("그래프 레인이 없는데(GraphWeight=0) 허브 감쇠가 켜진 채 남았다")
	}
	if (SearchTuning{GraphHubDamping: true, GraphWeight: math.NaN()}).Normalized().GraphHubDamping {
		t.Error("NaN 가중치(=레인 없음)에서 허브 감쇠가 남았다")
	}
	if !(SearchTuning{GraphHubDamping: true, GraphWeight: 0.5}).Normalized().GraphHubDamping {
		t.Error("그래프 레인이 있는데 허브 감쇠가 꺼졌다")
	}
}

func TestSearchTuning_GraphExpandBoostNormalized(t *testing.T) {
	t.Parallel()
	for in, want := range map[float64]float64{
		0: 0, 0.3: 0.3, 1: 1, 1.5: 1, -0.2: 0, math.NaN(): 0, math.Inf(1): 0,
	} {
		if got := (SearchTuning{GraphExpandBoost: in}).Normalized().GraphExpandBoost; got != want {
			t.Errorf("Normalized(GraphExpandBoost=%v) = %v, want %v", in, got, want)
		}
	}
	if (SearchTuning{GraphExpandBoost: 0.1}).IsZero() {
		t.Fatal("GraphExpandBoost 가 켜진 튜닝은 IsZero 가 아니다")
	}
}

func TestSearchTuning_RRFMissingRankNormalized(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"":       RRFMissingRankZero,
		"cutoff": RRFMissingRankCutoff,
		"Cutoff": RRFMissingRankZero, // 구조체 값은 정확해야 한다; 소문자화는 환경변수 경로만
		"zero":   RRFMissingRankZero,
	} {
		if got := (SearchTuning{RRFMissingRank: in}).Normalized().RRFMissingRank; got != want {
			t.Errorf("Normalized(RRFMissingRank=%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnvSearchTuning_GraphExtensions(t *testing.T) {
	for _, k := range []string{
		"SEARCH_ENTITY_KEYWORDS", "SEARCH_GRAPH_WEIGHT", "SEARCH_GRAPH_HUB_DAMPING",
		"SEARCH_GRAPH_EXPAND_BOOST", "SEARCH_RRF_MISSING_RANK",
	} {
		t.Setenv(k, "")
	}
	if got, want := EnvSearchTuning(), (SearchTuning{}).Normalized(); got != want {
		t.Errorf("환경변수가 없으면 현행 동작이어야 한다: %+v vs %+v", got, want)
	}

	t.Setenv("SEARCH_GRAPH_HUB_DAMPING", "true")
	if EnvSearchTuning().GraphHubDamping {
		t.Error("SEARCH_GRAPH_WEIGHT 없이 허브 감쇠가 켜졌다")
	}
	t.Setenv("SEARCH_ENTITY_KEYWORDS", "sparse")
	t.Setenv("SEARCH_GRAPH_WEIGHT", "0.5")
	if !EnvSearchTuning().GraphHubDamping {
		t.Error("SEARCH_GRAPH_HUB_DAMPING=true + SEARCH_GRAPH_WEIGHT=0.5 에서 꺼졌다")
	}

	for env, want := range map[string]float64{"0.4": 0.4, "2": 1, "abc": 0, "-1": 0} {
		t.Setenv("SEARCH_GRAPH_EXPAND_BOOST", env)
		if got := EnvSearchTuning().GraphExpandBoost; got != want {
			t.Errorf("SEARCH_GRAPH_EXPAND_BOOST=%q → %v, want %v", env, got, want)
		}
	}

	for env, want := range map[string]string{" CUTOFF ": RRFMissingRankCutoff, "cutoff": RRFMissingRankCutoff, "max": RRFMissingRankZero} {
		t.Setenv("SEARCH_RRF_MISSING_RANK", env)
		if got := EnvSearchTuning().RRFMissingRank; got != want {
			t.Errorf("SEARCH_RRF_MISSING_RANK=%q → %q, want %q", env, got, want)
		}
	}
}
