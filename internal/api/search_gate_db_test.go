package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// #286 항목 3 실DB 대조 실험: 검색 게이트가 실제로 ingest 의 풀 연결을 지키는가.
//
// 풀 MaxConns=4 로 만들고, 같은 풀에서 pg_sleep 을 실행해 연결을 붙잡는 검색기를
// 실제 검색 핸들러 뒤에 둔다. 검색 6건을 동시에 보내 레인이 연결을 쥔 상태에서
// 실제 ingest/messages 요청(실 DocumentStore·ChunkStore) 1건을 보낸다.
//
//   - 대조군(게이트 없음 = 이 변경 전 동작): 검색이 연결 4개를 모두 쥐어 ingest 는
//     연결을 못 얻고 게이트웨이 대용 데드라인(ingestGatewayDeadline)까지 막힌 뒤
//     저장에 실패한다 — 결함 재현. #290 이후 ingest 는 이 실패를 일시 오류로
//     분류해 503 + Retry-After 로 돌려준다(이전에는 201 + accepted 0 이라 폰이
//     커서를 전진해 레코드를 잃었다). 그래서 대조군의 기대 상태는 503 이다.
//   - 실험군(K=2, 자동값 max(1, 4/2)): 검색은 연결 2개까지만 쓰고 나머지 4건은
//     1초 뒤 503, ingest 는 1초 안에 저장된다.
//
// 검색 레인 수(pg_sleep 중인 백엔드 수)는 풀 밖의 별도 연결로 pg_stat_activity
// 에서 센다 — 풀이 가득 찬 대조군에서도 관측이 막히지 않게 하려는 것이다.
//
// TEST_DATABASE_URL 이 없으면 건너뛴다. 운영 DB 를 가리키면 안 된다 —
// 마이그레이션을 적용하고 테스트 행을 쓴다(끝나면 표식으로 지운다).

const (
	gateDBPoolMaxConns    = 4
	gateDBSearches        = 6
	gateDBSleep           = 5 * time.Second
	ingestGatewayDeadline = 3 * time.Second
	gateDBSleepMarker     = "zz_pr286_gate_sleep"
)

// sleepingSearcher 는 검색 한 건이 레인 동안 풀 연결 하나를 쥐는 모양을 그대로
// 흉내 낸다: 같은 풀에서 pg_sleep 을 실행한다.
type sleepingSearcher struct {
	pool *pgxpool.Pool
}

func (s sleepingSearcher) Search(ctx context.Context, _ model.SearchQuery) ([]*model.SearchResult, error) {
	_, err := s.pool.Exec(ctx, "SELECT pg_sleep($1) /* "+gateDBSleepMarker+" */", gateDBSleep.Seconds())
	return nil, err
}

// withPoolMaxConns 는 DSN 에 pool_max_conns 를 덧붙인다(URL·키값 형식 모두).
func withPoolMaxConns(t *testing.T, dsn string, n int) string {
	t.Helper()
	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("parse TEST_DATABASE_URL: invalid URL")
		}
		q := u.Query()
		q.Set("pool_max_conns", strconv.Itoa(n))
		u.RawQuery = q.Encode()
		return u.String()
	}
	return dsn + " pool_max_conns=" + strconv.Itoa(n)
}

type gateScenarioResult struct {
	ingestStatus   int
	ingestRetry    string
	ingestLatency  time.Duration
	ingestAccepted int
	ingestErrors   int
	maxSleeping    int
	searchCodes    map[int]int
}

func TestSearchGate_IngestProtection_RealDB(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-database search gate test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	pg, err := store.NewPostgres(ctx, withPoolMaxConns(t, dsn, gateDBPoolMaxConns))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pg.Close)
	if got := pg.MaxConns(); got != gateDBPoolMaxConns {
		t.Fatalf("pool MaxConns = %d, want %d (pool_max_conns not applied)", got, gateDBPoolMaxConns)
	}
	if err := pg.RunMigrations(ctx, filepath.Join("..", "..", "migrations"), 1536); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	monitor, err := pgx.Connect(ctx, dsn) // 풀 밖의 관측 전용 연결
	if err != nil {
		t.Fatalf("monitor connect: %v", err)
	}
	t.Cleanup(func() { _ = monitor.Close(context.Background()) })

	// 표식은 숫자 없이 만든다. SMS 매핑의 PII 마스킹이 본문 안의 긴 숫자열을
	// [REDACTED] 로 바꿔 uuid 가 끊기면 정리 쿼리(LIKE 표식)가 행을 놓친다(실측).
	marker := "zz-pr286-gate-" + strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9':
			return 'g' + (r - '0')
		case r == '-':
			return -1
		}
		return r
	}, uuid.NewString())
	t.Cleanup(func() {
		if _, err := monitor.Exec(context.Background(),
			`DELETE FROM documents WHERE source_type = 'sms' AND content LIKE $1`, "%"+marker+"%"); err != nil {
			t.Errorf("cleanup documents: %v", err)
		}
	})

	k, _ := SearchConcurrency(pg.MaxConns(), 0)
	if k != 2 {
		t.Fatalf("automatic K for MaxConns=4 = %d, want 2", k)
	}

	control := runGateScenario(t, pg, monitor, marker+"-control", 0)
	gated := runGateScenario(t, pg, monitor, marker+"-gated", k)

	t.Logf("control (no gate): ingest %d in %v accepted=%d errors=%d, max concurrent search backends=%d, search statuses=%v",
		control.ingestStatus, control.ingestLatency.Round(time.Millisecond), control.ingestAccepted, control.ingestErrors, control.maxSleeping, control.searchCodes)
	t.Logf("gated (K=%d):      ingest %d in %v accepted=%d errors=%d, max concurrent search backends=%d, search statuses=%v",
		k, gated.ingestStatus, gated.ingestLatency.Round(time.Millisecond), gated.ingestAccepted, gated.ingestErrors, gated.maxSleeping, gated.searchCodes)

	// 대조군: 결함이 재현돼야 이 실험이 의미가 있다.
	if control.maxSleeping != gateDBPoolMaxConns {
		t.Errorf("control: %d search backends held connections, want the whole pool (%d)", control.maxSleeping, gateDBPoolMaxConns)
	}
	if control.ingestAccepted != 0 || control.ingestLatency < ingestGatewayDeadline-200*time.Millisecond {
		t.Errorf("control: ingest accepted=%d after %v; want it starved until the %v deadline (defect not reproduced)",
			control.ingestAccepted, control.ingestLatency, ingestGatewayDeadline)
	}
	// #290: 저장에 실패한 배치는 201 이 아니라 503 + Retry-After 여야 폰이
	// 커서를 멈추고 다시 보낸다.
	if control.ingestStatus != http.StatusServiceUnavailable || control.ingestRetry == "" {
		t.Errorf("control: ingest status=%d Retry-After=%q, want 503 with Retry-After (a failed batch must not be 201)",
			control.ingestStatus, control.ingestRetry)
	}

	// 실험군: 검색은 K 개 연결까지만, ingest 는 제시간에 저장.
	if gated.maxSleeping > k {
		t.Errorf("gated: %d search backends held connections, want <= K=%d", gated.maxSleeping, k)
	}
	if gated.ingestStatus != http.StatusCreated {
		t.Errorf("gated: ingest status=%d, want 201", gated.ingestStatus)
	}
	if gated.ingestAccepted != 1 || gated.ingestErrors != 0 {
		t.Errorf("gated: ingest accepted=%d errors=%d, want 1 and 0", gated.ingestAccepted, gated.ingestErrors)
	}
	if gated.ingestLatency > time.Second {
		t.Errorf("gated: ingest took %v, want < 1s", gated.ingestLatency)
	}
	if got := gated.searchCodes[http.StatusServiceUnavailable]; got != gateDBSearches-k {
		t.Errorf("gated: %d searches got 503, want %d (N-K)", got, gateDBSearches-k)
	}
}

// runGateScenario 는 검색 gateDBSearches 건으로 풀을 압박한 상태에서 ingest 1건의
// 지연·결과를 잰다. k == 0 이면 게이트 없이(변경 전 동작) 돈다.
func runGateScenario(t *testing.T, pg *store.Postgres, monitor *pgx.Conn, marker string, k int) gateScenarioResult {
	t.Helper()

	docStore := store.NewDocumentStore(pg)
	srv := NewServer(nil, search.NewService(sleepingSearcher{pool: pg.Pool()}, askDisabledEmbedder{}), nil, nil, nil, "", "").
		WithIngestMessages(docStore, 0, time.Time{})
	if k > 0 {
		srv = srv.WithSearchConcurrency(k)
	}
	h := srv.Handler()

	searchCtx, cancelSearches := context.WithCancel(context.Background())
	defer cancelSearches()

	codes := make(chan int, gateDBSearches)
	var wg sync.WaitGroup
	for i := 0; i < gateDBSearches; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=gate+probe", nil).WithContext(searchCtx)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			codes <- w.Code
		}()
	}

	// 검색 레인이 연결을 쥘 때까지 기다린다.
	want := gateDBPoolMaxConns
	if k > 0 {
		want = k
	}
	maxSleeping := 0
	deadline := time.Now().Add(10 * time.Second)
	for {
		n := countSleeping(t, monitor)
		maxSleeping = max(maxSleeping, n)
		if n >= want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d search backends started, want %d", n, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // 나머지 검색이 게이트·풀 대기열에 들어가게 둔다

	// ingest 1건 — 폰 push 와 같은 경로. 게이트웨이 타임아웃 대용 데드라인을 건다.
	body, _ := json.Marshal(map[string]any{"sms": []map[string]any{{
		"address": "zz-gate-test",
		"body":    marker,
		"date_ms": time.Now().UnixMilli(),
		"type":    1,
	}}})
	ingestCtx, cancelIngest := context.WithTimeout(context.Background(), ingestGatewayDeadline)
	defer cancelIngest()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/messages", bytes.NewReader(body)).WithContext(ingestCtx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(w, req)
	latency := time.Since(start)

	// 상태 코드 판정은 호출자가 한다(대조군 503, 실험군 201).
	var resp IngestMessagesResponse
	if w.Code == http.StatusCreated {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("ingest response: %v", err)
		}
	}
	maxSleeping = max(maxSleeping, countSleeping(t, monitor))

	// 게이트가 있으면 초과분(N-K)은 1초 대기 후 503 으로 이미 돌아온다 — 그 수를
	// 세려고 잠깐 기다렸다가 나머지(pg_sleep 중)를 취소한다. 취소하면 pgx 가
	// 서버 측 문장도 멈춘다.
	result := gateScenarioResult{
		ingestStatus:   w.Code,
		ingestRetry:    w.Header().Get("Retry-After"),
		ingestLatency:  latency,
		ingestAccepted: resp.Accepted,
		ingestErrors:   len(resp.Errors),
		searchCodes:    map[int]int{},
	}
	if k > 0 {
		for i := 0; i < gateDBSearches-k; i++ {
			select {
			case c := <-codes:
				result.searchCodes[c]++
			case <-time.After(5 * time.Second):
				t.Errorf("gated: only %d over-limit searches returned", i)
			}
		}
	}
	maxSleeping = max(maxSleeping, countSleeping(t, monitor))
	cancelSearches()
	wg.Wait()
	close(codes)
	for c := range codes {
		result.searchCodes[c]++ // 취소된 나머지(pg_sleep 중이던 검색)는 500
	}
	result.maxSleeping = maxSleeping

	// 다음 시나리오 전에 서버 측 pg_sleep 이 모두 끝났는지 확인한다.
	deadline = time.Now().Add(10 * time.Second)
	for countSleeping(t, monitor) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("search backends still sleeping after cancel")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return result
}

func countSleeping(t *testing.T, monitor *pgx.Conn) int {
	t.Helper()
	var n int
	if err := monitor.QueryRow(context.Background(), `
		SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database()
		  AND state = 'active'
		  AND pid <> pg_backend_pid()
		  AND query LIKE '%' || $1 || '%'`, gateDBSleepMarker).Scan(&n); err != nil {
		t.Fatalf("pg_stat_activity: %v", err)
	}
	return n
}
