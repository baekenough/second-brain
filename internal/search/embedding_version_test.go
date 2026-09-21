package search

import "testing"

func TestEmbeddingVersion(t *testing.T) {
	tests := []struct {
		name       string
		model      string
		dimensions int
		recipe     string
		want       string
	}{
		{
			name:       "문서 레시피",
			model:      "text-embedding-3-small",
			dimensions: 1536,
			recipe:     RecipeDocumentV1,
			want:       "text-embedding-3-small:1536:doc-v1",
		},
		{
			name:       "청크 문맥 헤더 레시피",
			model:      "text-embedding-3-small",
			dimensions: 1536,
			recipe:     RecipeChunkContextV1,
			want:       "text-embedding-3-small:1536:chunk-ctx-v1",
		},
		{
			name:       "모델이 바뀌면 버전도 달라진다",
			model:      "text-embedding-3-large",
			dimensions: 1536,
			recipe:     RecipeChunkContextV1,
			want:       "text-embedding-3-large:1536:chunk-ctx-v1",
		},
		{
			name:       "차원 0 은 모델 기본값 표기",
			model:      "text-embedding-3-small",
			dimensions: 0,
			recipe:     RecipeDocumentV1,
			want:       "text-embedding-3-small:default:doc-v1",
		},
		{
			name:       "모델이 비면 버전을 만들지 않는다",
			model:      "",
			dimensions: 1536,
			recipe:     RecipeDocumentV1,
			want:       "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := EmbeddingVersion(tc.model, tc.dimensions, tc.recipe); got != tc.want {
				t.Errorf("버전 불일치: got=%q want=%q", got, tc.want)
			}
		})
	}
}

// 같은 모델·차원이어도 레시피가 다르면 버전이 달라야 한다. 이 성질이 깨지면
// 임베딩 입력 구성을 바꿔도 재임베딩 대상이 잡히지 않는다.
func TestEmbeddingVersion_레시피가_다르면_버전도_다르다(t *testing.T) {
	doc := EmbeddingVersion("text-embedding-3-small", 1536, RecipeDocumentV1)
	chunk := EmbeddingVersion("text-embedding-3-small", 1536, RecipeChunkContextV1)
	if doc == chunk {
		t.Fatalf("문서/청크 버전이 같다: %q", doc)
	}
}
