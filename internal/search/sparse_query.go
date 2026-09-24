package search

import (
	"context"
	"log/slog"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/sparseq"
)

// sparseTermsFor 는 SEARCH_SPARSE_QUERY 노브가 켜졌을 때 질문에서 희소 레인
// 키워드를 뽑는다(#276). raw(기본)이거나 살아남은 키워드가 없으면 빈 값을
// 돌려주고, 그러면 저장소는 질문 원문 경로(#276 이전 SQL)를 그대로 탄다.
//
// 키워드는 질문에서 파생된 개인 데이터다. 로그에는 개수만 남기고 키워드
// 자체는 절대 싣지 않는다 — QueryPlan.Reason 과 같은 규칙이다.
func sparseTermsFor(ctx context.Context, query string, tune model.SearchTuning) model.SparseTerms {
	if tune.SparseQuery != model.SparseQueryChunk && tune.SparseQuery != model.SparseQueryChunkDoc {
		return model.SparseTerms{}
	}
	terms := sparseq.Extract(query)
	st := model.SparseTerms{TSQuery: sparseq.TSQuery(terms.TS), Like: terms.Like}
	slog.DebugContext(ctx, "search: sparse query terms extracted",
		"mode", tune.SparseQuery,
		"terms_version", sparseq.Version,
		"ts_terms", len(terms.TS),
		"like_terms", len(terms.Like),
		"raw_fallback", !st.Active())
	return st
}
