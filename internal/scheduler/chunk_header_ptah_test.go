package scheduler

import (
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/chunkctx"
	"github.com/baekenough/second-brain/internal/model"
)

// deploy/ptah embeds chunk_sparse_context.sparse_text for context_version
// v1-full as the chunk vector input. That is only the chunk-ctx-v1 recipe
// while chunkctx.BuildSparseText(RecipeFull, ...) and withChunkContextHeader
// build the same string, including the document with no header at all.
func TestChunkEmbeddingInputIsTheV1FullSparseText(t *testing.T) {
	occurred := time.Date(2026, 9, 18, 5, 20, 0, 0, time.UTC)
	docs := []model.Document{
		{
			SourceType: model.SourceGmail, Title: "3분기 예산 검토", OccurredAt: &occurred,
			Metadata: map[string]any{"from": `"김민준" <minjun.kim@example.com>`, "to": "seoyeon.lee@example.com"},
		},
		{
			SourceType: model.SourceCallTranscript, Title: "통화 전사", OccurredAt: &occurred,
			Metadata: map[string]any{"contact_name": "강하은", "direction": "outgoing", "number": "010-0000-0000"},
		},
		{SourceType: model.SourceNote, Title: "독서 메모"},
		{},
	}
	for _, doc := range docs {
		sparse, ok := chunkctx.BuildSparseText(chunkctx.RecipeFull, doc, "청크 본문")
		if !ok {
			t.Fatalf("RecipeFull is not a known recipe")
		}
		if embed := withChunkContextHeader(doc, "청크 본문"); embed != sparse {
			t.Errorf("source %q: embedding input %q, v1-full sparse text %q", doc.SourceType, embed, sparse)
		}
	}
}
