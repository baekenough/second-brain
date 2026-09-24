package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
)

// ---------------------------------------------------------------------------
// #282: MCP search 도구의 입력 검증과 검색 타임아웃.
//
// REST 핸들러와 같은 공통 검증(search.ValidateInputText)을 거치는지, 그리고
// 도구 오류 문구에 질의 원문이 섞이지 않는지를 실제 registerSearchTool
// 핸들러 경로로 확인한다.
// ---------------------------------------------------------------------------

// leakMarker 는 응답에 질의 원문이 새는지 확인하려고 질의에 섞는 표식이다.
const leakMarker = "zq-leak-marker-282"

// countingDocSearcher 는 호출 횟수만 센다. 검증에 걸린 입력이 검색까지
// 내려가지 않았음을 확인하는 데 쓴다.
type countingDocSearcher struct{ calls atomic.Int32 }

func (c *countingDocSearcher) Search(context.Context, model.SearchQuery) ([]*model.SearchResult, error) {
	c.calls.Add(1)
	return nil, nil
}

// blockingDocSearcher 는 ctx 가 끝날 때까지 막혀 있다가 err 를 돌려준다.
// err 가 nil 이면 ctx.Err() 를 돌려준다. err 를 주는 경우는 context 오류를
// 감싸지 않은 채 올라오는 레인 오류(예: 57014 PgError)를 흉내 낸다.
type blockingDocSearcher struct{ err error }

func (b blockingDocSearcher) Search(ctx context.Context, _ model.SearchQuery) ([]*model.SearchResult, error) {
	<-ctx.Done()
	if b.err != nil {
		return nil, b.err
	}
	return nil, ctx.Err()
}

func newInputTestServer(docs search.DocumentSearcher, timeout time.Duration) *mcpserver.MCPServer {
	svc := search.NewService(docs, disabledEmbeddingEngine{})
	s := mcpserver.NewMCPServer("test", "0.0.0", mcpserver.WithToolCapabilities(false))
	registerSearchTool(s, svc, false, timeout)
	return s
}

func TestSearchTool_RejectsInvalidQueryInput(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"nul":          leakMarker + "\x00tail",
		"invalid_utf8": leakMarker + "\xff\xfe",
		"too_long":     leakMarker + strings.Repeat("가", search.MaxQueryBytes/3+1),
	}
	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			docs := &countingDocSearcher{}
			s := newInputTestServer(docs, time.Second)

			result := callTool(t, s, "search", authorizedCtx(), map[string]any{"query": query})

			if !isErrorResult(result) {
				t.Fatalf("expected IsError=true, got %q", resultText(result))
			}
			if got := docs.calls.Load(); got != 0 {
				t.Errorf("document searcher called %d times; invalid input must be rejected before search", got)
			}
			if text := resultText(result); strings.Contains(text, leakMarker) {
				t.Errorf("error text leaks the query: %q", text)
			}
		})
	}
}

func TestSearchTool_AcceptsQueryAtLimit(t *testing.T) {
	t.Parallel()

	docs := &countingDocSearcher{}
	s := newInputTestServer(docs, time.Second)
	query := strings.Repeat("a", search.MaxQueryBytes)

	result := callTool(t, s, "search", authorizedCtx(), map[string]any{"query": query})

	if isErrorResult(result) {
		t.Fatalf("query of exactly MaxQueryBytes rejected: %q", resultText(result))
	}
	if got := docs.calls.Load(); got != 1 {
		t.Errorf("document searcher called %d times, want 1", got)
	}
}

func TestSearchTool_TimesOut(t *testing.T) {
	t.Parallel()

	cases := map[string]error{
		"context_error":   nil,
		"unwrapped_57014": errors.New("ERROR: canceling statement due to user request (SQLSTATE 57014)"),
	}
	for name, searchErr := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newInputTestServer(blockingDocSearcher{err: searchErr}, 50*time.Millisecond)

			start := time.Now()
			result := callTool(t, s, "search", authorizedCtx(), map[string]any{"query": leakMarker})
			elapsed := time.Since(start)

			if !isErrorResult(result) {
				t.Fatalf("expected IsError=true, got %q", resultText(result))
			}
			text := resultText(result)
			if text != "search timed out" {
				t.Errorf("error text = %q, want %q", text, "search timed out")
			}
			if strings.Contains(text, leakMarker) {
				t.Errorf("error text leaks the query: %q", text)
			}
			if elapsed > 5*time.Second {
				t.Errorf("search took %v; the timeout did not bound it", elapsed)
			}
		})
	}
}
