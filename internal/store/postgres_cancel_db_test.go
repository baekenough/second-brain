package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/baekenough/second-brain/internal/model"
)

// ---------------------------------------------------------------------------
// #282: ctx 타임아웃이 서버 측 문장까지 실제로 취소하는지, 그리고 검증 없이
// NUL·잘못된 UTF-8 을 바인딩하면 DB 가 22021 로 실패하는지를 실제
// PostgreSQL 로 확인한다. TEST_DATABASE_URL 이 없으면 건너뛴다.
// ---------------------------------------------------------------------------

func cancelTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-database cancel test")
	}
	return dsn
}

// activeProbeCount 는 marker 를 담은 문장 중 아직 실행 중인 것의 수를 센다.
// 자기 자신(이 조회 문장도 marker 를 담는다)은 pid 로 뺀다.
func activeProbeCount(t *testing.T, pg *Postgres, marker string) int {
	t.Helper()
	var n int
	err := pg.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM pg_stat_activity
		WHERE state = 'active' AND pid <> pg_backend_pid() AND query LIKE '%' || $1 || '%'`,
		marker,
	).Scan(&n)
	if err != nil {
		t.Fatalf("pg_stat_activity: %v", err)
	}
	return n
}

// waitProbeCount 는 실행 중인 marker 문장 수가 want 가 될 때까지 최대 wait
// 동안 기다리고 마지막으로 본 값을 돌려준다.
func waitProbeCount(t *testing.T, pg *Postgres, marker string, want int, wait time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		n := activeProbeCount(t, pg, marker)
		if n == want || time.Now().After(deadline) {
			return n
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestPool_ContextTimeoutCancelsServerSide 는 검색 핸들러의 context 타임아웃이
// 호출자만 돌려보내는 것이 아니라 서버의 문장까지 멈추는지 확인한다(#282).
//
// pgx v5.9.2 기본 ctxwatch 핸들러(DeadlineContextWatcherHandler)는 ctx 가
// 끝나면 소켓 데드라인으로 호출자를 즉시 돌려보내고, 연결을 닫는 asyncClose
// 안에서 PostgreSQL 취소 요청(CancelRequest)을 보낸다. 그래서 풀 설정을 바꾸지
// 않아도 서버 측 문장이 취소된다 — 대신 그 연결은 버려지고 풀이 새로 맺는다.
// 이 테스트는 그 동작을 고정해, pgx 업그레이드로 기본값이 바뀌면 드러나게 한다.
func TestPool_ContextTimeoutCancelsServerSide(t *testing.T) {
	dsn := cancelTestDSN(t)
	pg, err := NewPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pg.Close)

	// 양성 대조군: 탐지 쿼리가 실제로 실행 중인 문장을 볼 수 있어야 아래의
	// "0건" 결과가 "취소됨"을 뜻한다. 기한 없는 ctx 로 돌려 두고 수동으로 끊는다.
	controlMarker := "cancel-control-" + uuid.NewString()
	controlCtx, stopControl := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = pg.pool.Exec(controlCtx, "SELECT /* "+controlMarker+" */ pg_sleep(30)")
	}()
	if n := waitProbeCount(t, pg, controlMarker, 1, 3*time.Second); n != 1 {
		stopControl()
		<-done
		t.Fatalf("control: detector saw %d running probe statement(s), want 1", n)
	}
	stopControl()
	<-done

	marker := "cancel-probe-" + uuid.NewString()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = pg.pool.Exec(ctx, "SELECT /* "+marker+" */ pg_sleep(30)")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("pg_sleep(30) under a 300ms ctx returned no error")
	}
	if elapsed > 5*time.Second {
		t.Errorf("caller waited %v; the ctx deadline did not bound the query", elapsed)
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Errorf("ctx.Err() = %v, want DeadlineExceeded", ctx.Err())
	}
	if n := waitProbeCount(t, pg, marker, 0, 3*time.Second); n != 0 {
		t.Errorf("%d probe statement(s) still active on the server after the ctx deadline", n)
	}
}

// TestSearch_InvalidTextParamFailsWith22021 은 입력 검증이 막는 실패를
// 실제 DB 로 재현한다: 검증 없이 NUL·잘못된 UTF-8 을 검색 질의로 바인딩하면
// 문서 검색 전체가 SQLSTATE 22021 로 끝난다(#282 의 원인).
func TestSearch_InvalidTextParamFailsWith22021(t *testing.T) {
	dsn := cancelTestDSN(t)
	pg, err := NewPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pg.Close)
	docs := NewDocumentStore(pg)

	for name, q := range map[string]string{"nul": "a\x00b", "invalid_utf8": "a\xffb"} {
		t.Run(name, func(t *testing.T) {
			_, err := docs.Search(context.Background(), model.SearchQuery{Query: q, Limit: 5})
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "22021" {
				t.Fatalf("Search() error = %v, want SQLSTATE 22021", err)
			}
		})
	}

	// 양성 대조군: 같은 경로가 정상 질의에는 오류 없이 돈다.
	if _, err := docs.Search(context.Background(), model.SearchQuery{Query: "zzdummy", Limit: 5}); err != nil {
		t.Fatalf("control: valid query failed: %v", err)
	}
}
