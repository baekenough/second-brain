package store

import (
	"context"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
)

// ---------------------------------------------------------------------------
// #215 — the chunk lanes must return the parent document's timestamps.
//
// A struct-field test proves the Go type has the fields. It cannot prove that
// PostgreSQL accepts `d.occurred_at` in this SELECT list, that the column
// order in the statement matches the order of the Scan targets, or that pgx
// decodes a NULL timestamptz into a *time.Time rather than failing. Column
// order in particular is the failure this file exists for: adding a column to
// the SELECT and forgetting one Scan target compiles, passes every stub test,
// and then either errors or silently shifts every subsequent field at runtime.
//
// Skipped unless TEST_DATABASE_URL is set. It must NEVER point at production —
// run a throwaway pgvector container. Seeded rows are scoped by the sentinel
// source_id prefix below and removed in t.Cleanup. All content is filler.
// ---------------------------------------------------------------------------

const chunkTSPrefix = "zz-dummy-chunkts-"

// chunkTSTestDB reuses the documents schema helper from
// document_source_types_db_test.go and adds the chunks table the chunk lanes
// join against.
func chunkTSTestDB(t *testing.T) *Postgres {
	t.Helper()
	pg := srcTestDB(t) // skips without TEST_DATABASE_URL; creates `documents`

	ctx := context.Background()
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS chunks (
			id bigserial PRIMARY KEY,
			document_id uuid NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
			chunk_index int NOT NULL,
			content text NOT NULL,
			byte_size int NOT NULL DEFAULT 0,
			embedding vector(3),
			created_at timestamptz NOT NULL DEFAULT now(),
			content_tsv tsvector GENERATED ALWAYS AS (
				to_tsvector('simple', coalesce(content,''))
			) STORED
		)`,
	}
	for _, s := range stmts {
		if _, err := pg.pool.Exec(ctx, s); err != nil {
			t.Fatalf("prepare chunks schema: %v", err)
		}
	}
	// documents rows are removed by srcTestDB's cleanup; ON DELETE CASCADE
	// takes the chunks with them.
	return pg
}

// seedChunkDoc inserts one document plus one chunk and returns the document ID.
func seedChunkDoc(t *testing.T, pg *Postgres, occurredAt *time.Time, collectedAt time.Time) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	id := uuid.New()
	if _, err := pg.pool.Exec(ctx, `
		INSERT INTO documents (id, source_type, source_id, title, content, embedding, occurred_at, collected_at)
		VALUES ($1, $2, $3, 'zzdummy chunk title', 'zzdummy chunk body', $4, $5, $6)`,
		id, string(model.SourceCalendar), chunkTSPrefix+id.String(),
		pgvector.NewVector([]float32{0.1, 0.2, 0.3}), occurredAt, collectedAt,
	); err != nil {
		t.Fatalf("seed document: %v", err)
	}
	if _, err := pg.pool.Exec(ctx, `
		INSERT INTO chunks (document_id, chunk_index, content, byte_size, embedding)
		VALUES ($1, 0, 'zzdummychunkneedle filler text', 30, $2)`,
		id, pgvector.NewVector([]float32{0.1, 0.2, 0.3}),
	); err != nil {
		t.Fatalf("seed chunk: %v", err)
	}
	return id
}

// TestDB_ChunkSearch_ReturnsDocumentTimestamps executes both chunk statements
// verbatim and asserts the joined timestamps arrive intact — including the NULL
// occurred_at case, which is the one that must stay nil rather than becoming
// the zero instant.
func TestDB_ChunkSearch_ReturnsDocumentTimestamps(t *testing.T) {
	pg := chunkTSTestDB(t)
	cs := NewChunkStore(pg)

	occurred := time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC)
	collectedWith := time.Date(2026, 8, 19, 1, 0, 0, 0, time.UTC)
	collectedWithout := time.Date(2026, 8, 18, 2, 0, 0, 0, time.UTC)

	withEvent := seedChunkDoc(t, pg, &occurred, collectedWith)
	noEvent := seedChunkDoc(t, pg, nil, collectedWithout)

	check := func(t *testing.T, lane string, rows []ChunkSearchResult) {
		t.Helper()
		byDoc := map[uuid.UUID]ChunkSearchResult{}
		for _, r := range rows {
			byDoc[r.Chunk.DocumentID] = r
		}

		got, ok := byDoc[withEvent]
		if !ok {
			t.Fatalf("%s: seeded document %s not returned; the lane cannot be asserted", lane, withEvent)
		}
		if got.DocumentOccurredAt == nil {
			t.Errorf("%s: DocumentOccurredAt = nil, want %s", lane, occurred)
		} else if !got.DocumentOccurredAt.UTC().Equal(occurred) {
			t.Errorf("%s: DocumentOccurredAt = %s, want %s", lane, got.DocumentOccurredAt.UTC(), occurred)
		}
		if !got.DocumentCollectedAt.UTC().Equal(collectedWith) {
			t.Errorf("%s: DocumentCollectedAt = %s, want %s", lane, got.DocumentCollectedAt.UTC(), collectedWith)
		}
		// Column-order guard: a mis-ordered Scan list shows up here first,
		// because these fields sit next to the new ones in the SELECT.
		if got.DocumentSource != string(model.SourceCalendar) || got.DocumentStatus != "active" {
			t.Errorf("%s: source/status = %q/%q, want %q/active — SELECT and Scan orders disagree",
				lane, got.DocumentSource, got.DocumentStatus, model.SourceCalendar)
		}

		none, ok := byDoc[noEvent]
		if !ok {
			t.Fatalf("%s: seeded document %s (NULL occurred_at) not returned", lane, noEvent)
		}
		if none.DocumentOccurredAt != nil {
			t.Errorf("%s: DocumentOccurredAt = %s for a NULL column, want nil", lane, none.DocumentOccurredAt)
		}
		if !none.DocumentCollectedAt.UTC().Equal(collectedWithout) {
			t.Errorf("%s: DocumentCollectedAt = %s, want %s", lane, none.DocumentCollectedAt.UTC(), collectedWithout)
		}
	}

	t.Run("SearchFTS", func(t *testing.T) {
		rows, err := cs.SearchFTS(context.Background(), "zzdummychunkneedle", 50)
		if err != nil {
			t.Fatalf("SearchFTS: %v", err)
		}
		check(t, "SearchFTS", rows)
	})

	t.Run("SearchVector", func(t *testing.T) {
		rows, err := cs.SearchVector(context.Background(), []float32{0.1, 0.2, 0.3}, 50)
		if err != nil {
			t.Fatalf("SearchVector: %v", err)
		}
		check(t, "SearchVector", rows)
	})
}
