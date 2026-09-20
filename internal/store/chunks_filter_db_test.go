package store

import (
	"context"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
	"testing"
	"time"
)

func TestDB_ChunkFiltersBeforeLimit(t *testing.T) {
	pg := chunkTSTestDB(t)
	ctx := context.Background()
	cs := NewChunkStore(pg)
	from := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 1)
	before := from.Add(-time.Hour)
	eligible := seedChunkDoc(t, pg, &from, from)
	wrongSource := seedChunkDoc(t, pg, &from, from)
	old := seedChunkDoc(t, pg, &before, before)
	disposable := seedChunkDoc(t, pg, &from, from)
	atEnd := seedChunkDoc(t, pg, &to, to)
	noTime := seedChunkDoc(t, pg, nil, from)
	if _, err := pg.pool.Exec(ctx, `UPDATE documents SET source_type='gmail' WHERE id=$1`, wrongSource); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(ctx, `UPDATE documents SET metadata='{"retention":"disposable"}'::jsonb WHERE id=$1`, disposable); err != nil {
		t.Fatal(err)
	}
	// Make every ineligible vector closer than the eligible vector. LIMIT 1
	// before predicates would therefore consume the candidate budget wrongly.
	lower := testEmbedding()
	lower[2] = -.3
	if _, err := pg.pool.Exec(ctx, `UPDATE chunks SET embedding=$1 WHERE document_id=$2`, pgvector.NewVector(lower), eligible); err != nil {
		t.Fatal(err)
	}
	// Strong FTS term frequency on excluded documents, weak eligible body.
	for _, id := range []uuid.UUID{wrongSource, old, disposable, atEnd, noTime} {
		if _, err := pg.pool.Exec(ctx, `UPDATE chunks SET content='zzdummychunkneedle zzdummychunkneedle zzdummychunkneedle' WHERE document_id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	q := model.SearchQuery{Query: "zzdummychunkneedle", Embedding: testEmbedding(), SourceTypes: []model.SourceType{model.SourceCalendar}, OccurredFrom: &from, OccurredTo: &to, ExcludeRetention: []string{model.RetentionDisposable}}
	for _, lane := range []struct {
		name string
		run  func(context.Context, model.SearchQuery, int) ([]ChunkSearchResult, error)
	}{{"vector", cs.SearchVectorFiltered}, {"fts", cs.SearchFTSFiltered}} {
		t.Run(lane.name, func(t *testing.T) {
			rows, err := lane.run(ctx, q, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Chunk.DocumentID != eligible {
				t.Fatalf("eligible candidate lost before limit: count=%d", len(rows))
			}
		})
	}
	q.SourceTypes = nil
	q.ExcludeSourceTypes = []model.SourceType{model.SourceGmail}
	rows, err := cs.SearchVectorFiltered(ctx, q, 1)
	if err != nil || len(rows) != 1 || rows[0].Chunk.DocumentID != eligible {
		t.Fatalf("exclusion failed: count=%d error=%v", len(rows), err)
	}
}
