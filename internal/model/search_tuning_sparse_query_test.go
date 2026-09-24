package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// SEARCH_SPARSE_QUERY(#276) 노브의 정규화·환경변수 규칙.

func TestSearchTuning_SparseQueryNormalized(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"":                  SparseQueryRaw,
		SparseQueryRaw:      SparseQueryRaw,
		SparseQueryChunk:    SparseQueryChunk,
		SparseQueryChunkDoc: SparseQueryChunkDoc,
		"CHUNK":             SparseQueryRaw, // 환경변수 경로만 소문자화한다; 구조체 값은 정확해야 한다
		"chunk-doc":         SparseQueryRaw,
	}
	for in, want := range tests {
		if got := (SearchTuning{SparseQuery: in}).Normalized().SparseQuery; got != want {
			t.Errorf("Normalized(SparseQuery=%q) = %q, want %q", in, got, want)
		}
	}
	// 제로값은 여전히 "현행 동작" 이다 — 새 필드가 IsZero 판정을 흔들면
	// 요청 튜닝이 비어 있을 때 서비스 기본값으로 가는 규약이 깨진다.
	if !(SearchTuning{}).IsZero() {
		t.Fatal("zero SearchTuning must report IsZero")
	}
}

func TestEnvSearchTuning_SparseQuery(t *testing.T) {
	tests := map[string]string{
		"":          SparseQueryRaw,
		"raw":       SparseQueryRaw,
		"chunk":     SparseQueryChunk,
		" Chunk ":   SparseQueryChunk,
		"chunk_doc": SparseQueryChunkDoc,
		"chunkdoc":  SparseQueryRaw, // 알 수 없는 값은 경고 후 기본값
	}
	for env, want := range tests {
		t.Setenv("SEARCH_SPARSE_QUERY", env)
		if got := EnvSearchTuning().SparseQuery; got != want {
			t.Errorf("SEARCH_SPARSE_QUERY=%q → %q, want %q", env, got, want)
		}
	}
}

// TestSearchQuery_SparseTermsNotJSON 은 SparseTerms 가 JSON 으로 들어오지도
// 나가지도 않는지 고정한다. REST·MCP 클라이언트가 임의의 tsquery 를 밀어
// 넣을 수 있으면 안 되고, 질문에서 파생된 키워드가 응답에 실려도 안 된다.
func TestSearchQuery_SparseTermsNotJSON(t *testing.T) {
	t.Parallel()
	var q SearchQuery
	if err := json.Unmarshal([]byte(`{"Query":"x","SparseTerms":{"TSQuery":"'a':*","Like":["a"]}}`), &q); err != nil {
		t.Fatal(err)
	}
	if q.SparseTerms.Active() {
		t.Fatalf("SparseTerms decoded from JSON: %+v", q.SparseTerms)
	}
	b, err := json.Marshal(SearchQuery{Query: "x", SparseTerms: SparseTerms{TSQuery: "'zzsecret':*", Like: []string{"zzsecret"}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "zzsecret") {
		t.Fatalf("SparseTerms leaked into JSON: %s", b)
	}
}
