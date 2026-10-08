package search

import (
	"context"
	"log/slog"
	"math"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// GraphSupportCounter 는 결과 시드 그래프 확장(SearchTuning.GraphExpandBoost)이
// 쓰는 선택 인터페이스다. *store.DocumentStore 가 만족한다. 만족하지 않는
// 저장소(테스트 더블 등)에서는 노브가 켜져 있어도 조용히 건너뛴다 — 인터페이스
// 단언 실패가 검색 실패가 되어서는 안 된다.
type GraphSupportCounter interface {
	GraphSupportCounts(ctx context.Context, q model.SearchQuery, seedDocIDs, candidateIDs []uuid.UUID) (map[uuid.UUID]int, error)
}

// applyGraphExpandBoost 는 융합이 끝난 후보 풀을 그래프 지지로 한 번 재정렬한다
// (Graphiti edge_search / Hindsight 의 결과 시드 확장).
//
//  1. 시드: 현재 순서의 상위 store.MaxGraphExpandSeedDocs 문서의 엔티티 ∪
//     질문 키워드(q.EntityKeywords)로 찾은 엔티티.
//  2. 저장소가 후보마다 시드와 이어진 서로 다른 관계 수 n 을 센다(필터 적용,
//     후보 자신의 엔티티만으로 이어진 관계 제외 — store.GraphSupportCounts).
//  3. score *= 1 + boost*s, s = graphSupportStrength(n, maxN) ∈ [0,1].
//
// 새 문서는 더하지 않는다 — 이미 후보인 문서의 점수만 바꾼다. 꺼져 있거나
// (boost=0), 최신순 정렬 질의(점수 순서가 아니므로 재정렬하면 요청한 순서를
// 깬다)이거나, 후보가 2건 미만이거나, 저장소가 인터페이스를 만족하지 않거나,
// 저장소 호출이 실패하면 results 를 그대로 돌려준다.
//
// 로그에는 개수만 남긴다. 키워드·엔티티 이름·질문은 남기지 않는다.
func (s *Service) applyGraphExpandBoost(ctx context.Context, q model.SearchQuery, results []*model.SearchResult, tune model.SearchTuning) []*model.SearchResult {
	if tune.GraphExpandBoost <= 0 || q.SortsByRecency() || len(results) < 2 {
		return results
	}
	counter, ok := s.store.(GraphSupportCounter)
	if !ok {
		return results
	}

	candidates := results
	if len(candidates) > store.MaxGraphExpandCandidates {
		candidates = candidates[:store.MaxGraphExpandCandidates]
	}
	ids := make([]uuid.UUID, len(candidates))
	for i, r := range candidates {
		ids[i] = r.ID
	}
	seeds := ids
	if len(seeds) > store.MaxGraphExpandSeedDocs {
		seeds = seeds[:store.MaxGraphExpandSeedDocs]
	}

	support, err := counter.GraphSupportCounts(ctx, q, seeds, ids)
	if err != nil {
		slog.Warn("search: graph expand boost failed, keeping fused order", "error", err)
		return results
	}
	boosted := boostByGraphSupport(results, support, tune.GraphExpandBoost)
	slog.Debug("search: graph expand boost",
		"candidates", len(ids), "seed_docs", len(seeds), "supported", len(support), "boosted", boosted)
	return results
}

// boostByGraphSupport 는 results 의 점수를 제자리에서 올리고 sortByScore 로 다시
// 정렬한다. 올린 결과 수를 돌려준다.
//
// 성질(테스트로 고정):
//   - 유한: 승수는 [1, 1+boost] 안이다. boost 는 Normalized 가 [0,1] 로 자른다.
//   - 단조: 지지 n 이 클수록 승수가 크거나 같다.
//   - 재정렬만: 길이·원소 집합이 바뀌지 않는다. support 에만 있는 ID 는 무시한다.
//   - 점수가 0 이하·NaN·무한인 결과는 건드리지 않는다(곱셈이 순서를 뒤집거나
//     NaN 을 퍼뜨리지 않게).
func boostByGraphSupport(results []*model.SearchResult, support map[uuid.UUID]int, boost float64) int {
	if boost <= 0 || math.IsNaN(boost) || math.IsInf(boost, 0) || len(support) == 0 {
		return 0
	}
	if boost > 1 {
		boost = 1
	}
	maxN := 0
	for _, r := range results {
		if n := support[r.ID]; n > maxN {
			maxN = n
		}
	}
	if maxN == 0 {
		return 0
	}
	boosted := 0
	for _, r := range results {
		n := support[r.ID]
		if n <= 0 || r.Score <= 0 || math.IsNaN(r.Score) || math.IsInf(r.Score, 0) {
			continue
		}
		r.Score *= 1 + boost*graphSupportStrength(n, maxN)
		boosted++
	}
	if boosted > 0 {
		sortByScore(results)
	}
	return boosted
}

// graphSupportStrength 는 지지 관계 수 n 을 [0,1] 로 정규화한다:
// ln(1+n)/ln(1+maxN). 후보 풀에서 가장 많이 지지받은 문서가 1, 지지가 없으면 0.
// 로그 비율이라 관계가 아주 많은 문서 하나가 나머지 지지를 0 근처로 누르지 않는다.
func graphSupportStrength(n, maxN int) float64 {
	if n <= 0 || maxN <= 0 {
		return 0
	}
	if n >= maxN {
		return 1
	}
	return math.Log1p(float64(n)) / math.Log1p(float64(maxN))
}
