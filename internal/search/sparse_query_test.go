package search

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// SEARCH_SPARSE_QUERY(#276) 서비스 배선 검사. 저장소 SQL 은
// internal/store 의 빌더·DB 테스트가, 여기서는 "어느 레인에 무엇이
// 넘어가는가" 만 본다.

// sparseQuestion 의 키워드는 zzsparsealpha, zzsparsebeta 둘이다.
const sparseQuestion = "이번 주 zzsparsealpha zzsparsebeta 알려줘"

// sparseDocSearcher 는 문서 저장소가 받은 질의를 기록한다.
type sparseDocSearcher struct {
	mu      sync.Mutex
	queries []model.SearchQuery
	results []*model.SearchResult
}

func (r *sparseDocSearcher) Search(_ context.Context, q model.SearchQuery) ([]*model.SearchResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = append(r.queries, q)
	return r.results, nil
}

// sparseChunkStore 는 필터 지원 청크 저장소(FilteredChunkSearcher +
// SparseContextSearcher)로, 레인별로 받은 질의를 기록한다.
type sparseChunkStore struct {
	mu       sync.Mutex
	fts      []model.SearchQuery
	ctx      []model.SearchQuery
	vector   []model.SearchQuery
	ftsHits  []store.ChunkSearchResult
	ctxHits  []store.ChunkSearchResult
	vecHits  []store.ChunkSearchResult
	legacyQs []string
}

func (r *sparseChunkStore) SearchFTS(_ context.Context, q string, _ int) ([]store.ChunkSearchResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.legacyQs = append(r.legacyQs, q)
	return r.ftsHits, nil
}

func (r *sparseChunkStore) SearchVector(context.Context, []float32, int) ([]store.ChunkSearchResult, error) {
	return r.vecHits, nil
}

func (r *sparseChunkStore) SearchFTSFiltered(_ context.Context, q model.SearchQuery, _ int) ([]store.ChunkSearchResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fts = append(r.fts, q)
	return r.ftsHits, nil
}

func (r *sparseChunkStore) SearchVectorFiltered(_ context.Context, q model.SearchQuery, _ int) ([]store.ChunkSearchResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.vector = append(r.vector, q)
	return r.vecHits, nil
}

func (r *sparseChunkStore) SearchSparseContextFiltered(_ context.Context, q model.SearchQuery, _ int, _ string) ([]store.ChunkSearchResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ctx = append(r.ctx, q)
	return r.ctxHits, nil
}

// sparseEmbedder 는 임베딩에 들어간 텍스트를 기록한다.
type sparseEmbedder struct {
	mu    sync.Mutex
	texts []string
}

func (e *sparseEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.texts = append(e.texts, text)
	return []float32{0.1, 0.2}, nil
}

func (e *sparseEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	return make([][]float32, len(texts)), nil
}
func (e *sparseEmbedder) Enabled() bool  { return true }
func (e *sparseEmbedder) Dimension() int { return 2 }

// sparseReranker 는 리랭커가 받은 질의를 기록하고 순서를 그대로 둔다.
type sparseReranker struct {
	mu      sync.Mutex
	queries []string
}

func (r *sparseReranker) Enabled() bool { return true }
func (r *sparseReranker) Rerank(_ context.Context, query string, docs []string) ([]RerankResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = append(r.queries, query)
	out := make([]RerankResult, len(docs))
	for i := range docs {
		out[i] = RerankResult{Index: i, Score: float64(len(docs) - i)}
	}
	return out, nil
}

type sparseWiringRun struct {
	docs   *sparseDocSearcher
	chunks *sparseChunkStore
	emb    *sparseEmbedder
	rr     *sparseReranker
}

func runSparseWiring(t *testing.T, tune model.SearchTuning, q model.SearchQuery) sparseWiringRun {
	t.Helper()
	run := sparseWiringRun{
		docs: &sparseDocSearcher{results: []*model.SearchResult{
			makeSearchResult(uuid.New(), "primary hit", 0.9),
		}},
		chunks: &sparseChunkStore{
			ftsHits: []store.ChunkSearchResult{makeChunkResult(uuid.New(), 0, 0.5, "chunk hit")},
			ctxHits: []store.ChunkSearchResult{makeChunkResult(uuid.New(), 0, 0.4, "ctx hit")},
		},
		emb: &sparseEmbedder{},
		rr:  &sparseReranker{},
	}
	svc := NewService(run.docs, run.emb).WithChunkStore(run.chunks).WithReranker(run.rr).WithTuning(tune)
	if _, err := svc.Search(context.Background(), q); err != nil {
		t.Fatalf("Search: %v", err)
	}
	return run
}

// TestSearch_SparseQuery_Wiring 는 노브 값별로 키워드가 정확히 의도한
// 레인에만 가는지, 그리고 어떤 값에서도 임베딩·리랭커·문서 저장소가 받는
// q.Query 는 질문 원문 그대로인지 본다.
func TestSearch_SparseQuery_Wiring(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		sparse       string
		chunkSparse  string
		ctxVersion   string
		wantChunk    bool // 청크 희소 레인이 키워드를 받는가
		wantDocStore bool // 문서 저장소가 키워드를 받는가
	}{
		{"raw+fuse", model.SparseQueryRaw, model.ChunkSparseFuse, "", false, false},
		{"chunk+fuse", model.SparseQueryChunk, model.ChunkSparseFuse, "", true, false},
		{"chunk_doc+fuse", model.SparseQueryChunkDoc, model.ChunkSparseFuse, "", true, true},
		{"raw+fuse_ctx", model.SparseQueryRaw, model.ChunkSparseFuseCtx, model.ChunkSparseCtxV1TP, false, false},
		{"chunk+fuse_ctx", model.SparseQueryChunk, model.ChunkSparseFuseCtx, model.ChunkSparseCtxV1TP, true, false},
		{"chunk_doc+fuse_ctx", model.SparseQueryChunkDoc, model.ChunkSparseFuseCtx, model.ChunkSparseCtxV1Full, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tune := model.SearchTuning{SparseQuery: tc.sparse, ChunkSparse: tc.chunkSparse, ChunkSparseCtxVersion: tc.ctxVersion}
			run := runSparseWiring(t, tune, model.SearchQuery{Query: sparseQuestion, Limit: 10, UseRerank: true})

			var chunkQs []model.SearchQuery
			if tc.chunkSparse == model.ChunkSparseFuseCtx {
				chunkQs = run.chunks.ctx
			} else {
				chunkQs = run.chunks.fts
			}
			if len(chunkQs) != 1 {
				t.Fatalf("청크 희소 레인 호출 %d회, want 1", len(chunkQs))
			}
			assertSparseTerms(t, "청크 희소 레인", chunkQs[0].SparseTerms, tc.wantChunk)
			if len(run.docs.queries) != 1 {
				t.Fatalf("문서 저장소 호출 %d회, want 1", len(run.docs.queries))
			}
			assertSparseTerms(t, "문서 저장소", run.docs.queries[0].SparseTerms, tc.wantDocStore)
			// 청크 벡터 레인은 어떤 값에서도 키워드를 받지 않는다.
			for _, vq := range run.chunks.vector {
				assertSparseTerms(t, "청크 벡터 레인", vq.SparseTerms, false)
			}

			// 원문 보존: 저장소·청크 레인의 q.Query, 임베딩 입력, 리랭커 질의.
			for _, q := range append(append([]model.SearchQuery{}, run.docs.queries...), chunkQs...) {
				if q.Query != sparseQuestion {
					t.Errorf("q.Query = %q, want 원문", q.Query)
				}
			}
			if len(run.emb.texts) != 1 || run.emb.texts[0] != sparseQuestion {
				t.Errorf("임베딩 입력 = %q, want [원문]", run.emb.texts)
			}
			if len(run.rr.queries) != 1 || run.rr.queries[0] != sparseQuestion {
				t.Errorf("리랭커 질의 = %q, want [원문]", run.rr.queries)
			}
		})
	}
}

// TestSearch_SparseQuery_FallbackBranch 는 기본 fallback 모드(1차 경로
// 0건일 때만 청크 FTS)에서도 chunk 값이면 키워드가 청크 FTS 로 가는지 본다
// (§9: 폴백 분기에도 적용).
func TestSearch_SparseQuery_FallbackBranch(t *testing.T) {
	t.Parallel()
	chunks := &sparseChunkStore{ftsHits: []store.ChunkSearchResult{makeChunkResult(uuid.New(), 0, 0.5, "chunk hit")}}
	svc := NewService(&sparseDocSearcher{}, disabledEmbedder{}).WithChunkStore(chunks).
		WithTuning(model.SearchTuning{SparseQuery: model.SparseQueryChunk})
	if _, err := svc.Search(context.Background(), model.SearchQuery{Query: sparseQuestion}); err != nil {
		t.Fatal(err)
	}
	if len(chunks.fts) != 1 {
		t.Fatalf("청크 FTS 폴백 호출 %d회, want 1", len(chunks.fts))
	}
	assertSparseTerms(t, "청크 FTS 폴백", chunks.fts[0].SparseTerms, true)
}

// TestSearch_SparseQuery_CallerTermsDiscarded 는 호출자가 SparseTerms 를
// 채워 보내도 서비스가 버리는지 본다. raw 에서 저장소가 키워드를 받으면
// "노브를 켜지 않은 배포의 SQL 은 바이트 동일" 이라는 약속이 깨진다.
func TestSearch_SparseQuery_CallerTermsDiscarded(t *testing.T) {
	t.Parallel()
	injected := model.SparseTerms{TSQuery: "'injected':*", Like: []string{"injected"}}
	run := runSparseWiring(t, model.SearchTuning{ChunkSparse: model.ChunkSparseFuse},
		model.SearchQuery{Query: sparseQuestion, SparseTerms: injected})
	assertSparseTerms(t, "문서 저장소", run.docs.queries[0].SparseTerms, false)
	assertSparseTerms(t, "청크 희소 레인", run.chunks.fts[0].SparseTerms, false)

	run = runSparseWiring(t, model.SearchTuning{SparseQuery: model.SparseQueryChunkDoc, ChunkSparse: model.ChunkSparseFuse},
		model.SearchQuery{Query: sparseQuestion, SparseTerms: injected})
	if strings.Contains(run.docs.queries[0].SparseTerms.TSQuery, "injected") {
		t.Errorf("호출자 키워드가 저장소까지 갔다: %+v", run.docs.queries[0].SparseTerms)
	}
}

// TestSearch_SparseQuery_EmptyExtractionFallsBackToRaw 는 키워드가 하나도
// 남지 않는 질문("뭐 있었지?")이면 저장소가 raw 경로를 타는지 본다.
func TestSearch_SparseQuery_EmptyExtractionFallsBackToRaw(t *testing.T) {
	t.Parallel()
	run := runSparseWiring(t, model.SearchTuning{SparseQuery: model.SparseQueryChunkDoc, ChunkSparse: model.ChunkSparseFuse},
		model.SearchQuery{Query: "뭐 있었지?"})
	assertSparseTerms(t, "문서 저장소", run.docs.queries[0].SparseTerms, false)
	assertSparseTerms(t, "청크 희소 레인", run.chunks.fts[0].SparseTerms, false)
}

// TestSearch_SparseQuery_NoTermsInLogs 는 키워드가 로그에 실리지 않는지
// 본다(개수만). 전역 slog 기본 핸들러를 바꾸므로 병렬로 돌리지 않는다.
func TestSearch_SparseQuery_NoTermsInLogs(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	runSparseWiring(t, model.SearchTuning{SparseQuery: model.SparseQueryChunkDoc, ChunkSparse: model.ChunkSparseFuse},
		model.SearchQuery{Query: sparseQuestion, UseRerank: true})

	out := buf.String()
	if !strings.Contains(out, "sparse query terms extracted") || !strings.Contains(out, "like_terms=2") {
		t.Fatalf("양성 대조군: 개수 로그가 없다 — 검사가 공허하게 통과할 수 있다:\n%s", out)
	}
	for _, term := range []string{"zzsparsealpha", "zzsparsebeta"} {
		if strings.Contains(out, term) {
			t.Errorf("키워드 %q 가 로그에 실렸다:\n%s", term, out)
		}
	}
}

func assertSparseTerms(t *testing.T, lane string, got model.SparseTerms, want bool) {
	t.Helper()
	if !want {
		if got.Active() {
			t.Errorf("%s: 키워드를 받으면 안 되는데 받았다(개수 %d)", lane, len(got.Like))
		}
		return
	}
	if got.TSQuery != "'zzsparsealpha':* | 'zzsparsebeta':*" {
		t.Errorf("%s: TSQuery = %q", lane, got.TSQuery)
	}
	if len(got.Like) != 2 || got.Like[0] != "zzsparsealpha" || got.Like[1] != "zzsparsebeta" {
		t.Errorf("%s: Like = %q", lane, got.Like)
	}
}
