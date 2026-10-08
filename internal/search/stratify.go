package search

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// LaneSourceStratify 는 소스별 보조 검색(SourceStratifyK)이 올린 후보의
// 레인 이름이다(SearchTrace.LaneHits).
const LaneSourceStratify = "source_stratify"

// stratifySources 는 보조 검색을 따로 돌리는 주요 소스다. 골든셋 실패 사례
// (다른 소스 문서에 밀려 20건 후보 풀에 못 든 정답, 일정 질문에서 빠진
// 캘린더 문서)가 이 다섯 소스에 몰려 있다. 순서는 결과에 영향이 없다 —
// 병합은 소스 순서와 무관한 RRF 합이고 마지막에 sortByScore 로 총순서를
// 다시 세운다. 다만 로그·trace 순서를 고정하기 위해 슬라이스로 둔다.
var stratifySources = []model.SourceType{
	model.SourceCalendar,
	model.SourceCall,
	model.SourceSMS,
	model.SourceGmail,
	model.SourceNote,
}

// stratifyArmSources 는 이 질의에서 보조 검색을 돌릴 소스를 고른다.
//
//   - k<=0 이면 끈다.
//   - 질의에 소스 포함 집합(SourceType/SourceTypes)이 있으면 끈다. 호출자가
//     이미 소스를 골랐으므로 "다른 소스에 밀려 빠진다" 는 문제가 없다.
//   - 질의가 제외한 소스(ExcludeSourceTypes, 정규화 후)는 건너뛴다.
func stratifyArmSources(q model.SearchQuery, k int) []model.SourceType {
	if k <= 0 || len(q.IncludeSourceTypes()) > 0 {
		return nil
	}
	excluded := make(map[model.SourceType]bool, len(q.ExcludeSourceTypes))
	for _, st := range q.ExcludeSourceTypes {
		excluded[model.NormalizeSourceType(st)] = true
	}
	out := make([]model.SourceType, 0, len(stratifySources))
	for _, st := range stratifySources {
		if !excluded[st] {
			out = append(out, st)
		}
	}
	return out
}

// sourceArms 는 진행 중인 소스별 보조 검색이다. nil 이면 노브가 꺼진 것이며,
// 모든 메서드가 nil 수신자를 받는다.
type sourceArms struct {
	wg      sync.WaitGroup
	ctx     context.Context
	cancel  context.CancelFunc
	sources []model.SourceType
	lists   [][]*model.SearchResult // 인덱스별로 한 고루틴만 쓴다
	k       int
	started time.Time
}

// startSourceArms 는 본 저장소 검색과 동시에 소스별 보조 검색을 띄운다
// (LightRAG round-robin / Hindsight per-arm retrieval).
//
// 각 보조 검색은 본 검색에 넘기는 storeQuery 의 사본이다 — 시간창·제외
// 소스·retention 제외·삭제 문서 정책·가중치·임베딩·희소/엔티티 키워드가
// 전부 같고, 다른 것은 소스 포함 집합(해당 소스 하나)과 Limit(k)뿐이다.
// 고루틴 수는 소스 수(최대 5)로 고정이고, 같은 ctx 를 쓰므로 요청의
// 취소·타임아웃을 그대로 따른다.
//
// 실패는 흡수한다: 한 소스의 검색이 실패하면 그 소스만 빠지고 검색은
// 계속된다. 보조 검색은 후보를 더하는 실험 신호일 뿐이라 본 검색을 실패시킬
// 이유가 없다.
func (s *Service) startSourceArms(ctx context.Context, storeQuery model.SearchQuery, k int) *sourceArms {
	sources := stratifyArmSources(storeQuery, k)
	if len(sources) == 0 {
		return nil
	}
	// 보조 검색 전용 ctx. 호출자 ctx 가 context.Background 여도 Search 가
	// 일찍 반환하는 경로(저장소 오류 등)에서 stop 이 남은 질의를 끊는다.
	armCtx, cancel := context.WithCancel(ctx)
	a := &sourceArms{
		ctx:     armCtx,
		cancel:  cancel,
		sources: sources,
		lists:   make([][]*model.SearchResult, len(sources)),
		k:       k,
		started: time.Now(),
	}
	for i, st := range sources {
		armQ := storeQuery
		armQ.SourceType = nil
		armQ.SourceTypes = []model.SourceType{st}
		armQ.Limit = k
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			res, err := s.store.Search(armCtx, armQ)
			if err != nil {
				if armCtx.Err() != nil {
					// stop 이 끊었거나 요청이 취소됐다: 실패가 아니라 중단이다.
					return
				}
				// 질의 텍스트는 싣지 않는다. 소스 이름은 개인 데이터가 아니다.
				slog.Warn("search: source stratify arm failed, skipping",
					"source_type", string(st), "error", err)
				return
			}
			a.lists[i] = res
		}()
	}
	return a
}

// active 는 보조 검색이 시작됐는지 알린다(nil 수신자 허용).
func (a *sourceArms) active() bool { return a != nil }

// stop 은 아직 도는 보조 검색을 취소한다. Search 가 반환할 때 항상(defer)
// 부른다 — merge 뒤라면 이미 끝난 질의라 무해하고, merge 전에 일찍 반환하는
// 경로라면 최대 소스 수만큼의 DB 질의가 요청보다 오래 살아남지 않게 한다.
// 기다리지는 않는다: 고루틴은 취소된 ctx 로 곧 끝나고 결과는 버려진다.
func (a *sourceArms) stop() {
	if a != nil {
		a.cancel()
	}
}

// merge 는 보조 검색이 끝나기를 기다린 뒤 전역 융합 목록과 합친다. 두 번째
// 값은 결과가 바뀌었는지다 — 보조 검색이 새 근거를 하나도 못 냈으면 전역
// 목록을 점수까지 그대로 돌려준다.
func (a *sourceArms) merge(q model.SearchQuery, global []*model.SearchResult, trace *SearchTrace) ([]*model.SearchResult, bool) {
	if a == nil {
		return global, false
	}
	a.wg.Wait()

	hits := 0
	lists := make([][]*model.SearchResult, 0, len(a.lists))
	for i, res := range a.lists {
		res = keepSource(res, a.sources[i], a.k)
		// 저장소가 SQL 로 이미 거른다. 본 검색 경로와 같은 집행 지점을
		// 공유하기 위해 한 번 더 부른다(무해한 no-op).
		res = applyRetentionExclusion(q, res)
		trace.recordLane(LaneSourceStratify, res)
		hits += len(res)
		lists = append(lists, res)
	}
	// 지연시간은 실험의 비용이라 남긴다. 개수·시간만 — 내용은 싣지 않는다.
	slog.Debug("search: source stratify arms done",
		"arms", len(a.sources), "hits", hits,
		"elapsed_ms", time.Since(a.started).Milliseconds())
	if hits == 0 {
		return global, false
	}
	return mergeStratified(global, lists), true
}

// keepSource 는 보조 검색 결과에서 해당 소스 문서만, 앞에서 k 건까지 남긴다.
// 운영 저장소는 SQL 로 이미 지키는 조건이지만, 필터를 무시하는 저장소(테스트
// 더블·레거시 어댑터)가 다른 소스 문서를 "그 소스의 상위 k" 로 끼워 넣지
// 못하게 한다.
func keepSource(res []*model.SearchResult, st model.SourceType, k int) []*model.SearchResult {
	out := make([]*model.SearchResult, 0, min(len(res), k))
	for _, r := range res {
		if len(out) >= k {
			break
		}
		if r != nil && model.NormalizeSourceType(r.SourceType) == st {
			out = append(out, r)
		}
	}
	return out
}

// mergeStratified 는 전역 융합 목록과 소스별 목록을 RRF 로 합친다.
//
//	score(d) = Σ_{d 를 담은 목록 L} 1/(rrfK + rank_L(d))
//
// 전역 목록도 순위만 쓴다(mergeRRF 와 같은 규약). 두 목록에 모두 있는 문서는
// 두 항을 다 받는다 — 전역 융합과 소스 내 경쟁이 독립적으로 동의한 셈이다.
// 한 목록 안의 중복 ID 는 첫 순위만 센다.
//
// 결과는 합집합이다: 전역 목록의 어떤 후보도 빠지지 않고, 크기는 최대
// len(global) + 소스 수 × k 다. 같은 문서는 전역 목록의 사본(청크 근거
// Evidence 포함)을 우선 쓴다. 정렬은 sortByScore(총순서).
func mergeStratified(global []*model.SearchResult, arms [][]*model.SearchResult) []*model.SearchResult {
	type entry struct {
		r     *model.SearchResult
		score float64
	}
	total := len(global)
	for _, l := range arms {
		total += len(l)
	}
	byID := make(map[uuid.UUID]*entry, total)
	order := make([]*entry, 0, total)
	add := func(list []*model.SearchResult) {
		seen := make(map[uuid.UUID]bool, len(list))
		for rank, r := range list {
			if r == nil || seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			c := 1.0 / (rrfK + float64(rank+1))
			if e, ok := byID[r.ID]; ok {
				e.score += c
				continue
			}
			cp := *r // 얕은 복사 — 레인 결과를 변형하지 않는다
			e := &entry{r: &cp, score: c}
			byID[r.ID] = e
			order = append(order, e)
		}
	}
	add(global)
	for _, l := range arms {
		add(l)
	}
	out := make([]*model.SearchResult, len(order))
	for i, e := range order {
		e.r.Score = e.score
		out[i] = e.r
	}
	sortByScore(out)
	return out
}

// rerankInputCap 은 보조 검색이 켜졌을 때 리랭커에 보내는 후보 수의 상한이다.
//
// 보조 검색 합류는 후보 풀을 laneLimit(이미 rerankPoolLimit 이 적용된 값)보다
// 최대 소스 수 × K 건 키운다. 그 증가분까지는 리랭커에 보내되(그게 이 노브의
// 목적이다), 어떤 경우에도 overfetchLimitCap 을 넘기지 않는다 — 실험 노브가
// 외부 리랭커 호출 크기를 무한정 키울 수 없어야 한다.
//
// 보조 검색이 꺼져 있으면 0(상한 없음)을 돌려준다. 이때 후보 풀은 지금처럼
// laneLimit 이하이므로, 상한을 걸지 않는 것이 노브 도입 전과 같은 동작이다.
func rerankInputCap(laneLimit int, tune model.SearchTuning) int {
	if tune.SourceStratifyK <= 0 {
		return 0
	}
	return min(laneLimit+tune.SourceStratifyK*len(stratifySources), overfetchLimitCap)
}

// splitRerankInput 은 후보를 리랭커에 보낼 머리와 보내지 않을 꼬리로 나눈다.
// 꼬리는 버리지 않는다 — joinRerankTail 이 리랭크된 머리 뒤에 융합 순서
// 그대로 다시 붙인다. limit<=0 이거나 후보가 그 이하이면 꼬리는 nil 이다.
func splitRerankInput(results []*model.SearchResult, limit int) (head, tail []*model.SearchResult) {
	if limit <= 0 || len(results) <= limit {
		return results, nil
	}
	return results[:limit:limit], results[limit:]
}

// joinRerankTail 은 리랭크된 머리 뒤에 꼬리를 붙인 새 슬라이스를 만든다.
func joinRerankTail(head, tail []*model.SearchResult) []*model.SearchResult {
	if len(tail) == 0 {
		return head
	}
	out := make([]*model.SearchResult, 0, len(head)+len(tail))
	return append(append(out, head...), tail...)
}
