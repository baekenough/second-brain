package scheduler

import (
	"github.com/baekenough/second-brain/internal/chunkctx"
	"github.com/baekenough/second-brain/internal/model"
)

// withChunkContextHeader 는 청크 임베딩에 실제로 보낼 텍스트를 만든다.
// 헤더가 비어 있으면 청크 본문을 그대로 돌려주므로, 붙일 문맥이 없을 때
// 앞머리에 빈 줄만 생기는 일이 없다.
//
// 헤더 구성 자체(BuildChunkContextHeader)는 internal/chunkctx 패키지로
// 옮겨졌다(#270 P0) — internal/search 와 internal/store 도 같은 헤더 로직이
// 필요해졌고, search/store -> scheduler 방향의 의존은 성립하지 않기
// 때문이다. 이 함수는 그 결과를 청크 본문 앞에 붙이는 조립만 한다.
//
// 반환값은 임베딩 입력 전용이다 — 저장되는 chunk.Content 에는 절대 쓰지 말 것.
func withChunkContextHeader(doc model.Document, chunkContent string) string {
	header := chunkctx.BuildChunkContextHeader(doc)
	if header == "" {
		return chunkContent
	}
	return header + "\n\n" + chunkContent
}
