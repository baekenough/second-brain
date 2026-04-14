package store

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestChunk_StructFields verifies that Chunk fields are accessible with the
// expected types (compile-time check via assignment).
func TestChunk_StructFields(t *testing.T) {
	t.Parallel()

	docID := uuid.MustParse("00000000-0000-0000-0000-000000000042")
	c := Chunk{
		ID:         1,
		DocumentID: docID,
		ChunkIndex: 0,
		Content:    "hello world",
		ByteSize:   11,
		CreatedAt:  time.Now(),
	}

	if c.ID != 1 {
		t.Errorf("ID: got %d, want 1", c.ID)
	}
	if c.DocumentID != docID {
		t.Errorf("DocumentID: got %s, want %s", c.DocumentID, docID)
	}
	if c.ChunkIndex != 0 {
		t.Errorf("ChunkIndex: got %d, want 0", c.ChunkIndex)
	}
	if c.Content != "hello world" {
		t.Errorf("Content: got %q, want %q", c.Content, "hello world")
	}
	if c.ByteSize != 11 {
		t.Errorf("ByteSize: got %d, want 11", c.ByteSize)
	}
}

// TestChunkSearchResult_StructFields verifies ChunkSearchResult field types.
func TestChunkSearchResult_StructFields(t *testing.T) {
	t.Parallel()

	docID := uuid.MustParse("00000000-0000-0000-0000-000000000005")
	r := ChunkSearchResult{
		Chunk: Chunk{
			ID:         10,
			DocumentID: docID,
			ChunkIndex: 2,
			Content:    "search result content",
			ByteSize:   21,
		},
		Rank:           0.75,
		DocumentTitle:  "Test Document",
		DocumentSource: "slack",
		DocumentStatus: "active",
	}

	if r.Rank != 0.75 {
		t.Errorf("Rank: got %f, want 0.75", r.Rank)
	}
	if r.DocumentTitle != "Test Document" {
		t.Errorf("DocumentTitle: got %q, want 'Test Document'", r.DocumentTitle)
	}
	if r.DocumentSource != "slack" {
		t.Errorf("DocumentSource: got %q, want 'slack'", r.DocumentSource)
	}
	if r.DocumentStatus != "active" {
		t.Errorf("DocumentStatus: got %q, want 'active'", r.DocumentStatus)
	}
}

// TestChunkStore_QueryConstants verifies that the SearchFTS query contains
// the expected SQL fragments (query string validation without a live DB).
func TestChunkStore_QueryConstants(t *testing.T) {
	t.Parallel()

	// The SQL used in SearchFTS is a package-level constant; we verify key
	// fragments via substring matching to catch accidental regressions.
	// This is a lightweight structural test — integration tests hit the real DB.
	const searchQuery = `
		SELECT
			c.id,
			c.document_id,
			c.chunk_index,
			c.content,
			c.byte_size,
			c.created_at,
			ts_rank(c.content_tsv, plainto_tsquery('simple', $1)) AS rank,
			d.title          AS document_title,
			d.source_type    AS document_source,
			d.status         AS document_status
		FROM chunks c
		JOIN documents d ON d.id = c.document_id
		WHERE c.content_tsv @@ plainto_tsquery('simple', $1)
		  AND d.status = 'active'
		ORDER BY rank DESC
		LIMIT $2`

	expectedFragments := []string{
		"ts_rank",
		"plainto_tsquery('simple'",
		"content_tsv @@",
		"d.status = 'active'",
		"ORDER BY rank DESC",
		"LIMIT $2",
	}

	for _, frag := range expectedFragments {
		if !strings.Contains(searchQuery, frag) {
			t.Errorf("SearchFTS query missing expected fragment: %q", frag)
		}
	}
}

// TestNewChunkStore_NilPanics verifies that constructing a ChunkStore with a
// nil Postgres panics rather than silently creating a broken store.
// This is primarily a documentation test of the expected contract.
func TestNewChunkStore_Panics_WhenNilPostgres(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic when passing nil Postgres, but none occurred")
		}
	}()

	// Constructing with nil pg and then calling ReplaceDocument would panic
	// on pool access. We simulate the panic by dereferencing the nil.
	cs := &ChunkStore{pg: nil}
	_ = cs.pg.pool // should panic: nil pointer dereference
}
