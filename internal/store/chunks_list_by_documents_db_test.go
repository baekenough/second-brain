package store

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// ChunkStore.ListByDocuments 실 DB 검증(#263).
//
// 리랭크 입력 선택은 "동점이면 chunk_index 가 작은 청크" 규칙을 쓰고, 그 규칙은
// 조회 순서에 기댄다. 배치 조회가 단건 조회(ListByDocument)와 한 행이라도 다른
// 순서·내용을 돌려주면 같은 질의의 순위가 조용히 바뀐다. 그래서 여기서는
// "배치 결과 == 문서마다 단건 조회한 결과" 를 실제 SQL 로 대조한다.
//
// TEST_DATABASE_URL 이 없으면 건너뛴다(이 패키지의 다른 *_db_test.go 와 같은
// 규약). 운영 DB 를 가리키면 안 된다 — 일회용 pgvector+pg_bigm 컨테이너에
// TestMain 이 전체 마이그레이션을 적용한 뒤 실행한다. 쓴 행은 전부 정리한다.
// ---------------------------------------------------------------------------

const listByDocsTestPrefix = "zz-dummy-listbydocs-"

func listByDocsTestDB(t *testing.T) *Postgres {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-database ListByDocuments test")
	}
	pg, err := NewPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(pg.Close)
	return pg
}

// seedListByDocsDoc 는 문서 한 건과 그 청크를 넣는다. 청크는 주어진 순서
// 그대로 삽입하므로, 호출자가 chunk_index 를 섞어 넘기면 ORDER BY 가 실제로
// 일하는지 확인할 수 있다.
func seedListByDocsDoc(t *testing.T, pg *Postgres, chunks []Chunk) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	doc := &model.Document{
		SourceType:  model.SourceNote,
		SourceID:    listByDocsTestPrefix + uuid.NewString(),
		Title:       "listbydocs fixture",
		Content:     "본문",
		Metadata:    map[string]any{},
		CollectedAt: time.Now(),
	}
	if _, err := NewDocumentStore(pg).UpsertTracked(ctx, doc); err != nil {
		t.Fatalf("seed document: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.pool.Exec(context.Background(), `DELETE FROM documents WHERE id = $1`, doc.ID)
	})
	// UpsertTracked 가 본문으로 청크를 만들었을 수 있으므로 fixture 로 통째 교체한다.
	if err := NewChunkStore(pg).ReplaceDocument(ctx, doc.ID, chunks); err != nil {
		t.Fatalf("seed chunks: %v", err)
	}
	return doc.ID
}

func TestChunkStore_ListByDocuments_MatchesListByDocument(t *testing.T) {
	pg := listByDocsTestDB(t)
	ctx := context.Background()
	cs := NewChunkStore(pg)

	// 삽입 순서를 일부러 뒤섞는다(2, 0, 1).
	multi := seedListByDocsDoc(t, pg, []Chunk{
		{ChunkIndex: 2, Content: "세 번째 청크", ByteSize: len("세 번째 청크")},
		{ChunkIndex: 0, Content: "첫 번째 청크", ByteSize: len("첫 번째 청크")},
		{ChunkIndex: 1, Content: "두 번째 청크", ByteSize: len("두 번째 청크")},
	})
	single := seedListByDocsDoc(t, pg, []Chunk{
		{ChunkIndex: 0, Content: "하나뿐인 청크", ByteSize: len("하나뿐인 청크")},
	})
	empty := seedListByDocsDoc(t, pg, nil)
	unknown := uuid.New() // DB 에 없는 문서

	ids := []uuid.UUID{single, multi, empty, unknown, multi} // 중복 포함
	got, err := cs.ListByDocuments(ctx, ids)
	if err != nil {
		t.Fatalf("ListByDocuments: %v", err)
	}

	for _, id := range []uuid.UUID{multi, single} {
		want, err := cs.ListByDocument(ctx, id)
		if err != nil {
			t.Fatalf("ListByDocument(%s): %v", id, err)
		}
		if len(want) == 0 {
			t.Fatalf("fixture 문서 %s 에 청크가 없다 — 대조가 무의미하다", id)
		}
		if !slices.EqualFunc(got[id], want, chunksEqual) {
			t.Errorf("문서 %s: 배치 결과가 단건 조회와 다르다\n got: %+v\nwant: %+v", id, got[id], want)
		}
	}

	var idx []int
	for _, c := range got[multi] {
		idx = append(idx, c.ChunkIndex)
	}
	if !slices.Equal(idx, []int{0, 1, 2}) {
		t.Errorf("chunk_index 오름차순이 아니다: %v", idx)
	}

	for _, id := range []uuid.UUID{empty, unknown} {
		if cs, ok := got[id]; ok {
			t.Errorf("청크 없는 문서 %s 는 맵에 키가 없어야 한다: %+v", id, cs)
		}
	}
	if len(got) != 2 {
		t.Errorf("맵 키 수 = %d, want 2 (중복 ID 가 행을 부풀리면 안 된다)", len(got))
	}
}

func TestChunkStore_ListByDocuments_EmptyIDs(t *testing.T) {
	pg := listByDocsTestDB(t)
	got, err := NewChunkStore(pg).ListByDocuments(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListByDocuments(nil): %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("빈 입력은 빈(non-nil) 맵이어야 한다: %#v", got)
	}
}

func chunksEqual(a, b Chunk) bool {
	return a.ID == b.ID && a.DocumentID == b.DocumentID && a.ChunkIndex == b.ChunkIndex &&
		a.Content == b.Content && a.ByteSize == b.ByteSize && a.CreatedAt.Equal(b.CreatedAt)
}
