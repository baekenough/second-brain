package chunkctx

import (
	"strings"

	"github.com/baekenough/second-brain/internal/model"
)

// Recipe 이름. model 패키지의 model.ChunkSparseCtxV1TP/V1Full 과 값이
// 같다 — model.SearchTuning.ChunkSparseCtxVersion 이 그대로 여기 Recipe
// 인자가 된다. 상수를 model 에 둔 이유는 model.ChunkSparseCtxV1TP 의 doc
// comment 참고(이 패키지가 이미 model 을 임포트해서 반대 방향은 순환이 된다).
const (
	RecipeTP   = model.ChunkSparseCtxV1TP
	RecipeFull = model.ChunkSparseCtxV1Full
)

// BuildSparseText 는 #270 phase B 의 청크 희소(FTS/bigm) 매칭 대상 텍스트를
// 만든다. recipe 에 따라 헤더 구성만 달라지고, 헤더 뒤에는 항상 청크 본문을
// "\n\n" 으로 이어 붙인다 — plainto_tsquery 는 토큰을 AND 로 묶으므로,
// 이름이 헤더에·본문 단어가 청크에 있는 질의가 한 tsvector 에서 만나야
// 하기 때문이다(마이그레이션 040 의 sparse_tsv 컬럼 주석 참고).
//
//   - RecipeTP: 제목+참여자만(날짜·소스 라벨 없음). 소스 라벨("메일",
//     "통화")이 그 단어를 포함한 질의에서 오탐을 키울 수 있어 분리한다.
//   - RecipeFull: BuildChunkContextHeader 와 완전히 같다(임베딩 헤더 재사용).
//
// 두 번째 반환값이 false 면 알 수 없는 recipe 다 — 호출자(백필 워커, 조회
// 레인)는 그 행을 건너뛰어야 한다.
//
// 반환값은 매칭 전용이다 — SearchResult.Content 나 인용·support_spans·
// synthesis 프롬프트로는 절대 흘리지 않는다(호출자 책임 — 이 함수는 저장되는
// 값이 어디로 가는지 모른다).
func BuildSparseText(recipe string, doc model.Document, chunkContent string) (string, bool) {
	var header string
	switch recipe {
	case RecipeTP:
		header = buildParticipantsOnlyHeader(doc)
	case RecipeFull:
		header = BuildChunkContextHeader(doc)
	default:
		return "", false
	}
	if header == "" {
		return chunkContent, true
	}
	return header + "\n\n" + chunkContent, true
}

// buildParticipantsOnlyHeader 는 RecipeTP 의 헤더다: 제목과 참여자만,
// 날짜·소스 라벨은 뺀다. BuildChunkContextHeader 와 같은 sanitizer
// (chunkHeaderParticipants/sanitizePerson/looksLikePhoneNumber)를 그대로
// 쓰므로 전화번호·마스킹 토큰 배제 규칙은 두 recipe 가 동일하다.
func buildParticipantsOnlyHeader(doc model.Document) string {
	var parts []string
	if title := truncateRunes(collapseSpaces(doc.Title), headerTitleMaxRunes); title != "" {
		parts = append(parts, "제목: "+title)
	}
	parts = append(parts, chunkHeaderParticipants(doc)...)
	if len(parts) == 0 {
		return ""
	}
	return truncateRunes(strings.Join(parts, " · "), chunkHeaderMaxRunes)
}
