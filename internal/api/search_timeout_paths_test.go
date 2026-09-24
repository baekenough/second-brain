package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/graph"
	"github.com/baekenough/second-brain/internal/model"
)

// #282 보안 리뷰 후속: golden 후보 검색과 그래프 엔티티 검색도 searchTimeout
// 으로 묶이는지 확인한다.

func TestGoldenSearchStream_UsesSearchTimeout(t *testing.T) {
	t.Parallel()

	const timeout = 50 * time.Millisecond
	srv := newSearchInputTestServer(slowSearcher{}).WithSearchTimeout(timeout)

	done := make(chan error, 1)
	go func() {
		_, err := srv.goldenSearchStream(context.Background(), "relevance", model.SearchQuery{Query: "x"})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("goldenSearchStream returned no error for a search that never finishes")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("goldenSearchStream was not bounded by searchTimeout")
	}
}

// deadlineRecordingGraph 는 SearchEntities 가 받은 ctx 에 기한이 있는지 기록한다.
type deadlineRecordingGraph struct {
	GraphReader
	hadDeadline bool
	remaining   time.Duration
}

func (g *deadlineRecordingGraph) SearchEntities(ctx context.Context, _ string, _ int) ([]graph.EntityHit, error) {
	deadline, ok := ctx.Deadline()
	g.hadDeadline = ok
	g.remaining = time.Until(deadline)
	return nil, nil
}

func TestGraphEntitiesHandler_UsesSearchTimeout(t *testing.T) {
	t.Parallel()

	const timeout = 7 * time.Second
	g := &deadlineRecordingGraph{}
	srv := newSearchInputTestServer(&callCountingSearcher{}).WithSearchTimeout(timeout)
	srv.graph = g

	w := httptest.NewRecorder()
	srv.graphEntitiesHandler(w, httptest.NewRequest(http.MethodGet, "/api/v1/graph/entities?q=abc", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if !g.hadDeadline {
		t.Fatal("SearchEntities ctx has no deadline; searchTimeout was not applied")
	}
	if g.remaining > timeout {
		t.Errorf("deadline %v away, want <= searchTimeout %v", g.remaining, timeout)
	}
}
