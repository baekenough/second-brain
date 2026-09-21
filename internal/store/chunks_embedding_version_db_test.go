package store

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
)

// 임베딩 버전(마이그레이션 037) 동작을 실제 DB 로 검증한다.
//
// TEST_DATABASE_URL 이 없으면 건너뛴다 — 운영 DB 를 절대 건드리지 않기 위한
// 기존 규약(internal/store/main_test.go)을 그대로 따른다. 이 테스트를 실행하려면
// 일회용 DB 를 가리키게 할 것:
//
//	TEST_DATABASE_URL=postgres://... go test ./internal/store/
//
// 쓰는 행은 전부 이 테스트가 만든 것이고 t.Cleanup 에서 지운다.
func embeddingVersionTestDB(t *testing.T) *Postgres {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-database embedding version test")
	}
	pg, err := NewPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(pg.Close)
	return pg
}

// seedChunkWithVersion 은 문서 1건 + 청크 1건을 만들고 정리까지 등록한다.
func seedChunkWithVersion(t *testing.T, pg *Postgres, version *string, withEmbedding bool) (uuid.UUID, int64) {
	t.Helper()
	ctx := context.Background()

	docID := uuid.New()
	_, err := pg.pool.Exec(ctx, `
		INSERT INTO documents (id, source_type, source_id, title, content, metadata, occurred_at, collected_at)
		VALUES ($1, 'sms', $2, '문자 수신 테스트', '본문', '{"contact_name":"테스트"}'::jsonb, now(), now())`,
		docID, "zz-embedding-version-test-"+docID.String(),
	)
	if err != nil {
		t.Fatalf("seed document: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.pool.Exec(context.Background(), `DELETE FROM documents WHERE id = $1`, docID)
	})

	var embedding any
	if withEmbedding {
		embedding = pgvector.NewVector(testEmbedding())
	}
	var chunkID int64
	err = pg.pool.QueryRow(ctx, `
		INSERT INTO chunks (document_id, chunk_index, content, byte_size, embedding, embedding_version)
		VALUES ($1, 0, '청크 본문', 12, $2, $3)
		RETURNING id`,
		docID, embedding, version,
	).Scan(&chunkID)
	if err != nil {
		t.Fatalf("seed chunk: %v", err)
	}
	return docID, chunkID
}

// ListChunksNeedingEmbedding 선별 규칙을 실제 SQL 로 확인한다.
func TestListChunksNeedingEmbedding_버전선별(t *testing.T) {
	pg := embeddingVersionTestDB(t)
	cs := NewChunkStore(pg)

	current := "text-embedding-3-small:1536:chunk-ctx-v1"
	legacy := "text-embedding-3-small:1536:legacy"

	_, nullEmbedID := seedChunkWithVersion(t, pg, nil, false)    // 임베딩 없음
	_, staleID := seedChunkWithVersion(t, pg, &legacy, true)     // 구버전
	_, currentID := seedChunkWithVersion(t, pg, &current, true)  // 최신
	_, legacyNullVerID := seedChunkWithVersion(t, pg, nil, true) // 037 이전 레거시

	// 버전을 넘기지 않으면 임베딩이 NULL 인 청크만 대상이다.
	got := listChunkIDs(t, cs, "")
	if !got[nullEmbedID] {
		t.Errorf("임베딩 NULL 청크가 선별되지 않았다: %d", nullEmbedID)
	}
	for _, id := range []int64{staleID, currentID, legacyNullVerID} {
		if got[id] {
			t.Errorf("버전 미지정인데 구버전 청크가 선별됐다: %d", id)
		}
	}

	// 버전을 넘기면 구버전·레거시(NULL 버전)까지 포함하되 최신은 제외한다.
	got = listChunkIDs(t, cs, current)
	for _, id := range []int64{nullEmbedID, staleID, legacyNullVerID} {
		if !got[id] {
			t.Errorf("재임베딩 대상이 빠졌다: %d", id)
		}
	}
	if got[currentID] {
		t.Errorf("최신 버전 청크가 재임베딩 대상으로 잡혔다: %d", currentID)
	}
}

// UpdateChunkEmbeddings 가 벡터와 버전을 같은 UPDATE 에서 쓰는지 확인한다.
// 버전이 남지 않으면 같은 청크가 매 사이클 다시 선별돼 무한 재임베딩이 된다.
func TestUpdateChunkEmbeddings_버전기록(t *testing.T) {
	pg := embeddingVersionTestDB(t)
	cs := NewChunkStore(pg)
	ctx := context.Background()

	current := "text-embedding-3-small:1536:chunk-ctx-v1"
	_, chunkID := seedChunkWithVersion(t, pg, nil, false)

	if err := cs.UpdateChunkEmbeddings(ctx, []ChunkEmbedding{
		{ChunkID: chunkID, Embedding: testEmbedding(), Version: current},
	}); err != nil {
		t.Fatalf("UpdateChunkEmbeddings: %v", err)
	}

	var got *string
	if err := pg.pool.QueryRow(ctx,
		`SELECT embedding_version FROM chunks WHERE id = $1`, chunkID).Scan(&got); err != nil {
		t.Fatalf("read back version: %v", err)
	}
	if got == nil || *got != current {
		t.Fatalf("버전이 기록되지 않았다: %v", got)
	}

	// 같은 버전으로 다시 조회하면 더 이상 대상이 아니어야 한다(무한 루프 방지).
	if listChunkIDs(t, cs, current)[chunkID] {
		t.Fatalf("버전을 기록했는데도 재임베딩 대상으로 남아 있다: %d", chunkID)
	}

	// Version 이 빈 문자열이면 기존 버전을 지우지 않는다.
	if err := cs.UpdateChunkEmbeddings(ctx, []ChunkEmbedding{
		{ChunkID: chunkID, Embedding: testEmbedding()},
	}); err != nil {
		t.Fatalf("UpdateChunkEmbeddings (버전 없음): %v", err)
	}
	if err := pg.pool.QueryRow(ctx,
		`SELECT embedding_version FROM chunks WHERE id = $1`, chunkID).Scan(&got); err != nil {
		t.Fatalf("read back version: %v", err)
	}
	if got == nil || *got != current {
		t.Fatalf("빈 버전이 기존 값을 덮어썼다: %v", got)
	}
}

// listChunkIDs 는 재임베딩 대상 청크 ID 집합을 돌려준다.
func listChunkIDs(t *testing.T, cs *ChunkStore, version string) map[int64]bool {
	t.Helper()
	chunks, err := cs.ListChunksNeedingEmbedding(context.Background(), 100, version)
	if err != nil {
		t.Fatalf("ListChunksNeedingEmbedding: %v", err)
	}
	set := make(map[int64]bool, len(chunks))
	for _, c := range chunks {
		set[c.ID] = true
	}
	return set
}
