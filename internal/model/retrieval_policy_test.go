package model

import "testing"

func TestKakaoRetrievalRequiresExplicitSource(t *testing.T) {
	kakao := SourceKakao
	for _, tc := range []struct {
		name     string
		q        SearchQuery
		excluded bool
	}{
		{"default", SearchQuery{}, true},
		{"singular", SearchQuery{SourceType: &kakao}, false},
		{"mixed", SearchQuery{SourceTypes: []SourceType{SourceSMS, SourceKakao}}, false},
		{"exclude_wins", SearchQuery{SourceType: &kakao, ExcludeSourceTypes: []SourceType{SourceKakao}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := tc.q.WithRetrievalDefaults().WithRetrievalDefaults()
			count := 0
			for _, source := range q.ExcludeSourceTypes {
				if source == SourceKakao {
					count++
				}
			}
			if (count == 1) != tc.excluded || count > 1 {
				t.Fatalf("exclusions = %v", q.ExcludeSourceTypes)
			}
		})
	}
	backing := []SourceType{SourceInsight, SourceSMS}
	q := SearchQuery{ExcludeSourceTypes: backing[:1]}
	_ = q.WithRetrievalDefaults()
	if backing[1] != SourceSMS {
		t.Fatal("caller slice mutated")
	}
}
