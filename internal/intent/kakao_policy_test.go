package intent

import "testing"

// 자연어 계획은 명시 source_type 요청과 다르다. 아직 카카오 RAG로 확장하지 않는다.
func TestPlannerCannotOptIntoKakao(t *testing.T) {
	if _, ok := parseSourceTypes([]string{"kakao"}); ok {
		t.Fatal("LLM planner can bypass Kakao retrieval isolation")
	}
}
