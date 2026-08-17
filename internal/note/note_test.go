package note

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// --- test doubles ---

type fakeDocUpserter struct {
	upsertErr error
	lastDoc   *model.Document
}

func (f *fakeDocUpserter) Upsert(_ context.Context, doc *model.Document) error {
	if doc.ID == (uuid.UUID{}) {
		doc.ID = uuid.New()
	}
	f.lastDoc = doc
	return f.upsertErr
}

type fakeChunkWriter struct {
	replaceErr   error
	updateErr    error
	chunks       []store.Chunk
	listErr      error
	replaceCalls int
	updateCalls  int
}

func (f *fakeChunkWriter) ReplaceDocument(_ context.Context, _ uuid.UUID, chunks []store.Chunk) error {
	f.replaceCalls++
	f.chunks = chunks
	return f.replaceErr
}

func (f *fakeChunkWriter) ListByDocument(_ context.Context, _ uuid.UUID) ([]store.Chunk, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.chunks, nil
}

func (f *fakeChunkWriter) UpdateChunkEmbeddings(_ context.Context, _ []store.ChunkEmbedding) error {
	f.updateCalls++
	return f.updateErr
}

type fakeEmbedder struct {
	enabled  bool
	embedErr error
	vectors  [][]float32
}

func (f *fakeEmbedder) Enabled() bool { return f.enabled }

func (f *fakeEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	if f.embedErr != nil {
		return nil, f.embedErr
	}
	if f.vectors != nil {
		return f.vectors, nil
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{0.1, 0.2}
	}
	return out, nil
}

// --- tests ---

// TestSave_RequireTitle_EmptyTitle_ReturnsError verifies the MCP path
// (requireTitle=true) rejects an empty title, unchanged from the
// pre-extraction handleAddNote behaviour.
func TestSave_RequireTitle_EmptyTitle_ReturnsError(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{enabled: false}

	_, errMsg := Save(context.Background(), docs, chunks, embed,
		model.SourceLLMMemory, "" /* title */, "some content", "", nil, false, true /* requireTitle */)

	if errMsg == "" {
		t.Fatal("expected error for empty title when requireTitle=true, got none")
	}
	if !strings.Contains(errMsg, "title") {
		t.Errorf("errMsg = %q, want it to mention title", errMsg)
	}
}

// TestSave_OptionalTitle_EmptyTitle_Accepted verifies the REST path
// (requireTitle=false) accepts an empty title — spec §6.1: the enrichment
// worker fills in the real title later, and the server must not guess one.
func TestSave_OptionalTitle_EmptyTitle_Accepted(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{enabled: false}

	result, errMsg := Save(context.Background(), docs, chunks, embed,
		model.SourceNote, "" /* title */, "some content", "", nil, false, false /* requireTitle */)

	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if docs.lastDoc.Title != "" {
		t.Errorf("Title = %q, want empty string preserved (no guessed title)", docs.lastDoc.Title)
	}
	if result.ID == "" {
		t.Error("expected a non-empty result ID")
	}
}

// TestSave_EmptyContent_AlwaysRejected verifies content is required
// regardless of requireTitle.
func TestSave_EmptyContent_AlwaysRejected(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{enabled: false}

	_, errMsg := Save(context.Background(), docs, chunks, embed,
		model.SourceNote, "title", "" /* content */, "", nil, false, false)

	if errMsg == "" {
		t.Fatal("expected error for empty content, got none")
	}
}

// TestSave_ContentTooLarge_ReturnsError verifies the 10 MiB content cap.
func TestSave_ContentTooLarge_ReturnsError(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{enabled: false}

	oversized := strings.Repeat("a", MaxContentBytes+1)
	_, errMsg := Save(context.Background(), docs, chunks, embed,
		model.SourceNote, "title", oversized, "", nil, false, false)

	if errMsg == "" {
		t.Fatal("expected error for content exceeding MaxContentBytes, got none")
	}
}

// TestSave_EmbedSkipped_ChunkCountExceedsLimit verifies that exceeding
// MaxEmbedChunks skips embedding silently (EmbeddingCreated=false, no error
// surfaced) rather than failing the whole Save call.
func TestSave_EmbedSkipped_ChunkCountExceedsLimit(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	// Build content that chunker.Split will break into > MaxEmbedChunks
	// pieces. model.SourceNote falls into chunker.SelectOptions' "unknown /
	// future source types" default branch (TargetSize=2000 runes,
	// HeadingAware=true). splitFlat's mergeSegments step folds consecutive
	// *short* paragraphs together up to TargetSize, so short paragraphs
	// alone do not guarantee a chunk-per-paragraph count. Each paragraph
	// here is sized just over TargetSize (2001 runes) so mergeSegments
	// cannot combine it with a neighbour — paragraph count equals chunk
	// count, guaranteeing > MaxEmbedChunks chunks regardless of merge
	// behaviour.
	oversizedParagraph := strings.Repeat("a", 2001)
	var sb strings.Builder
	for i := 0; i < MaxEmbedChunks+10; i++ {
		sb.WriteString(oversizedParagraph)
		sb.WriteString("\n\n")
	}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{enabled: true}

	result, errMsg := Save(context.Background(), docs, chunks, embed,
		model.SourceNote, "title", sb.String(), "", nil, true, false)

	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if result.EmbeddingCreated {
		t.Error("EmbeddingCreated = true, want false when chunk count exceeds MaxEmbedChunks")
	}
}

// TestSave_SourceTypeParameterized verifies that the caller-supplied
// sourceType is what gets persisted — the MCP path passes
// model.SourceLLMMemory, the REST path passes model.SourceNote, and Save
// must not hard-code either (spec §6.2).
func TestSave_SourceTypeParameterized(t *testing.T) {
	t.Parallel()

	tests := []model.SourceType{model.SourceLLMMemory, model.SourceNote}
	for _, st := range tests {
		st := st
		t.Run(string(st), func(t *testing.T) {
			t.Parallel()
			docs := &fakeDocUpserter{}
			chunks := &fakeChunkWriter{}
			embed := &fakeEmbedder{enabled: false}

			_, errMsg := Save(context.Background(), docs, chunks, embed,
				st, "title", "content", "", nil, false, false)

			if errMsg != "" {
				t.Fatalf("unexpected error: %s", errMsg)
			}
			if docs.lastDoc.SourceType != st {
				t.Errorf("SourceType = %q, want %q", docs.lastDoc.SourceType, st)
			}
		})
	}
}
