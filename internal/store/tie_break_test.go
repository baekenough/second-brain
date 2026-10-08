package store

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/sparseq"
)

// 검색 레인의 동점 처리 검사(DB 없음). 레인은 LIMIT 으로 잘리므로 ORDER BY 가 동점을
// 남기면 PostgreSQL 이 임의의 부분집합을 자르고, 병렬 스캔에서는 그 부분집합이 실행마다
// 달라진다. 모든 레인 순서는 (점수, 최신 사건 시각 NULLS LAST, id) 처럼 전순서여야 하고,
// 각 CTE 는 바깥 ORDER BY 를 LIMIT 앞에 명시해야 한다.

var cteLimitRe = regexp.MustCompile(`ORDER BY rank\s+LIMIT \$3`)

// windowOrders 는 SQL 안의 모든 "row_number() OVER (ORDER BY …) AS rank" 의 정렬 키 본문을 돌려준다.
func windowOrders(sql string) []string {
	var out []string
	const open = "row_number() OVER (ORDER BY"
	for {
		i := strings.Index(sql, open)
		if i < 0 {
			return out
		}
		sql = sql[i+len(open):]
		j := strings.Index(sql, ") AS rank")
		out = append(out, sql[:j])
	}
}

func tieBreakQueries(t *testing.T) map[string]string {
	t.Helper()
	from := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	base := model.SearchQuery{Query: "이번 주 zztiealpha 회의 알려줘", Limit: 10, Embedding: []float32{0.1, 0.2},
		OccurredFrom: &from, OccurredTo: &to}
	terms := sparseq.Extract(base.Query)

	out := map[string]string{}
	w := model.SearchWeights{}.Defaults()
	w.SummaryVec = model.DefaultSummaryVecWeight
	w.EntityWeight = model.DefaultEntityWeight

	legacy, _ := buildHybridSearchQuery(base, w)
	out["hybrid/legacy-entity"] = legacy

	sparse := base
	sparse.SparseTerms = model.SparseTerms{TSQuery: sparseq.TSQuery(terms.TS), Like: terms.Like}
	sparseSQL, _ := buildHybridSearchQuery(sparse, w)
	out["hybrid/sparse-terms"] = sparseSQL

	kw := base
	kw.EntityKeywords = []string{"zztiealpha"}
	kw.Tuning.GraphWeight = 0.5
	kwSQL, _ := buildHybridSearchQuery(kw, w)
	out["hybrid/keyword-entity+graph"] = kwSQL
	return out
}

func TestLaneOrderings_AreTotalOrders(t *testing.T) {
	t.Parallel()
	for name, sql := range tieBreakQueries(t) {
		matches := windowOrders(sql)
		// fts, vec, bigm, summvec, entity (+ graph) 레인.
		if len(matches) < 5 {
			t.Fatalf("%s: window 순서 %d개만 찾음", name, len(matches))
		}
		for _, key := range matches {
			if !strings.Contains(key, "occurred_at DESC NULLS LAST") {
				t.Errorf("%s: 레인 순서에 최신 사건 시각 동점 처리가 없다: %q", name, key)
			}
			if !regexp.MustCompile(`(?:^|[ .])(?:id|document_id|evidence_document_id) ASC\s*$`).MatchString(strings.TrimSpace(key)) {
				t.Errorf("%s: 레인 순서가 id 로 끝나지 않는다: %q", name, key)
			}
		}
		// 모든 레인 CTE(fts, vec, bigm, summvec, entity[, graph])는 LIMIT 앞에 ORDER BY rank.
		if got := len(cteLimitRe.FindAllString(sql, -1)); got != len(matches) {
			t.Errorf("%s: ORDER BY rank LIMIT $3 가 %d개, 레인은 %d개", name, got, len(matches))
		}
		// 레인 CTE + 최종 SELECT + 벡터 레인 안쪽의 인덱스 순서 절단(vec, summvec).
		if strings.Count(sql, "LIMIT $3") != len(matches)+1+2 {
			t.Errorf("%s: LIMIT $3 개수가 예상과 다르다", name)
		}
		// 최종 SELECT 도 id 로 끝난다.
		if !strings.Contains(sql, "ORDER BY score DESC, d.occurred_at DESC NULLS LAST, d.id ASC\n\t\tLIMIT $3") {
			t.Errorf("%s: 최종 ORDER BY 에 동점 처리가 없다", name)
		}
	}
}

func TestOtherSearchSQL_TieBreakers(t *testing.T) {
	t.Parallel()
	q := model.SearchQuery{Query: "zztie 회의", Limit: 10, Embedding: []float32{0.1, 0.2}}
	terms := sparseq.Extract(q.Query)
	withTerms := q
	withTerms.SparseTerms = model.SparseTerms{TSQuery: sparseq.TSQuery(terms.TS), Like: terms.Like}

	ft, _ := buildFulltextSearchQuery(q)
	ftTerms, _ := buildFulltextSearchQuery(withTerms)
	chunkFTS, _ := buildChunkFTSQuery(q, 30)
	chunkFTSTerms, _ := buildChunkFTSQuery(withTerms, 30)
	ctxSQL, _ := buildSparseContextQuery(q, 30, model.ChunkSparseCtxV1TP)

	for name, tc := range map[string]struct{ sql, want string }{
		"fulltext":         {ft, "ORDER BY score DESC, occurred_at DESC NULLS LAST, id ASC\n\t\tLIMIT $2"},
		"fulltext/terms":   {ftTerms, "ORDER BY score DESC, occurred_at DESC NULLS LAST, id ASC\n\t\tLIMIT $2"},
		"chunk_fts":        {chunkFTS, "ORDER BY rank DESC, d.occurred_at DESC NULLS LAST, c.id ASC\n\t\tLIMIT $2"},
		"chunk_fts/terms":  {chunkFTSTerms, "ORDER BY rank DESC, d.occurred_at DESC NULLS LAST, c.id ASC\n\t\tLIMIT $2"},
		"sparse_ctx/fresh": {ctxSQL, "ORDER BY rank DESC, d.occurred_at DESC NULLS LAST, c.id ASC\n\t\t\tLIMIT $2"},
		"sparse_ctx/union": {ctxSQL, "ORDER BY rank DESC, document_occurred_at DESC NULLS LAST, id ASC"},
	} {
		if !strings.Contains(tc.sql, tc.want) {
			t.Errorf("%s: %q 가 없다", name, tc.want)
		}
	}
	if n := strings.Count(ctxSQL, "ORDER BY rank DESC, d.occurred_at DESC NULLS LAST, c.id ASC"); n != 2 {
		t.Errorf("sparse_ctx: fresh/raw CTE 순서 %d개, want 2", n)
	}
}

// 벡터 레인(chunk_vector, vec, summvec)은 HNSW 인덱스 순서로 후보를 자르는 안쪽
// "ORDER BY 거리 LIMIT" 를 그대로 두고, 바깥에서만 동점을 깬다. 동점 키를 안쪽 ORDER BY 에
// 넣으면 인덱스 순서를 못 써서 전 테이블을 읽고 정렬한다(EXPLAIN 으로 확인한 회귀).
func TestVectorLanes_KeepIndexOrderedCut(t *testing.T) {
	t.Parallel()
	sql, args := buildChunkVectorQuery(model.SearchQuery{Embedding: []float32{0.1, 0.2}}, 30)
	for _, want := range []string{
		"ORDER BY c.embedding <=> $1::vector\n\t\t\tLIMIT $2\n\t\t) ann",
		"ORDER BY score DESC, document_occurred_at DESC NULLS LAST, id ASC",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("청크 벡터 레인에 %q 가 없다:\n%s", want, sql)
		}
	}
	if len(args) < 2 {
		t.Errorf("args = %v", args)
	}

	for name, hybrid := range tieBreakQueries(t) {
		for _, col := range []string{"embedding", "summary_embedding"} {
			inner := "ORDER BY " + col + " <=> $2 ASC\n\t\t\t\tLIMIT $3\n\t\t\t) ann"
			if !strings.Contains(hybrid, inner) {
				t.Errorf("%s: %s 레인의 안쪽 절단이 인덱스 순서가 아니다", name, col)
			}
		}
		if strings.Contains(hybrid, "ORDER BY embedding <=> $2 ASC,") || strings.Contains(hybrid, "ORDER BY summary_embedding <=> $2 ASC,") {
			t.Errorf("%s: 벡터 거리 ORDER BY 에 동점 키가 섞여 인덱스 순서를 막는다", name)
		}
	}
}
