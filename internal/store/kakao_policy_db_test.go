package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
)

// 문서·청크·엔티티에 신호가 이미 있어도 기본 후보는 제한 전에 제외한다.
// 실제 수집은 별도 테스트에서 검증한 대로 임베딩 백필에도 진입하지 않는다.
func TestDB_KakaoDefaultRetrievalIsolation(t *testing.T) {
	pg := srcTestDB(t)
	ctx := context.Background()
	ds, cs := NewDocumentStore(pg), NewChunkStore(pg)
	sms := seedSrcDoc(t, pg, model.SourceSMS, nil)
	for _, id := range []uuid.UUID{sms} {
		if _, err := pg.pool.Exec(ctx, `UPDATE documents SET summary_embedding=embedding WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	queries := []model.SearchQuery{
		{Query: "zzdummy", Limit: 1},
		{Query: "zzdummy", Limit: 1, Embedding: testEmbedding(), Weights: model.SearchWeights{EntityWeight: 1}},
		{Query: "zzdummy", Limit: 1, SparseTerms: model.SparseTerms{TSQuery: "zzdummy:*", Like: []string{"zzdummy"}}},
	}
	before := make([][]*model.SearchResult, len(queries))
	for i, q := range queries {
		var err error
		before[i], err = ds.Search(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(before[i]) != 1 {
			t.Fatal("missing baseline")
		}
	}
	for i := 0; i < 12; i++ {
		id := seedSrcDoc(t, pg, model.SourceKakao, nil)
		if _, err := pg.pool.Exec(ctx, `UPDATE documents SET summary_embedding=embedding WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := pg.pool.Exec(ctx, `INSERT INTO chunks(document_id,chunk_index,content,byte_size,embedding) VALUES($1,0,'zzdummy sentinel body',21,$2)`, id, pgvector.NewVector(testEmbedding())); err != nil {
			t.Fatal(err)
		}
	}
	for i, q := range queries {
		after, err := ds.Search(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before[i], after) {
			t.Fatalf("query %d changed after isolated corpus growth", i)
		}
	}
	for _, vector := range []bool{false, true} {
		q := model.SearchQuery{Query: "zzdummy", Embedding: testEmbedding()}
		search := cs.SearchFTSFiltered
		if vector {
			search = cs.SearchVectorFiltered
		}
		got, err := search(ctx, q, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("Kakao chunk leaked: vector=%v", vector)
		}
		q.SourceTypes = []model.SourceType{model.SourceKakao}
		got, err = search(ctx, q, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("explicit chunk search missing: vector=%v", vector)
		}
	}
	kakao := model.SourceKakao
	got, err := ds.Search(ctx, model.SearchQuery{Query: "zzdummy", SourceType: &kakao, Limit: 1})
	if err != nil || len(got) != 1 {
		t.Fatalf("explicit original search: count=%d error=%v", len(got), err)
	}
	if _, err := ds.GetByID(ctx, got[0].ID); err != nil {
		t.Fatalf("ID lookup blocked: %v", err)
	}
	recent, err := ds.ListRecent(ctx, model.SourceKakao, nil, 1, 0)
	if err != nil || len(recent) != 1 {
		t.Fatalf("recent list blocked: count=%d error=%v", len(recent), err)
	}
}

func TestDB_KakaoDoesNotEnterBackgroundQueuesOrCoverage(t *testing.T) {
	pg := srcTestDB(t)
	ctx := context.Background()
	ds, cs := NewDocumentStore(pg), NewChunkStore(pg)
	sms := seedSrcDoc(t, pg, model.SourceSMS, nil)
	if _, err := pg.pool.Exec(ctx, `UPDATE documents SET summary_embedding=embedding WHERE id=$1`, sms); err != nil {
		t.Fatal(err)
	}
	coverageBefore, err := ds.SummaryCoverageRatio(ctx)
	if err != nil {
		t.Fatal(err)
	}
	chunkCountBefore, err := cs.CountChunksNeedingEmbedding(ctx, "new-version")
	if err != nil {
		t.Fatal(err)
	}
	countBefore, err := ds.CountDocumentsNeedingEmbedding(ctx, "new-version")
	if err != nil {
		t.Fatal(err)
	}
	id := seedSrcDoc(t, pg, model.SourceKakao, nil)
	if _, err := pg.pool.Exec(ctx, `UPDATE documents SET embedding=NULL, collected_at='2000-01-01' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(ctx, `INSERT INTO chunks(document_id,chunk_index,content,byte_size) VALUES($1,0,'zzdummy',7)`, id); err != nil {
		t.Fatal(err)
	}
	for name, list := range map[string]func(context.Context, int) ([]*model.Document, error){
		"embedding": ds.ListUnembedded, "entity": ds.ListWithoutEntities, "summary": ds.ListUnsummarized,
		"versioned_embedding": func(ctx context.Context, n int) ([]*model.Document, error) {
			return ds.ListDocumentsNeedingEmbedding(ctx, n, "new-version")
		},
		"classification":      func(ctx context.Context, n int) ([]*model.Document, error) { return ds.ListUnclassified(ctx, n, 0) },
		"relation_extraction": ds.ListPendingForExtraction,
	} {
		rows, err := list(ctx, 1000)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, row := range rows {
			if row.ID == id {
				t.Fatalf("Kakao entered %s", name)
			}
		}
	}
	for _, version := range []string{"", "new-version"} {
		rows, err := cs.ListChunksNeedingEmbedding(ctx, 1000, version)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.DocumentID == id {
				t.Fatal("Kakao entered chunk embedding")
			}
		}
	}
	chunkCountAfter, err := cs.CountChunksNeedingEmbedding(ctx, "new-version")
	if err != nil {
		t.Fatal(err)
	}
	if chunkCountBefore != chunkCountAfter {
		t.Fatal("Kakao changed chunk embedding backlog count")
	}
	countAfter, err := ds.CountDocumentsNeedingEmbedding(ctx, "new-version")
	if err != nil {
		t.Fatal(err)
	}
	if countBefore != countAfter {
		t.Fatal("Kakao changed embedding backlog count")
	}
	// 선택 쿼리를 우회한 수동 백필도 공유 벡터 저장 경계에서 차단한다.
	if err := ds.UpdateEmbedding(ctx, &model.Document{ID: id, Embedding: testEmbedding()}); err != nil {
		t.Fatal(err)
	}
	if err := ds.UpdateSummary(ctx, id, "합성 요약", "합성 내용", testEmbedding()); err != nil {
		t.Fatal(err)
	}
	var chunkID int64
	if err := pg.pool.QueryRow(ctx, `SELECT id FROM chunks WHERE document_id=$1`, id).Scan(&chunkID); err != nil {
		t.Fatal(err)
	}
	if err := cs.UpdateChunkEmbeddings(ctx, []ChunkEmbedding{{ChunkID: chunkID, Embedding: testEmbedding()}}); err != nil {
		t.Fatal(err)
	}
	var polluted bool
	if err := pg.pool.QueryRow(ctx, `SELECT embedding IS NOT NULL OR summary_embedding IS NOT NULL OR title_summary IS NOT NULL FROM documents WHERE id=$1`, id).Scan(&polluted); err != nil {
		t.Fatal(err)
	}
	if polluted {
		t.Fatal("manual document embedding/summary bypassed isolation")
	}
	if err := pg.pool.QueryRow(ctx, `SELECT embedding IS NOT NULL FROM chunks WHERE id=$1`, chunkID).Scan(&polluted); err != nil {
		t.Fatal(err)
	}
	if polluted {
		t.Fatal("manual chunk embedding bypassed isolation")
	}
	coverageAfter, err := NewDocumentStore(pg).SummaryCoverageRatio(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if coverageBefore != coverageAfter {
		t.Fatal("Kakao changed summary lane coverage gate")
	}
}

func TestDB_KakaoEntityAndProjectionIsolation(t *testing.T) {
	pg := srcTestDB(t)
	ctx := context.Background()
	id := seedSrcDoc(t, pg, model.SourceKakao, nil)
	var entityID int64
	err := pg.pool.QueryRow(ctx, `INSERT INTO entities(name,type,normalized_name) VALUES('zzkakaoentity','person',$1) RETURNING id`, "zzkakaoentity-"+uuid.NewString()).Scan(&entityID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.pool.Exec(context.Background(), `DELETE FROM entities WHERE id=$1`, entityID) })
	if _, err = pg.pool.Exec(ctx, `INSERT INTO document_entities(document_id,entity_id) VALUES($1,$2)`, id, entityID); err != nil {
		t.Fatal(err)
	}
	if _, err = pg.pool.Exec(ctx, `UPDATE documents SET embedding=NULL WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err = pg.pool.Exec(ctx, `INSERT INTO entity_relations(from_entity_id,to_entity_id,type,evidence_document_id) VALUES($1,$1,'mentions',$2)`, entityID, id); err != nil {
		t.Fatal(err)
	}
	q := model.SearchQuery{Query: "zzkakaoentity", Limit: 10, Embedding: testEmbedding(), Weights: model.SearchWeights{EntityWeight: 1}}
	ds := NewDocumentStore(pg)
	rows, err := ds.Search(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatal("entity-only Kakao document entered default RAG")
	}
	q.SourceTypes = []model.SourceType{model.SourceKakao}
	rows, err = ds.Search(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != id {
		t.Fatal("entity fixture did not exercise entity lane")
	}
	gs := NewGraphSource(pg)
	for _, cursor := range []string{"", "00000000-0000-0000-0000-000000000000"} {
		mentions, err := gs.ListMentionsAfter(ctx, cursor, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range mentions {
			if row.DocumentID == id.String() {
				t.Fatal("Kakao entered graph mention projection")
			}
		}
	}
	relations, err := gs.ListRelationsAfter(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range relations {
		if row.EvidenceDocumentID == id.String() {
			t.Fatal("Kakao entered graph relation projection")
		}
	}
}
