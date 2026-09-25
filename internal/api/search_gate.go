package api

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/baekenough/second-brain/internal/model"
)

// 검색 동시성 제한(#286 항목 3).
//
// 서버 프로세스 하나의 pgx 풀을 검색(REST·GraphQL·golden·/ask)과 ingest(폰
// push 의 messages·recording·file), feedback, notes 가 함께 쓴다. 검색 한 건은
// 레인을 순차로 돌며 연결을 최대 1개 잡으므로, 동시 검색 수가 MaxConns 에
// 닿으면 풀이 비고 ingest 는 r.Context() 만 들고 연결을 무기한 기다린다. 폰은
// Cloudflare 게이트웨이 타임아웃(502)을 받고 같은 배치를 재전송해 부하가 더
// 쌓인다(2026-06-21 사고와 같은 모양).
//
// 그래서 "검색 서비스 호출 하나" 동안만 슬롯을 잡는 세마포어로 동시 검색 수를
// K 로 묶는다. 검색이 쓰는 연결은 K 개를 넘지 못하므로 MaxConns−K 개는 항상
// 비검색 경로의 몫으로 남는다. 풀 크기를 키우는 것(pool_max_conns)만으로는
// 이 보장이 생기지 않는다 — 검색이 늘어난 만큼 다 가져갈 수 있다.
//
// 슬롯 대기는 풀 연결을 잡지 않는다. 기다리는 동안 DB 에는 아무 부하도 없다.
//
// # 대기 우선순위: REST·GraphQL·golden 먼저, /ask 는 양보
//
// REST·GraphQL·golden 은 세마포어의 FIFO 대기열에 서서 최대 searchGateWait(1초)
// 기다린다. /ask 는 대기열에 서지 않고 askSearchPollInterval 마다 빈 슬롯을
// 시도(TryAcquire)하며 최대 askSearchWaitMax 까지 기다린다. semaphore.Weighted
// 의 TryAcquire 는 대기열에 누가 있으면 실패하고 Release 는 대기열부터 깨우므로,
// 슬롯이 돌 때 대기 중인 REST 가 항상 먼저 받는다.
//
// 이렇게 나눈 이유(deep-verify 재현): /ask 도 같은 FIFO 에 줄을 서면, 대기 상한이
// 긴 /ask 들이 앞자리를 차지해 슬롯이 정상적으로 돌고 있는데도 뒤에 온 REST 가
// 1초를 넘겨 503 을 받는다(K=1, /ask 검색 2건 대기 → REST 1.004초 뒤 503).
// /ask 에 대기 상한만 두거나 /ask 대기자 수를 K×2 로 묶어도 이 현상은 남는다 —
// 앞에 선 /ask 검색 몇 건의 실행 시간만으로 REST 의 1초 창은 지나간다. 그래서
// 대기자 수 상한 대신 우선순위를 나눴다. /ask 대기자는 대기열을 차지하지 않으므로
// 수를 묶을 필요도 없다(폴링 비용은 대기자당 askSearchPollInterval 마다 뮤텍스 1회).
// 대가: REST 가 쉬지 않고 몰리면 /ask 가 askSearchWaitMax 까지 슬롯을 못 얻을 수
// 있다 — 그때는 기존 "retrieval failed" SSE 로 끝난다.
//
// # 대기 시간과 타임아웃 예산
//
//   - REST GET/POST·golden/next: 슬롯을 얻은 뒤에 검색 타임아웃 ctx 를 만든다.
//     대기(최대 1초)는 searchTimeout 예산에 들어가지 않는다.
//   - GraphQL: guardGraphQL 이 요청 전체에 searchTimeout 을 한 번 건다. 필드의
//     슬롯 대기도 그 요청 단위 예산 안에서 일어난다(필드별 검색 타임아웃은 슬롯을
//     얻은 뒤 시작하지만 요청 ctx 를 넘지 못한다). 대기 중 요청 기한이 끝나면
//     errSearchTimeout 으로 분류한다.
//   - /ask: askTimeout 이 대기까지 포함해 요청 전체를 묶는다.
//
// # 슬롯 단위: 요청이 아니라 검색 호출
//
// GraphQL 요청 하나는 search 필드를 최대 graphqlMaxSearchFields(5)개,
// golden/next 는 최대 4회, /ask 는 2회 검색한다. 슬롯은 요청 단위(1개를 요청
// 끝까지)가 아니라 검색 호출 단위로 잡고 놓는다. 근거:
//
//   - 데드락이 구조적으로 불가능하다. 한 요청 안의 검색은 모두 순차다 —
//     graphql-go v0.8.1 은 query 연산의 필드를 한 goroutine 에서 차례로 푼다
//     (executor.go executeSubFields, 리졸버가 thunk 를 돌려주지 않으므로 병렬
//     해석 없음), golden/next·/ask 도 순차 호출이다. 그래서 한 요청이 동시에
//     쥐는 슬롯은 언제나 최대 1개이고, "슬롯을 쥔 채 다른 슬롯을 기다리는"
//     상태(hold-and-wait)가 생기지 않는다. 요청 단위 슬롯을 잡고 안에서 검색
//     단위 슬롯을 또 잡는 식으로 중첩하면 K 개 요청이 서로를 기다리며 멈출 수
//     있으므로, 게이트는 이 파일의 두 지점(searchWithTimeout, gatedSearcher)
//     에서만 잡고 절대 중첩하지 않는다.
//   - 요청 단위로 잡으면 검색이 아닌 구간(GraphQL 의 documents·stats 필드,
//     curated 검색의 LLM 큐레이션, /ask 의 LLM 생성)까지 슬롯을 쥔다. 그
//     시간에는 DB 연결을 쓰지 않으므로 보호 효과 없이 처리량만 줄어든다.
//   - 대가: 검색 5개짜리 GraphQL 요청이 중간에 슬롯을 못 얻으면 그 필드만
//     "search capacity exceeded" 오류가 되고 나머지는 결과를 받는다(GraphQL 의
//     부분 성공 관례). 대기 1초(searchGateWait)가 이 순간 경합을 흡수한다.

const (
	// searchGateWait 는 REST·GraphQL·golden 검색이 슬롯을 기다리는 상한이다.
	// 이슈 제안은 "즉시 503" 이었지만, GraphQL 검색 5개·golden 4회처럼 한
	// 요청이 슬롯을 순차로 잡았다 놓는 사이의 짧은 경합에서 헛 503 이 나지
	// 않도록 1초 기다린다. 대기는 풀 연결을 잡지 않으므로 ingest 보호 효과는
	// 즉시 거부와 같다. REST·golden 에서는 이 대기가 검색 타임아웃
	// (searchTimeout)에 들어가지 않는다 — 슬롯을 얻은 뒤에 타임아웃 ctx 를
	// 만든다. GraphQL 은 요청 단위 예산 안에서 기다린다(파일 머리 주석).
	searchGateWait = time.Second

	// searchBusyRetryAfter 는 503 응답의 Retry-After(초)다. 검색 p50 이 약
	// 0.5초라 2초면 대개 슬롯이 비어 있다.
	searchBusyRetryAfter = "2"

	// askSearchWaitMax 는 /ask 검색 호출 하나가 슬롯을 기다리는 상한이다.
	// /ask 는 SSE 헤더(200)를 이미 보낸 뒤라 503 을 줄 수 없어 REST 보다 길게
	// 기다리지만, askTimeout(기본 60초) 전체를 기다리게 두면 과부하 때 사용자는
	// 1분 동안 빈 화면을 보다 실패한다. 10초인 근거: 검색 p50 약 0.5초면 슬롯
	// 하나가 10초 동안 스무 번쯤 돈다 — 그래도 못 얻으면 일시 경합이 아니라
	// 과부하다. /ask 는 검색 2회(본검색·인사이트)라 최악 20초를 기다려도
	// askTimeout 기본값에 LLM 생성 몫 40초가 남는다. 초과하면 기존
	// "retrieval failed" 흐름으로 끝난다.
	askSearchWaitMax = 10 * time.Second

	// askSearchPollInterval 은 /ask 가 빈 슬롯을 다시 시도하는 간격이다. 검색
	// p50(약 0.5초)보다 충분히 짧아 슬롯이 비면 곧 잡고, 대기자당 초당 40회
	// 뮤텍스 획득이라 비용은 무시할 만하다.
	askSearchPollInterval = 25 * time.Millisecond
)

// SearchGateWait 는 REST·golden 검색이 슬롯을 기다리는 상한(searchGateWait)을
// 밖에 알린다. cmd/server 가 "슬롯 대기 + 검색 타임아웃" 이 HTTP WriteTimeout
// 안에 드는지 시작 시 점검하는 데 쓴다.
const SearchGateWait = searchGateWait

// errSearchBusy 는 searchGateWait 안에 검색 슬롯을 얻지 못했다는 뜻이다.
// REST·golden 은 503 + Retry-After, GraphQL 은 같은 문구의 errors[] 로 바꾼다.
// 문구는 고정이며 질의를 담지 않는다.
var errSearchBusy = errors.New("search capacity exceeded")

// searchGate 는 동시 검색 수를 묶는 세마포어다. nil 이면 제한이 없다(게이트를
// 배선하지 않은 테스트용 Server 의 기존 동작). 운영 서버는 cmd/server 가 항상
// WithSearchConcurrency 로 1 이상을 넣는다.
//
// golang.org/x/sync/semaphore 를 쓰는 이유: 대기자를 도착 순서(FIFO)로
// 깨워 오래 기다린 요청이 새로 온 요청에 계속 밀리지 않고, ctx 취소 중에
// 슬롯을 얻은 경합도 안에서 되돌려 준다.
type searchGate struct {
	sem  *semaphore.Weighted
	size int

	// askWait 는 /ask 검색 호출의 대기 상한이다. 운영에서는 항상
	// askSearchWaitMax 이고, 테스트만 짧게 바꾼다.
	askWait time.Duration
}

// newSearchGate 는 슬롯 k 개짜리 게이트를 만든다. k <= 0 이면 nil(제한 없음).
func newSearchGate(k int) *searchGate {
	if k <= 0 {
		return nil
	}
	return &searchGate{sem: semaphore.NewWeighted(int64(k)), size: k, askWait: askSearchWaitMax}
}

// capacity 는 슬롯 수다. nil 게이트는 0.
func (g *searchGate) capacity() int {
	if g == nil {
		return 0
	}
	return g.size
}

// acquire 는 FIFO 대기열에 서서 최대 wait 동안 슬롯 하나를 기다린다(REST·
// GraphQL·golden 용, 우선순위 높음). 못 얻으면 errSearchBusy, 그 전에 ctx 가
// 끝나면 ctx.Err() 를 감싸 돌려준다(클라이언트 이탈·요청 기한은 "용량 초과"가
// 아니다). wait 는 양수여야 한다 — 0 이하면 기다리지 않고 한 번만 시도한다.
//
// release 는 여러 번 불러도 슬롯을 한 번만 돌려준다. 두 번 돌려주면 다른
// 요청의 슬롯까지 풀려 K 를 넘는 검색이 들어오기 때문이다. 호출자는 얻은
// 직후 defer release() 로 패닉·오류·타임아웃 경로 모두에서 반환을 보장한다.
func (g *searchGate) acquire(ctx context.Context, wait time.Duration) (release func(), err error) {
	if g == nil {
		return func() {}, nil
	}
	if wait <= 0 {
		if g.sem.TryAcquire(1) {
			return g.releaser(), nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("waiting for search slot: %w", ctxErr)
		}
		return nil, errSearchBusy
	}

	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	if err := g.sem.Acquire(waitCtx, 1); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("waiting for search slot: %w", ctxErr)
		}
		return nil, errSearchBusy
	}
	return g.releaser(), nil
}

// acquireYielding 은 FIFO 대기열에 서지 않고 poll 간격으로 빈 슬롯을 시도하며
// 최대 wait 동안 기다린다(/ask 용, 우선순위 낮음 — 파일 머리 주석 참고).
// 오류 규약은 acquire 와 같다.
func (g *searchGate) acquireYielding(ctx context.Context, wait, poll time.Duration) (release func(), err error) {
	if g == nil {
		return func() {}, nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		if g.sem.TryAcquire(1) {
			return g.releaser(), nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for search slot: %w", ctx.Err())
		case <-timer.C:
			return nil, errSearchBusy
		case <-ticker.C:
		}
	}
}

// askWaitMax 는 /ask 검색 호출의 대기 상한이다. nil 게이트는 기다릴 일이 없다.
func (g *searchGate) askWaitMax() time.Duration {
	if g == nil {
		return 0
	}
	return g.askWait
}

// releaser 는 슬롯 하나를 한 번만 돌려주는 함수를 만든다.
func (g *searchGate) releaser() func() {
	var once sync.Once
	return func() { once.Do(func() { g.sem.Release(1) }) }
}

// gatedSearcher 는 /ask 의 Stage 2 검색에 게이트를 씌운다.
//
// /ask 는 searchWithTimeout 을 거치지 않으므로(검색·LLM 전체를 askTimeout 이
// 묶는다) 따로 감싼다. SSE 헤더(200)를 이미 보낸 뒤라 503 을 줄 수 없으므로
// REST 보다 길게(askSearchWaitMax) 기다리되, REST 대기자에게 양보한다
// (acquireYielding). 상한이나 ask ctx 가 먼저 끝나면 기존 "retrieval failed"
// SSE 경로로 떨어진다. 슬롯은 검색 호출 동안만 쥐고 LLM 생성 동안에는 쥐지 않는다.
type gatedSearcher struct {
	inner documentSearcher
	gate  *searchGate
}

func (g gatedSearcher) Search(ctx context.Context, q model.SearchQuery) ([]*model.SearchResult, error) {
	release, err := g.gate.acquireYielding(ctx, g.gate.askWaitMax(), askSearchPollInterval)
	if err != nil {
		return nil, err
	}
	defer release()
	return g.inner.Search(ctx, q)
}

// SearchConcurrency 는 풀 크기 maxConns 와 SEARCH_MAX_CONCURRENCY 설정값
// configured(0 = 자동)로 동시 검색 상한 K 를 정한다. clamped 는 설정값이
// 상한을 넘어 잘렸는지다(시작 로그 경고용).
//
//   - 자동(configured <= 0): max(1, maxConns/2). 이슈 제안값. 풀의 절반을
//     검색에, 나머지 절반을 ingest·feedback·notes·/ask 대화 저장 같은 비검색
//     경로에 남긴다. pgx 기본 MaxConns 는 max(4, CPU 수)라 최소 2 가 된다.
//   - 설정값: 그대로 쓰되 maxConns−1 로 자른다. 비검색 경로에 연결 1개는
//     반드시 남겨야 게이트의 존재 이유(ingest 보호)가 성립한다.
//   - maxConns 가 1 이하면 둘 다 1 이다. 이때는 비검색 경로 몫을 보장할 수
//     없으므로 호출자가 경고를 남긴다.
func SearchConcurrency(maxConns int32, configured int) (k int, clamped bool) {
	upper := max(1, int(maxConns)-1)
	if configured > 0 {
		if configured > upper {
			return upper, true
		}
		return configured, false
	}
	return max(1, int(maxConns)/2), false
}

// WithSearchConcurrency 는 동시 검색 상한을 k 로 건다. k <= 0 이면 게이트를
// 두지 않는다(기존 테스트 호환). 운영 배선(cmd/server)은 SearchConcurrency 로
// 구한 1 이상의 값을 넣는다. Handler() 전에 불러야 한다.
func (s *Server) WithSearchConcurrency(k int) *Server {
	s.searchGate = newSearchGate(k)
	return s
}
