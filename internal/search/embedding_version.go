package search

import "fmt"

// 임베딩 "레시피" 식별자. 모델·차원이 같아도 임베딩에 넣는 텍스트를 어떻게
// 구성했는지가 다르면 벡터 공간이 달라지므로, 레시피도 버전 문자열에 포함한다.
//
// 값을 바꾸면 해당 레시피로 만들어진 모든 행이 "구버전"이 되어 재임베딩
// 대상으로 잡힌다. 그러니 임베딩 입력 구성을 실제로 바꿨을 때만 올릴 것.
const (
	// RecipeDocumentV1 은 문서 임베딩 = title + "\n\n" + content.
	RecipeDocumentV1 = "doc-v1"
	// RecipeChunkContextV1 은 청크 임베딩 = 문맥 헤더 + "\n\n" + 청크 본문.
	// 헤더 구성은 scheduler.BuildChunkContextHeader 가 정의한다.
	// (그 이전의 "청크 본문만" 방식에는 버전이 없었다 — 컬럼 NULL 이 곧
	//  레거시 표시다.)
	RecipeChunkContextV1 = "chunk-ctx-v1"
)

// EmbeddingVersion 은 벡터가 어떤 설정으로 만들어졌는지 식별하는 문자열을
// 만든다. 형식: "{model}:{dimensions}:{recipe}"
//
//	예) "text-embedding-3-small:1536:chunk-ctx-v1"
//
// 이 값을 documents.embedding_version / chunks.embedding_version 에 기록해
// 두면, 모델이나 레시피를 바꿨을 때 "아직 옛 설정으로 남아 있는 행"을 SQL
// 한 줄(IS DISTINCT FROM)로 찾아 재임베딩할 수 있다. 서로 다른 설정으로 만든
// 벡터가 같은 인덱스에 섞이면 거리 비교가 의미를 잃는데, 그 상태를 눈으로
// 확인할 수단이 지금까지 없었다.
//
// model 이 비어 있으면 빈 문자열을 돌려준다 — 임베딩이 꺼진 배포에서
// 의미 없는 버전 문자열을 쓰지 않기 위함이다. dimensions 가 0 이면 모델
// 기본 차원을 뜻하는 "default" 로 표기한다.
func EmbeddingVersion(model string, dimensions int, recipe string) string {
	if model == "" {
		return ""
	}
	dim := "default"
	if dimensions > 0 {
		dim = fmt.Sprintf("%d", dimensions)
	}
	return fmt.Sprintf("%s:%s:%s", model, dim, recipe)
}
