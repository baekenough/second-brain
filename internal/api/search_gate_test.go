package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// #286 항목 3: 검색 동시성 게이트.
//
// 모든 HTTP 검사는 실제 라우터(srv.Handler())를 거친다 — recoverer 미들웨어가
// 있어야 패닉 경로의 슬롯 반환을 확인할 수 있다. ingest 보호 자체(풀 연결이
// 비검색 경로에 남는가)는 search_gate_db_test.go 가 실DB 로 증명한다.

// holdingSearcher 는 release 로 값을 받을 때까지(또는 ctx 가 끝날 때까지)
// 막혀 있는 검색기다. 들어올 때마다 entered 에 신호를 보낸다.
type holdingSearcher struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func newHoldingSearcher() *holdingSearcher {
	return &holdingSearcher{entered: make(chan struct{}, 64), release: make(chan struct{})}
}

func (h *holdingSearcher) Search(ctx context.Context, _ model.SearchQuery) ([]*model.SearchResult, error) {
	h.calls.Add(1)
	h.entered <- struct{}{}
	select {
	case <-h.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func waitEntered(t *testing.T, h *holdingSearcher, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-h.entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d searches reached the searcher", i, n)
		}
	}
}

// doGateSearch 는 REST 검색 요청 하나를 라우터로 보낸다.
func doGateSearch(srv *Server, ctx context.Context, method string) *httptest.ResponseRecorder {
	var req *http.Request
	if method == http.MethodPost {
		req = httptest.NewRequest(http.MethodPost, "/api/v1/search", strings.NewReader(`{"query":"gate probe"}`))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(http.MethodGet, "/api/v1/search?q=gate+probe", nil)
	}
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// holdSlots 는 게이트 슬롯을 테스트가 직접 n 개 쥔다(검색기 없이 "다른 요청이
// 검색 중" 상태를 만든다). 반환 함수로 모두 돌려준다.
func holdSlots(t *testing.T, srv *Server, n int) func() {
	t.Helper()
	releases := make([]func(), 0, n)
	for i := 0; i < n; i++ {
		r, err := srv.searchGate.acquire(context.Background(), searchGateWait)
		if err != nil {
			t.Fatalf("hold slot %d: %v", i, err)
		}
		releases = append(releases, r)
	}
	return func() {
		for _, r := range releases {
			r()
		}
	}
}

func assertBusy503(t *testing.T, w *httptest.ResponseRecorder, elapsed time.Duration) {
	t.Helper()
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != searchBusyRetryAfter {
		t.Errorf("Retry-After = %q, want %q", got, searchBusyRetryAfter)
	}
	if !strings.Contains(w.Body.String(), errSearchBusy.Error()) {
		t.Errorf("body = %s, want the fixed %q message", w.Body.String(), errSearchBusy.Error())
	}
	// 즉시 거부가 아니라 searchGateWait 만큼 기다린 뒤의 503 이어야 한다.
	if elapsed < searchGateWait-50*time.Millisecond {
		t.Errorf("503 after %v, want it only after waiting ~%v for a slot", elapsed, searchGateWait)
	}
	if elapsed > searchGateWait+3*time.Second {
		t.Errorf("503 after %v, want the wait bounded by ~%v", elapsed, searchGateWait)
	}
}

// K 개가 검색 중이면 K+1 번째는 1초 대기 후 503 이고 검색기를 부르지 않는다.
// 슬롯 하나가 돌아오면 다음 요청은 다시 성공한다.
func TestSearchGate_REST_OverLimitReturns503(t *testing.T) {
	t.Parallel()

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			const k = 2
			h := newHoldingSearcher()
			// 게이트가 빠지면 초과 요청이 검색기에 막혀 기본 타임아웃(60초)까지
			// 끌므로, 결함이 빨리 드러나게 짧게 둔다(정상 경로는 1초 남짓).
			srv := newSearchInputTestServer(h).WithSearchConcurrency(k).WithSearchTimeout(5 * time.Second)

			codes := make(chan int, k+1)
			for i := 0; i < k; i++ {
				go func() { codes <- doGateSearch(srv, context.Background(), method).Code }()
			}
			waitEntered(t, h, k)

			start := time.Now()
			w := doGateSearch(srv, context.Background(), method)
			assertBusy503(t, w, time.Since(start))
			if got := h.calls.Load(); got != k {
				t.Errorf("searcher called %d times, want %d — the rejected request must not reach the store", got, k)
			}

			// 슬롯 하나 반환 → 새 요청이 검색기에 도달하고 성공한다.
			h.release <- struct{}{}
			if code := <-codes; code != http.StatusOK {
				t.Errorf("released search status = %d, want 200", code)
			}
			go func() { codes <- doGateSearch(srv, context.Background(), method).Code }()
			waitEntered(t, h, 1)
			for i := 0; i < k; i++ {
				h.release <- struct{}{}
				if code := <-codes; code != http.StatusOK {
					t.Errorf("search after slot release status = %d, want 200", code)
				}
			}
		})
	}
}

// firstCallSearcher 는 첫 호출에만 first 를 실행하고 이후 호출은 즉시 성공한다.
type firstCallSearcher struct {
	n     atomic.Int32
	first func(ctx context.Context) error
}

func (f *firstCallSearcher) Search(ctx context.Context, _ model.SearchQuery) ([]*model.SearchResult, error) {
	if f.n.Add(1) == 1 {
		return nil, f.first(ctx)
	}
	return nil, nil
}

// 검색이 오류·패닉·타임아웃·클라이언트 이탈로 끝나도 슬롯이 돌아와야 한다.
// K=1 이라 슬롯이 새면 다음 요청은 1초 기다린 뒤 503 이 된다.
func TestSearchGate_SlotReturnedOnEveryExitPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		first   func(entered chan<- struct{}) func(ctx context.Context) error
		timeout time.Duration
		cancel  bool
	}{
		{name: "error", first: func(chan<- struct{}) func(context.Context) error {
			return func(context.Context) error { return errors.New("lane failed") }
		}},
		{name: "panic", first: func(chan<- struct{}) func(context.Context) error {
			return func(context.Context) error { panic("injected search panic") }
		}},
		{name: "timeout", timeout: 50 * time.Millisecond, first: func(chan<- struct{}) func(context.Context) error {
			return func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
		}},
		{name: "client_cancel", cancel: true, first: func(entered chan<- struct{}) func(context.Context) error {
			return func(ctx context.Context) error { close(entered); <-ctx.Done(); return ctx.Err() }
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			entered := make(chan struct{})
			f := &firstCallSearcher{first: tc.first(entered)}
			srv := newSearchInputTestServer(f).WithSearchConcurrency(1)
			if tc.timeout > 0 {
				srv = srv.WithSearchTimeout(tc.timeout)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				go func() { <-entered; cancel() }()
			}
			if w := doGateSearch(srv, ctx, http.MethodGet); w.Code == http.StatusOK {
				t.Fatalf("first search status = 200, want a failure for the %s path", tc.name)
			}

			start := time.Now()
			w := doGateSearch(srv, context.Background(), http.MethodGet)
			if w.Code != http.StatusOK {
				t.Fatalf("follow-up status = %d, want 200 — the %s path leaked its slot; body = %s", w.Code, tc.name, w.Body.String())
			}
			if elapsed := time.Since(start); elapsed > searchGateWait/2 {
				t.Errorf("follow-up waited %v for a slot; the %s path returned it late", elapsed, tc.name)
			}
		})
	}
}

// 슬롯을 기다리는 동안 클라이언트가 떠나면 기다림을 즉시 끝내고, 그것을
// "용량 초과"(503)로 보고하지 않는다. 검색기도 부르지 않는다.
func TestSearchGate_ContextCanceledWhileWaiting(t *testing.T) {
	t.Parallel()

	docs := &callCountingSearcher{}
	srv := newSearchInputTestServer(docs).WithSearchConcurrency(1)
	releaseHeld := holdSlots(t, srv, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	w := doGateSearch(srv, ctx, http.MethodGet)
	elapsed := time.Since(start)

	if w.Code == http.StatusServiceUnavailable || w.Code == http.StatusOK {
		t.Errorf("status = %d; a client that left while waiting is neither served nor 'capacity exceeded'", w.Code)
	}
	if elapsed > searchGateWait-200*time.Millisecond {
		t.Errorf("waited %v; the wait must end with the request context", elapsed)
	}
	if got := docs.calls.Load(); got != 0 {
		t.Errorf("searcher called %d times, want 0", got)
	}

	releaseHeld()
	if w := doGateSearch(srv, context.Background(), http.MethodGet); w.Code != http.StatusOK {
		t.Errorf("after release status = %d, want 200 (the canceled waiter must not keep a slot)", w.Code)
	}
}

func TestSearchGate_AcquireSemantics(t *testing.T) {
	t.Parallel()

	t.Run("nil_gate_is_unlimited", func(t *testing.T) {
		t.Parallel()
		var g *searchGate
		for i := 0; i < 3; i++ {
			release, err := g.acquire(context.Background(), time.Millisecond)
			if err != nil {
				t.Fatalf("nil gate acquire: %v", err)
			}
			release()
		}
		if g.capacity() != 0 {
			t.Errorf("nil gate capacity = %d, want 0", g.capacity())
		}
	})

	t.Run("k_zero_or_negative_means_no_gate", func(t *testing.T) {
		t.Parallel()
		for _, k := range []int{0, -1} {
			if g := newSearchGate(k); g != nil {
				t.Errorf("newSearchGate(%d) = %+v, want nil", k, g)
			}
		}
	})

	t.Run("double_release_returns_one_slot", func(t *testing.T) {
		t.Parallel()
		g := newSearchGate(1)
		r1, err := g.acquire(context.Background(), time.Second)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		r1()
		r1() // 두 번째 호출은 아무것도 돌려주지 않아야 한다.

		r2, err := g.acquire(context.Background(), time.Second)
		if err != nil {
			t.Fatalf("acquire after release: %v", err)
		}
		defer r2()
		if _, err := g.acquire(context.Background(), 50*time.Millisecond); !errors.Is(err, errSearchBusy) {
			t.Errorf("second concurrent acquire err = %v, want errSearchBusy (double release over-admitted)", err)
		}
	})

	t.Run("ctx_error_is_not_busy", func(t *testing.T) {
		t.Parallel()
		g := newSearchGate(1)
		r, _ := g.acquire(context.Background(), time.Second)
		defer r()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := g.acquire(ctx, time.Second)
		if errors.Is(err, errSearchBusy) || !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want a wrapped context.Canceled", err)
		}
	})

	t.Run("zero_wait_tries_once", func(t *testing.T) {
		t.Parallel()
		g := newSearchGate(1)
		r, _ := g.acquire(context.Background(), time.Second)
		defer r()
		start := time.Now()
		if _, err := g.acquire(context.Background(), 0); !errors.Is(err, errSearchBusy) {
			t.Errorf("err = %v, want errSearchBusy", err)
		}
		if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
			t.Errorf("wait<=0 blocked for %v; it must try once and return", elapsed)
		}
	})

	t.Run("yielding_is_bounded_by_wait", func(t *testing.T) {
		t.Parallel()
		g := newSearchGate(1)
		r, _ := g.acquire(context.Background(), time.Second)
		defer r()
		// 상한이 빠진 구현이 테스트를 영원히 막지 않도록 바깥 ctx 에도 기한을 둔다.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		start := time.Now()
		_, err := g.acquireYielding(ctx, 150*time.Millisecond, 10*time.Millisecond)
		elapsed := time.Since(start)
		if !errors.Is(err, errSearchBusy) {
			t.Errorf("err = %v, want errSearchBusy after the wait cap", err)
		}
		if elapsed < 140*time.Millisecond || elapsed > time.Second {
			t.Errorf("gave up after %v, want ~150ms", elapsed)
		}
	})

	t.Run("yielding_ctx_error_is_not_busy", func(t *testing.T) {
		t.Parallel()
		g := newSearchGate(1)
		r, _ := g.acquire(context.Background(), time.Second)
		defer r()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := g.acquireYielding(ctx, time.Second, 10*time.Millisecond)
		if errors.Is(err, errSearchBusy) || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want a wrapped context.DeadlineExceeded", err)
		}
	})

	// 슬롯이 돌 때 FIFO 대기자(REST)가 폴링 대기자(/ask)보다 먼저 받는다.
	t.Run("yielding_waiter_yields_to_fifo_waiter", func(t *testing.T) {
		t.Parallel()
		g := newSearchGate(1)
		hold, _ := g.acquire(context.Background(), time.Second)

		askGot := make(chan func(), 1)
		askCtx, cancelAsk := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelAsk()
		go func() {
			r, err := g.acquireYielding(askCtx, 5*time.Second, 5*time.Millisecond)
			if err != nil {
				t.Errorf("yielding acquire: %v", err)
				askGot <- func() {}
				return
			}
			askGot <- r
		}()
		time.Sleep(50 * time.Millisecond) // /ask 가 먼저 기다리기 시작한다

		restGot := make(chan error, 1)
		var restRelease func()
		go func() {
			r, err := g.acquire(context.Background(), time.Second)
			restRelease = r
			restGot <- err
		}()
		time.Sleep(50 * time.Millisecond)
		hold()

		if err := <-restGot; err != nil {
			t.Fatalf("FIFO waiter lost the slot to the yielding waiter: %v", err)
		}
		select {
		case r := <-askGot:
			r()
			t.Fatal("yielding waiter got a slot while the FIFO waiter held the only one")
		case <-time.After(100 * time.Millisecond):
		}
		restRelease()
		select {
		case r := <-askGot:
			r()
		case <-time.After(2 * time.Second):
			t.Fatal("yielding waiter never got the slot after it was freed")
		}
	})
}

// 무작위 취소·짧은 대기·이중 반환·패닉을 섞어도 끝나면 슬롯이 정확히 K 개로
// 돌아온다(deep-verify R3 재현을 옮김). FIFO·폴링 두 대기 방식을 모두 섞는다.
func TestSearchGate_SlotAccountingStress(t *testing.T) {
	t.Parallel()

	const k = 3
	g := newSearchGate(k)
	done := make(chan struct{})
	for i := 0; i < 3000; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			defer func() { _ = recover() }()
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i%3)*time.Millisecond)
			defer cancel()
			var (
				release func()
				err     error
			)
			if i%2 == 0 {
				release, err = g.acquire(ctx, time.Duration(i%2)*time.Millisecond)
			} else {
				release, err = g.acquireYielding(ctx, time.Duration(i%4)*time.Millisecond, time.Millisecond)
			}
			if err != nil {
				return
			}
			defer release()
			if i%7 == 0 {
				release() // 이중 호출(멱등)
			}
			if i%11 == 0 {
				panic("injected panic")
			}
			time.Sleep(time.Duration(i%200) * time.Microsecond)
		}(i)
	}
	for i := 0; i < 3000; i++ {
		<-done
	}
	for i := 0; i < k; i++ {
		if !g.sem.TryAcquire(1) {
			t.Fatalf("slot leaked: only %d of %d free", i, k)
		}
	}
	if g.sem.TryAcquire(1) {
		t.Fatalf("over-release: more than %d slots", k)
	}
}

func TestSearchConcurrency(t *testing.T) {
	t.Parallel()

	cases := []struct {
		maxConns    int32
		configured  int
		want        int
		wantClamped bool
	}{
		// 자동: max(1, MaxConns/2)
		{maxConns: 4, want: 2},
		{maxConns: 8, want: 4},
		{maxConns: 9, want: 4},
		{maxConns: 3, want: 1},
		{maxConns: 2, want: 1},
		{maxConns: 1, want: 1},
		{maxConns: 4, configured: -1, want: 2}, // 음수는 config 가 이미 0 으로 바꾸지만 방어
		// 설정값: 그대로, 단 MaxConns-1 로 자름
		{maxConns: 8, configured: 3, want: 3},
		{maxConns: 8, configured: 7, want: 7},
		{maxConns: 8, configured: 8, want: 7, wantClamped: true},
		{maxConns: 8, configured: 100, want: 7, wantClamped: true},
		{maxConns: 1, configured: 5, want: 1, wantClamped: true},
	}
	for _, tc := range cases {
		k, clamped := SearchConcurrency(tc.maxConns, tc.configured)
		if k != tc.want || clamped != tc.wantClamped {
			t.Errorf("SearchConcurrency(%d, %d) = (%d, %v), want (%d, %v)",
				tc.maxConns, tc.configured, k, clamped, tc.want, tc.wantClamped)
		}
	}
}

// 슬롯 대기 시간은 검색 타임아웃에 포함되지 않는다: 타임아웃보다 오래 기다린
// 뒤에도 검색기는 타임아웃 전체를 받는다.
func TestSearchGate_WaitNotChargedToSearchTimeout(t *testing.T) {
	t.Parallel()

	const timeout = 400 * time.Millisecond
	g := &deadlineRecordingSearcher{}
	srv := newSearchInputTestServer(g).WithSearchTimeout(timeout).WithSearchConcurrency(1)
	releaseHeld := holdSlots(t, srv, 1)

	done := make(chan int, 1)
	go func() { done <- doGateSearch(srv, context.Background(), http.MethodGet).Code }()
	time.Sleep(600 * time.Millisecond) // timeout 보다 길게, searchGateWait 보다 짧게
	releaseHeld()

	if code := <-done; code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if rem := time.Duration(g.remaining.Load()); rem < timeout/2 {
		t.Errorf("searcher got %v of its %v budget; the slot wait was charged to the search timeout", rem, timeout)
	}
}

type deadlineRecordingSearcher struct{ remaining atomic.Int64 }

func (d *deadlineRecordingSearcher) Search(ctx context.Context, _ model.SearchQuery) ([]*model.SearchResult, error) {
	if dl, ok := ctx.Deadline(); ok {
		d.remaining.Store(int64(time.Until(dl)))
	}
	return nil, nil
}

// GraphQL 검색 5개는 K=1 에서도 모두 성공한다 — 필드가 순차로 슬롯을 잡았다
// 놓으므로 한 요청이 자기 자신을 막지 않는다(데드락·헛 503 없음).
func TestSearchGate_GraphQL_FiveSearchesShareOneSlot(t *testing.T) {
	t.Parallel()

	docs := &callCountingSearcher{}
	srv := newSearchInputTestServer(docs).WithSearchConcurrency(1)

	start := time.Now()
	w := postGraphQL(t, srv, map[string]any{"query": gqlAliasDoc(graphqlMaxSearchFields)})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"errors"`) {
		t.Errorf("body has errors: %s", w.Body.String())
	}
	if got := docs.calls.Load(); got != graphqlMaxSearchFields {
		t.Errorf("searcher called %d times, want %d", got, graphqlMaxSearchFields)
	}
	if elapsed := time.Since(start); elapsed > searchGateWait/2 {
		t.Errorf("took %v; fields waited on a slot their own request held", elapsed)
	}
}

// GraphQL 검색이 슬롯을 못 얻으면 HTTP 200 + errors[] 의 고정 문구다.
func TestSearchGate_GraphQL_BusyIsFieldError(t *testing.T) {
	t.Parallel()

	docs := &callCountingSearcher{}
	srv := newSearchInputTestServer(docs).WithSearchConcurrency(1)
	defer holdSlots(t, srv, 1)()

	w := postGraphQL(t, srv, map[string]any{"query": gqlAliasDoc(1)})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (GraphQL field error); body = %s", w.Code, w.Body.String())
	}
	var body struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body = %s", err, w.Body.String())
	}
	if len(body.Errors) != 1 || body.Errors[0].Message != errSearchBusy.Error() {
		t.Errorf("errors = %+v, want one %q", body.Errors, errSearchBusy.Error())
	}
	if got := docs.calls.Load(); got != 0 {
		t.Errorf("searcher called %d times, want 0", got)
	}
}

// golden/next 도 REST 와 같은 503 + Retry-After 다.
func TestSearchGate_GoldenNextBusyReturns503(t *testing.T) {
	t.Parallel()

	docs := &callCountingSearcher{}
	stub := &stubGoldenSet{nextQuery: &store.GoldenQuery{ID: uuid.New(), Text: "gate probe", Status: "open"}}
	srv := newGoldenTestServer(stub, docs).WithSearchConcurrency(1)
	defer holdSlots(t, srv, 1)()

	start := time.Now()
	rec := doGoldenRequest(srv, http.MethodGet, "/api/v1/golden/next", nil)
	assertBusy503(t, rec, time.Since(start))
	if got := docs.calls.Load(); got != 0 {
		t.Errorf("searcher called %d times, want 0", got)
	}
}

// /ask 는 503 없이 ask ctx 안에서 슬롯을 기다렸다가 정상 진행한다.
func TestSearchGate_AskWaitsForSlotThenSucceeds(t *testing.T) {
	t.Parallel()

	searcher := &fixedDocSearcher{observed: []*model.SearchResult{docResult(model.SourceSMS, "문자 내용")}}
	fakeLLM := &fakeAskLLM{enabled: true, chunks: []string{"답"}}
	srv := newAskTestServer(searcher, &fakeIntentClassifier{}, fakeLLM).WithSearchConcurrency(1)
	releaseHeld := holdSlots(t, srv, 1)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doAskRequest(t, srv, nil, map[string]any{"question": "질문"}, "Bearer test-key")
	}()

	// REST 의 대기 상한(searchGateWait)을 넘겨도 /ask 는 포기하지 않는다.
	select {
	case rr := <-done:
		t.Fatalf("ask finished while the only search slot was held: %s", rr.Body.String())
	case <-time.After(searchGateWait + 500*time.Millisecond):
	}
	releaseHeld()

	var rr *httptest.ResponseRecorder
	select {
	case rr = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ask did not proceed after the slot was released")
	}
	events := frameEvents(parseSSEFrames(t, rr.Body.String()))
	want := []string{"conversation", "sources", "token", "done"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v, want %v", events, want)
	}
	if len(searcher.gotQueries) == 0 {
		t.Error("ask never reached the searcher")
	}
}

// ask 타임아웃이 슬롯 대기 중에 끝나면 기존 retrieval failed 경로로 끝난다.
func TestSearchGate_AskTimeoutWhileWaiting(t *testing.T) {
	t.Parallel()

	searcher := &fixedDocSearcher{observed: []*model.SearchResult{docResult(model.SourceSMS, "문자 내용")}}
	fakeLLM := &fakeAskLLM{enabled: true, chunks: []string{"답"}}
	srv := newAskTestServer(searcher, &fakeIntentClassifier{}, fakeLLM).
		WithAskConfig(300*time.Millisecond, 0, 0).
		WithSearchConcurrency(1)
	defer holdSlots(t, srv, 1)()

	rr := doAskRequest(t, srv, nil, map[string]any{"question": "질문"}, "Bearer test-key")

	frames := parseSSEFrames(t, rr.Body.String())
	events := frameEvents(frames)
	if strings.Join(events, ",") != "conversation,error,done" {
		t.Fatalf("events = %v, want [conversation error done]", events)
	}
	if !strings.Contains(frames[1].data, "retrieval failed") {
		t.Errorf("error event = %s, want the retrieval failed message", frames[1].data)
	}
	if len(searcher.gotQueries) != 0 {
		t.Errorf("searcher saw %d queries, want 0 while no slot was free", len(searcher.gotQueries))
	}
	if fakeLLM.streamCalls != 0 {
		t.Errorf("LLM stream called %d times, want 0", fakeLLM.streamCalls)
	}
}

// /ask 대기자가 쌓여 있어도 REST 는 그 뒤에 줄 서지 않는다: 슬롯이 돌기 시작하면
// REST 가 먼저 받는다(deep-verify R1 재현을 옮김 — 수정 전에는 K=1, /ask 검색
// 2건 대기 뒤 REST 가 1.004초 만에 503 이었다).
func TestSearchGate_AskBacklogDoesNotStarveREST(t *testing.T) {
	t.Parallel()

	docs := &callCountingSearcher{}
	srv := newSearchInputTestServer(docs).WithSearchConcurrency(1)
	releaseHolder := holdSlots(t, srv, 1)

	// /ask 검색 2건이 슬롯을 기다린다(각 600ms 실행).
	askSearch := gatedSearcher{inner: sleepSearcher{d: 600 * time.Millisecond}, gate: srv.searchGate}
	askCtx, cancelAsk := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelAsk()
	askErrs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := askSearch.Search(askCtx, model.SearchQuery{Query: "ask"})
			askErrs <- err
		}()
	}
	time.Sleep(50 * time.Millisecond)
	go func() { time.Sleep(100 * time.Millisecond); releaseHolder() }()

	start := time.Now()
	w := doGateSearch(srv, context.Background(), http.MethodGet)
	elapsed := time.Since(start)
	if w.Code != http.StatusOK {
		t.Fatalf("REST status = %d after %v, want 200 — queued /ask searches starved REST while the slot was cycling", w.Code, elapsed)
	}
	if elapsed > searchGateWait/2 {
		t.Errorf("REST waited %v; it should get the slot as soon as the holder releases (~100ms)", elapsed)
	}
	// /ask 검색도 결국 성공한다(각자 대기 상한 안에서).
	for i := 0; i < 2; i++ {
		if err := <-askErrs; err != nil {
			t.Errorf("ask search %d: %v", i, err)
		}
	}
}

type sleepSearcher struct{ d time.Duration }

func (s sleepSearcher) Search(ctx context.Context, _ model.SearchQuery) ([]*model.SearchResult, error) {
	select {
	case <-time.After(s.d):
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// /ask 는 askTimeout(기본 60초)이 아니라 검색당 대기 상한에서 멈추고 기존
// retrieval failed 흐름으로 끝난다. 운영 상한은 10초라 테스트에서만 줄인다.
func TestSearchGate_AskWaitIsCapped(t *testing.T) {
	t.Parallel()

	searcher := &fixedDocSearcher{observed: []*model.SearchResult{docResult(model.SourceSMS, "문자 내용")}}
	fakeLLM := &fakeAskLLM{enabled: true, chunks: []string{"답"}}
	srv := newAskTestServer(searcher, &fakeIntentClassifier{}, fakeLLM).WithSearchConcurrency(1)
	if srv.searchGate.askWait != askSearchWaitMax {
		t.Fatalf("default ask wait = %v, want askSearchWaitMax %v", srv.searchGate.askWait, askSearchWaitMax)
	}
	srv.searchGate.askWait = 300 * time.Millisecond
	defer holdSlots(t, srv, 1)()

	done := make(chan *httptest.ResponseRecorder, 1)
	start := time.Now()
	go func() { done <- doAskRequest(t, srv, nil, map[string]any{"question": "질문"}, "Bearer test-key") }()

	var rr *httptest.ResponseRecorder
	select {
	case rr = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ask kept waiting past its per-search wait cap")
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Errorf("ask gave up after %v, want it to wait ~300ms first", elapsed)
	}
	frames := parseSSEFrames(t, rr.Body.String())
	if got := strings.Join(frameEvents(frames), ","); got != "conversation,error,done" {
		t.Fatalf("events = %s, want conversation,error,done", got)
	}
	if !strings.Contains(frames[1].data, "retrieval failed") {
		t.Errorf("error event = %s, want the retrieval failed message", frames[1].data)
	}
	if len(searcher.gotQueries) != 0 {
		t.Errorf("searcher saw %d queries, want 0", len(searcher.gotQueries))
	}
}

// 대기 단계에서 요청 기한이 끝나면(GraphQL 요청 단위 기한) 슬롯 부족이나 일반
// 오류가 아니라 errSearchTimeout 이다(deep-verify R2).
func TestSearchGate_DeadlineWhileWaitingIsTimeout(t *testing.T) {
	t.Parallel()

	docs := &callCountingSearcher{}
	srv := newSearchInputTestServer(docs).WithSearchConcurrency(1)
	defer holdSlots(t, srv, 1)()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := srv.searchWithTimeout(ctx, model.SearchQuery{Query: "x"})
	if !errors.Is(err, errSearchTimeout) || errors.Is(err, errSearchBusy) {
		t.Errorf("err = %v, want errSearchTimeout (deadline hit while waiting for a slot)", err)
	}
	if got := docs.calls.Load(); got != 0 {
		t.Errorf("searcher called %d times, want 0", got)
	}
}

// GraphQL 경로 전체: 요청 기한이 슬롯 대기 중 끝나면 리졸버는 Warn
// "graphql: search timed out" 을 남기고 slog.Error 를 남기지 않는다.
//
// t.Parallel 을 쓰지 않는다: slog 기본 로거를 바꿔 확인한다(병렬 테스트는 이
// 테스트가 끝난 뒤 시작된다).
func TestSearchGate_GraphQLDeadlineWhileWaiting_LogsTimeoutNotError(t *testing.T) {
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	docs := &callCountingSearcher{}
	srv := newSearchInputTestServer(docs).WithSearchTimeout(300 * time.Millisecond).WithSearchConcurrency(1)
	defer holdSlots(t, srv, 1)()

	_ = postGraphQL(t, srv, map[string]any{"query": gqlAliasDoc(1)})

	// graphql-go 는 요청 ctx 가 끝나면 실행 goroutine 을 기다리지 않고 돌아오므로,
	// 리졸버의 로그가 찍힐 때까지 기다린다.
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(logs.String(), "graphql: search timed out") {
		if time.Now().After(deadline) {
			t.Fatalf("resolver never logged the timeout; logs:\n%s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("a deadline while waiting for a slot was logged as an error:\n%s", logs.String())
	}
	if got := docs.calls.Load(); got != 0 {
		t.Errorf("searcher called %d times, want 0", got)
	}
}
