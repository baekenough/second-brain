package search

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// perDocListerDouble 은 ChunkLister(단건)만 만족하는 더블이다. 문서별로 다른
// 청크를 돌려줄 수 있어야 배치 경로와 같은 fixture 로 비교할 수 있다.
type perDocListerDouble struct {
	byDoc       map[uuid.UUID][]store.Chunk
	singleCalls int
}

func (d *perDocListerDouble) SearchFTS(context.Context, string, int) ([]store.ChunkSearchResult, error) {
	return nil, nil
}

func (d *perDocListerDouble) SearchVector(context.Context, []float32, int) ([]store.ChunkSearchResult, error) {
	return nil, nil
}

func (d *perDocListerDouble) ListByDocument(_ context.Context, id uuid.UUID) ([]store.Chunk, error) {
	d.singleCalls++
	return d.byDoc[id], nil
}

// batchListerDouble 은 단건·배치를 모두 만족한다. 배치 경로가 선택되면
// singleCalls 는 0 이어야 한다.
type batchListerDouble struct {
	perDocListerDouble
	batchErr   error
	batchCalls int
	batchIDs   [][]uuid.UUID
}

func (d *batchListerDouble) ListByDocuments(_ context.Context, ids []uuid.UUID) (map[uuid.UUID][]store.Chunk, error) {
	d.batchCalls++
	d.batchIDs = append(d.batchIDs, slices.Clone(ids))
	if d.batchErr != nil {
		return nil, d.batchErr
	}
	out := make(map[uuid.UUID][]store.Chunk)
	for _, id := range ids {
		if cs, ok := d.byDoc[id]; ok && len(cs) > 0 {
			out[id] = cs // 실제 스토어처럼 청크 없는 문서는 키를 만들지 않는다
		}
	}
	return out, nil
}

// batchFixture 는 선택 규칙의 모든 갈래를 한 번에 밟는 후보 목록이다:
// 겹침 최다 청크, 동점(인덱스 작은 쪽), 청크 없는 문서(head 폴백), 청크 레인
// 결과(재조회 금지), 같은 문서의 중복 등장.
func batchFixture() ([]*model.SearchResult, map[uuid.UUID][]store.Chunk) {
	occurred := time.Date(2026, 9, 1, 13, 30, 0, 0, time.UTC)

	call := tuneResult(uuidN(1), "9월 정기 회의", "인사말과 서명뿐인 앞부분", 1.0)
	call.SourceType = model.SourceCall
	call.OccurredAt = &occurred

	tie := tuneResult(uuidN(2), "동점 메일", "메일 앞부분", 0.9)
	tie.SourceType = model.SourceGmail

	noChunks := tuneResult(uuidN(3), "청크 없는 노트", "노트 본문 그대로", 0.8)
	noChunks.SourceType = model.SourceNote

	chunkHit := tuneResult(uuidN(4), "긴 메일", "이미 고른 청크 본문", 0.7)
	chunkHit.MatchType = "chunk-fts"

	dup := tuneResult(uuidN(1), "9월 정기 회의", "인사말과 서명뿐인 앞부분", 0.6)
	dup.SourceType = model.SourceCall

	chunks := map[uuid.UUID][]store.Chunk{
		uuidN(1): {
			{DocumentID: uuidN(1), ChunkIndex: 0, Content: "안녕하세요 오랜만입니다"},
			{DocumentID: uuidN(1), ChunkIndex: 1, Content: "예산 승인 건은 다음 주에 처리하기로 했습니다"},
			{DocumentID: uuidN(1), ChunkIndex: 2, Content: "그럼 들어가세요"},
		},
		uuidN(2): {
			{DocumentID: uuidN(2), ChunkIndex: 0, Content: "예산 첫 번째 청크"},
			{DocumentID: uuidN(2), ChunkIndex: 1, Content: "예산 두 번째 청크"},
		},
		uuidN(4): {{DocumentID: uuidN(4), ChunkIndex: 0, Content: "쓰이면 안 되는 청크"}},
	}
	return []*model.SearchResult{call, tie, noChunks, chunkHit, dup}, chunks
}

func TestBuildRerankDocs_BatchMatchesPerDocument(t *testing.T) {
	t.Parallel()
	tune := model.SearchTuning{RerankInput: model.RerankInputBestChunk}.Normalized()

	// 빈 질의는 바이그램이 없어 첫 청크를 고르는 갈래, "예산" 은 동점 갈래,
	// "예산 승인" 은 겹침 최다 갈래를 밟는다.
	for _, query := range []string{"예산 승인", "예산", "", "무관한 질의"} {
		t.Run("query="+query, func(t *testing.T) {
			t.Parallel()
			results, chunks := batchFixture()

			single := &perDocListerDouble{byDoc: chunks}
			want := (&Service{chunkStore: single}).buildRerankDocs(context.Background(), query, results, tune)

			batch := &batchListerDouble{perDocListerDouble: perDocListerDouble{byDoc: chunks}}
			got := (&Service{chunkStore: batch}).buildRerankDocs(context.Background(), query, results, tune)

			if !slices.Equal(got, want) {
				t.Fatalf("배치 경로의 리랭크 입력이 단건 경로와 다르다\n got: %q\nwant: %q", got, want)
			}
			// 단건 경로는 청크 레인 결과를 뺀 4건(중복 포함)을 문서마다 읽는다.
			if single.singleCalls != 4 {
				t.Errorf("단건 경로 호출 수 = %d, want 4", single.singleCalls)
			}
			if batch.batchCalls != 1 || batch.singleCalls != 0 {
				t.Errorf("배치 경로는 1회 조회여야 한다: batch=%d single=%d",
					batch.batchCalls, batch.singleCalls)
			}
			wantIDs := []uuid.UUID{uuidN(1), uuidN(2), uuidN(3)} // 청크 레인 제외, 중복 제거
			if len(batch.batchIDs) != 1 || !slices.Equal(batch.batchIDs[0], wantIDs) {
				t.Errorf("배치 조회 대상 ID = %v, want %v", batch.batchIDs, wantIDs)
			}
		})
	}

	// 등가성만으로는 "둘 다 틀린" 경우를 못 잡으므로 기대값을 한 번 고정한다.
	t.Run("선택 결과 고정", func(t *testing.T) {
		t.Parallel()
		results, chunks := batchFixture()
		batch := &batchListerDouble{perDocListerDouble: perDocListerDouble{byDoc: chunks}}
		got := (&Service{chunkStore: batch}).buildRerankDocs(context.Background(), "예산", results, tune)
		for i, want := range []string{
			"[통화 · 2026-09-01 · 9월 정기 회의]\n예산 승인 건은 다음 주에 처리하기로 했습니다",
			"[메일 · 동점 메일]\n예산 첫 번째 청크",   // 동점이면 chunk_index 가 작은 쪽
			"[노트 · 청크 없는 노트]\n노트 본문 그대로", // 청크 없음 → head 본문
			"[문서 · 긴 메일]\n이미 고른 청크 본문",   // 청크 레인 결과는 그대로
		} {
			if got[i] != want {
				t.Errorf("docs[%d] = %q, want %q", i, got[i], want)
			}
		}
	})
}

func TestBuildRerankDocs_BatchErrorFallsBackToHead(t *testing.T) {
	t.Parallel()
	results, chunks := batchFixture()
	batch := &batchListerDouble{
		perDocListerDouble: perDocListerDouble{byDoc: chunks},
		batchErr:           errors.New("boom"),
	}
	tune := model.SearchTuning{RerankInput: model.RerankInputBestChunk}.Normalized()
	got := (&Service{chunkStore: batch}).buildRerankDocs(context.Background(), "예산 승인", results, tune)

	if batch.batchCalls != 1 || batch.singleCalls != 0 {
		t.Fatalf("배치 실패 후 단건으로 재시도하면 안 된다: batch=%d single=%d",
			batch.batchCalls, batch.singleCalls)
	}
	for i, r := range results {
		if !strings.HasSuffix(got[i], "\n"+r.Content) {
			t.Errorf("docs[%d] 가 head 본문으로 되돌아가지 않았다: %q", i, got[i])
		}
	}
}

func TestBuildRerankDocs_BatchSkipsWhenOnlyChunkLaneResults(t *testing.T) {
	t.Parallel()
	hit := tuneResult(uuidN(9), "긴 메일", "이미 고른 청크 본문", 1.0)
	hit.MatchType = "chunk-vector"
	batch := &batchListerDouble{}
	tune := model.SearchTuning{RerankInput: model.RerankInputBestChunk}.Normalized()
	got := (&Service{chunkStore: batch}).buildRerankDocs(context.Background(), "질의",
		[]*model.SearchResult{hit}, tune)

	if batch.batchCalls != 0 || batch.singleCalls != 0 {
		t.Errorf("청크 레인 결과뿐이면 DB 를 읽지 않아야 한다: batch=%d single=%d",
			batch.batchCalls, batch.singleCalls)
	}
	if !strings.Contains(got[0], "이미 고른 청크 본문") {
		t.Errorf("청크 레인 본문이 쓰이지 않았다: %q", got[0])
	}
}

func TestBuildRerankDocs_HeadModeNeverBatches(t *testing.T) {
	t.Parallel()
	results, chunks := batchFixture()
	batch := &batchListerDouble{perDocListerDouble: perDocListerDouble{byDoc: chunks}}
	(&Service{chunkStore: batch}).buildRerankDocs(context.Background(), "예산", results,
		model.SearchTuning{}.Normalized())
	if batch.batchCalls != 0 || batch.singleCalls != 0 {
		t.Errorf("head 모드는 DB 를 읽지 않아야 한다: batch=%d single=%d",
			batch.batchCalls, batch.singleCalls)
	}
}
