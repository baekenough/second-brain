package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
)

// ---------------------------------------------------------------------------
// #282 실DB 통합 확인. 가짜 검색기가 아니라 실제 DocumentStore(pgx → PostgreSQL)
// 뒤에 붙은 핸들러로, NUL·잘못된 UTF-8 이 22021(→500) 없이 400 이 되고 정상
// 질의는 실제 SQL 을 거쳐 200 이 되는지, 타임아웃이 실제 pgx 오류 모양에서도
// 504 로 분류되는지를 본다. TEST_DATABASE_URL 이 없으면 건너뛴다. 운영 DB 를
// 가리키면 안 된다 — 마이그레이션을 적용한다.
// ---------------------------------------------------------------------------

func newRealDBSearchServer(t *testing.T) *Server {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-database search handler test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pg, err := store.NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pg.Close)
	if err := pg.RunMigrations(ctx, filepath.Join("..", "..", "migrations"), 1536); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	svc := search.NewService(store.NewDocumentStore(pg), askDisabledEmbedder{})
	return NewServer(nil, svc, nil, nil, nil, "", "")
}

func TestSearchHandlers_RealDB(t *testing.T) {
	srv := newRealDBSearchServer(t)

	t.Run("post_nul_is_400_not_22021", func(t *testing.T) {
		body := []byte(`{"query":"` + searchLeakMarker + `\u0000x"}`)
		w := httptest.NewRecorder()
		srv.searchHandler(w, httptest.NewRequest(http.MethodPost, "/api/v1/search", bytes.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
		}
		assertNoLeak(t, "response body", w.Body.String())
	})

	t.Run("get_invalid_utf8_is_400_not_22021", func(t *testing.T) {
		w := httptest.NewRecorder()
		srv.searchGetHandler(w, httptest.NewRequest(http.MethodGet, "/api/v1/search?q="+searchLeakMarker+"%FF", nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
		}
		assertNoLeak(t, "response body", w.Body.String())
	})

	// 양성 대조군: 같은 서버가 정상 질의에는 실제 SQL 을 돌려 200 을 준다.
	// 이게 실패하면 위 400 은 검증이 아니라 다른 고장 때문일 수 있다.
	t.Run("valid_query_is_200", func(t *testing.T) {
		w := httptest.NewRecorder()
		srv.searchGetHandler(w, httptest.NewRequest(http.MethodGet, "/api/v1/search?q=zzdummy", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
	})

	// 실제 pgx 가 돌려주는 타임아웃 오류 모양으로도 504 가 나오는지 본다.
	t.Run("expired_timeout_is_504", func(t *testing.T) {
		fast := newRealDBSearchServer(t).WithSearchTimeout(time.Nanosecond)
		w := httptest.NewRecorder()
		fast.searchGetHandler(w, httptest.NewRequest(http.MethodGet, "/api/v1/search?q="+searchLeakMarker, nil))
		if w.Code != http.StatusGatewayTimeout {
			t.Fatalf("status = %d, want 504; body = %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "search timed out") {
			t.Errorf("body = %s, want the timeout message", w.Body.String())
		}
		assertNoLeak(t, "response body", w.Body.String())
	})
}
