package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// EntityKeywordMode / HighLevelKeywordsToSparse / GraphWeight 노브의 정규화·
// 환경변수 규칙.

func TestSearchTuning_EntityKeywordModeNormalized(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"":                   EntityKeywordsOff,
		EntityKeywordsSparse: EntityKeywordsSparse,
		EntityKeywordsLLM:    EntityKeywordsLLM,
		"LLM":                EntityKeywordsOff, // 환경변수 경로만 소문자화한다; 구조체 값은 정확해야 한다
		"off":                EntityKeywordsOff,
		"dual":               EntityKeywordsOff,
	}
	for in, want := range tests {
		if got := (SearchTuning{EntityKeywordMode: in}).Normalized().EntityKeywordMode; got != want {
			t.Errorf("Normalized(EntityKeywordMode=%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSearchTuning_HighLevelKeywordsToSparseNormalized(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   SearchTuning
		want bool
	}{
		{"llm + chunk_doc", SearchTuning{EntityKeywordMode: EntityKeywordsLLM, SparseQuery: SparseQueryChunkDoc, HighLevelKeywordsToSparse: true}, true},
		{"llm + chunk", SearchTuning{EntityKeywordMode: EntityKeywordsLLM, SparseQuery: SparseQueryChunk, HighLevelKeywordsToSparse: true}, true},
		{"llm 이지만 희소 질의 raw", SearchTuning{EntityKeywordMode: EntityKeywordsLLM, HighLevelKeywordsToSparse: true}, false},
		{"sparse 모드는 고수준 키워드가 없다", SearchTuning{EntityKeywordMode: EntityKeywordsSparse, SparseQuery: SparseQueryChunkDoc, HighLevelKeywordsToSparse: true}, false},
		{"모드 off", SearchTuning{SparseQuery: SparseQueryChunkDoc, HighLevelKeywordsToSparse: true}, false},
	}
	for _, tc := range tests {
		if got := tc.in.Normalized().HighLevelKeywordsToSparse; got != tc.want {
			t.Errorf("%s: HighLevelKeywordsToSparse = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSearchTuning_GraphWeightNormalized(t *testing.T) {
	t.Parallel()
	nan := 0.0
	nan = nan / nan
	for in, want := range map[float64]float64{0: 0, 0.7: 0.7, -1: 0, nan: 0} {
		if got := (SearchTuning{GraphWeight: in}).Normalized().GraphWeight; got != want {
			t.Errorf("Normalized(GraphWeight=%v) = %v, want %v", in, got, want)
		}
	}
	// 새 필드가 IsZero 판정을 흔들면 요청 튜닝이 비어 있을 때 서비스 기본값으로
	// 가는 규약이 깨진다.
	if !(SearchTuning{}).IsZero() {
		t.Fatal("zero SearchTuning must report IsZero")
	}
	if (SearchTuning{GraphWeight: 0.1}).IsZero() {
		t.Fatal("GraphWeight 가 켜진 튜닝은 IsZero 가 아니다")
	}
}

func TestEnvSearchTuning_EntityKeywords(t *testing.T) {
	// 모든 노브를 끈 환경은 제로값에 Normalized 를 거친 것과 같아야 한다.
	t.Setenv("SEARCH_ENTITY_KEYWORDS", "")
	t.Setenv("SEARCH_HIGH_LEVEL_KEYWORDS_TO_SPARSE", "")
	t.Setenv("SEARCH_GRAPH_WEIGHT", "")
	if got, want := EnvSearchTuning(), (SearchTuning{}).Normalized(); got != want {
		t.Errorf("환경변수가 없으면 현행 동작이어야 한다: %+v vs %+v", got, want)
	}

	modes := map[string]string{
		"":       EntityKeywordsOff,
		"sparse": EntityKeywordsSparse,
		" LLM ":  EntityKeywordsLLM,
		"llm":    EntityKeywordsLLM,
		"dual":   EntityKeywordsOff, // 알 수 없는 값은 경고 후 기본값
	}
	for env, want := range modes {
		t.Setenv("SEARCH_ENTITY_KEYWORDS", env)
		if got := EnvSearchTuning().EntityKeywordMode; got != want {
			t.Errorf("SEARCH_ENTITY_KEYWORDS=%q → %q, want %q", env, got, want)
		}
	}

	t.Setenv("SEARCH_GRAPH_WEIGHT", "0.8")
	if got := EnvSearchTuning().GraphWeight; got != 0.8 {
		t.Errorf("SEARCH_GRAPH_WEIGHT=0.8 → %v", got)
	}
	for _, bad := range []string{"abc", "-1", "NaN"} {
		t.Setenv("SEARCH_GRAPH_WEIGHT", bad)
		if got := EnvSearchTuning().GraphWeight; got != 0 {
			t.Errorf("SEARCH_GRAPH_WEIGHT=%q → %v, want 0", bad, got)
		}
	}

	// 고수준 키워드 노브는 llm + 희소 질의가 함께 있어야 남는다.
	t.Setenv("SEARCH_HIGH_LEVEL_KEYWORDS_TO_SPARSE", "true")
	t.Setenv("SEARCH_ENTITY_KEYWORDS", "llm")
	t.Setenv("SEARCH_SPARSE_QUERY", "chunk_doc")
	if !EnvSearchTuning().HighLevelKeywordsToSparse {
		t.Error("llm + chunk_doc 에서 고수준 키워드 노브가 꺼졌다")
	}
	t.Setenv("SEARCH_SPARSE_QUERY", "raw")
	if EnvSearchTuning().HighLevelKeywordsToSparse {
		t.Error("효과 없는 조합이 켜진 채 남았다")
	}
}

// TestSearchQuery_EntityKeywordsNotJSON 은 EntityKeywords 가 JSON 으로 들어오지도
// 나가지도 않는지 고정한다. 클라이언트가 임의의 엔티티 매칭 조건을 밀어 넣을 수
// 없어야 하고, 질문에서 파생된 키워드가 응답에 실려서도 안 된다.
func TestSearchQuery_EntityKeywordsNotJSON(t *testing.T) {
	t.Parallel()
	var q SearchQuery
	if err := json.Unmarshal([]byte(`{"Query":"x","EntityKeywords":["injected"]}`), &q); err != nil {
		t.Fatal(err)
	}
	if len(q.EntityKeywords) != 0 {
		t.Fatalf("EntityKeywords decoded from JSON: %q", q.EntityKeywords)
	}
	b, err := json.Marshal(SearchQuery{Query: "x", EntityKeywords: []string{"zzsecret"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "zzsecret") {
		t.Fatalf("EntityKeywords leaked into JSON: %s", b)
	}
}
