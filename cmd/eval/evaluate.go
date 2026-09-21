package main

import (
	"context"
	"fmt"
	"time"

	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/baekenough/second-brain/internal/timeutil"
	"golang.org/x/sync/errgroup"
)

type evalSearcher interface {
	Search(context.Context, model.SearchQuery) ([]*model.SearchResult, error)
}

// tracedSearcher 는 검색과 함께 진단 기록을 돌려주는 검색기다. --dump 를 켰을
// 때만 쓰이며, 검색기가 이 인터페이스를 만족하지 않으면 진단 행은 순위 정보
// 없이(레인·풀 항목이 빈 채로) 남는다 — 조용히 평가 자체를 건너뛰지는 않는다.
type tracedSearcher interface {
	SearchTraced(context.Context, model.SearchQuery) ([]*model.SearchResult, *search.SearchTrace, error)
}

// windowResolver 는 질의 문구에서 사건 시각 창 [from, to) 를 뽑는다.
// ok=false 는 "기간 표현이 없다"는 뜻이며, 그 질의는 시간창 없이 검색한다.
type windowResolver func(query string) (from, to time.Time, ok bool)

// planWindowResolver 는 골든 후보 화면(internal/api/golden.go 의
// goldenResolveWindow)이 쓰는 것과 동일한 결정론적 기간 파서를 asOf 기준으로
// 적용한다. LLM 은 호출하지 않는다 — 평가가 모델 응답에 따라 흔들리면 같은
// 라벨로 같은 점수가 두 번 나오지 않는다.
//
// 다만 후보 화면과 똑같아지는 것은 "시간창" 하나뿐이다. 화면이 쓰는
// 관련도/최신순 두 스트림 병합과 IncludeRetention 은 재현하지 않는다. 전자는
// 검색 순위가 아니라 검토 편의를 위한 화면 구성이고, 후자는 운영 /ask 가 쓰지
// 않는 설정이라 여기서 켜면 운영보다 넓은 코퍼스를 재는 셈이 된다.
func planWindowResolver(asOf time.Time) windowResolver {
	return func(query string) (time.Time, time.Time, bool) {
		from, to, _, ok := intent.DeterministicWindow(query, asOf.In(timeutil.KST()))
		return from, to, ok
	}
}

// evalRunOptions 는 한 번의 평가 실행 설정이다.
type evalRunOptions struct {
	rerank bool
	// window 가 nil 이면 시간창 없이 전체 코퍼스에서 검색한다(기존 동작).
	window windowResolver
	// diagnose 가 true 면 질의별 진단 행을 모은다(--dump).
	diagnose bool
	// tuning 은 검색 튜닝 노브다. 제로값이면 서비스 기본값(= 환경변수)을
	// 쓰므로, 노브 플래그를 주지 않은 실행은 운영 설정 그대로 평가한다.
	tuning model.SearchTuning
}

type evaluation struct {
	Metrics                                             search.EvalMetrics
	Attempted, Failed, PositiveQueries, NegativeQueries int
	FPPenalty10                                         float64
	Latencies                                           []float64
	// Diagnostics 는 opts.diagnose 가 켜졌을 때만 채워지며, pairs 와 같은
	// 순서를 유지한다.
	Diagnostics []queryDiagnostics
}

// Each input owns one result slot, including failed searches. Failures receive
// empty results and remain in the relevance denominator; they also fail the run.
// Negative-only questions contribute to FP, not undefined relevance metrics.
func evaluatePairs(ctx context.Context, svc evalSearcher, pairs []store.EvalPair, opts evalRunOptions) evaluation {
	results := make([][]string, len(pairs))
	positive := make([]map[string]bool, len(pairs))
	negative := make([]map[string]bool, len(pairs))
	latencies := make([]float64, len(pairs))
	failed := make([]bool, len(pairs))
	diagnostics := make([]queryDiagnostics, len(pairs))
	traced, canTrace := svc.(tracedSearcher)
	var group errgroup.Group
	group.SetLimit(10)
	for i, p := range pairs {
		positive[i] = map[string]bool{}
		negative[i] = map[string]bool{}
		for _, id := range p.RelevantDocIDs {
			positive[i][id] = true
		}
		for _, id := range p.IrrelevantDocIDs {
			negative[i][id] = true
		}
		group.Go(func() error {
			query := model.SearchQuery{Query: p.Query, Limit: 10, UseRerank: opts.rerank, Tuning: opts.tuning}
			var applied *dumpWindow
			if opts.window != nil {
				if from, to, ok := opts.window(p.Query); ok {
					query.OccurredFrom, query.OccurredTo = &from, &to
					applied = &dumpWindow{From: from.Format(time.RFC3339), To: to.Format(time.RFC3339)}
				}
			}

			start := time.Now()
			var rows []*model.SearchResult
			var trace *search.SearchTrace
			var err error
			if opts.diagnose && canTrace {
				rows, trace, err = traced.SearchTraced(ctx, query)
			} else {
				rows, err = svc.Search(ctx, query)
			}
			latencies[i] = float64(time.Since(start).Nanoseconds()) / 1e6
			if err != nil {
				failed[i] = true
				rows = nil
			}
			for _, row := range rows {
				results[i] = append(results[i], row.ID.String())
			}
			if opts.diagnose {
				diagnostics[i] = buildDiagnostics(p, applied, rows, trace, failed[i], negative[i])
			}
			return nil
		})
	}
	_ = group.Wait()
	out := evaluation{Attempted: len(pairs), Latencies: latencies}
	if opts.diagnose {
		out.Diagnostics = diagnostics
	}
	var relevanceResults [][]string
	var relevanceLabels []map[string]bool
	for i := range pairs {
		if failed[i] {
			out.Failed++
		}
		if len(positive[i]) > 0 {
			out.PositiveQueries++
			relevanceResults = append(relevanceResults, results[i])
			relevanceLabels = append(relevanceLabels, positive[i])
		}
		if len(negative[i]) > 0 {
			out.NegativeQueries++
		}
	}
	out.Metrics = search.Aggregate(relevanceResults, relevanceLabels)
	out.FPPenalty10 = search.AggregateFPPenalty(results, negative, 10)
	return out
}

func (e evaluation) completionError() error {
	if e.Attempted == 0 {
		return fmt.Errorf("eval: no queries evaluated")
	}
	if e.Failed > 0 {
		return fmt.Errorf("eval: %d of %d searches failed; run is not a valid baseline", e.Failed, e.Attempted)
	}
	return nil
}
