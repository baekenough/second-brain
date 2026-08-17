package note

import (
	"context"
	"errors"
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
	enabled    bool
	embedErr   error
	vectors    [][]float32
	embedCalls int
}

func (f *fakeEmbedder) Enabled() bool { return f.enabled }

func (f *fakeEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	f.embedCalls++
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

// skipSentinelEmbedder is an Embedder that returns ErrEmbedSkipped from
// EmbedBatch, simulating what embedChunks propagates when chunk count
// exceeds MaxEmbedChunks. It lets TestSave_EmbedSkippedSentinel_EmbeddingCreatedFalse
// exercise Save's sentinel-handling switch branch without needing to
// construct MaxEmbedChunks+1 real chunks.
type skipSentinelEmbedder struct{}

func (s *skipSentinelEmbedder) Enabled() bool { return true }
func (s *skipSentinelEmbedder) EmbedBatch(_ context.Context, _ []string) ([][]float32, error) {
	return nil, ErrEmbedSkipped
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

// ---------------------------------------------------------------------------
// Ported from cmd/mcp/main_test.go (Task 3 fix round 1): these behaviours
// were covered by the pre-extraction handleAddNote tests but did not move
// with the logic when cmd/mcp/main.go was rewired onto note.Save.
// ---------------------------------------------------------------------------

// TestSave_Success_EmbedTrue verifies the normal path with doEmbed=true and
// an enabled embedder: the note is saved, chunked, and embedded.
// Ported from TestHandleAddNote_Success_EmbedTrue.
func TestSave_Success_EmbedTrue(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{enabled: true}

	result, errMsg := Save(context.Background(), docs, chunks, embed,
		model.SourceLLMMemory, "Test Note", "Some content for the note.", "", nil, true, true)

	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.ID == "" {
		t.Error("expected non-empty document ID in result")
	}
	if !result.EmbeddingCreated {
		t.Error("expected EmbeddingCreated=true when doEmbed=true and embedder enabled")
	}
	if chunks.replaceCalls != 1 {
		t.Errorf("ReplaceDocument called %d times, want 1", chunks.replaceCalls)
	}
	if embed.embedCalls != 1 {
		t.Errorf("EmbedBatch called %d times, want 1", embed.embedCalls)
	}
}

// TestSave_Success_EmbedFalse verifies the normal path with doEmbed=false:
// the note is saved and chunked but never embedded.
// Ported from TestHandleAddNote_Success_EmbedFalse.
func TestSave_Success_EmbedFalse(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{enabled: true}

	result, errMsg := Save(context.Background(), docs, chunks, embed,
		model.SourceLLMMemory, "Test Note", "Some content for the note.", "", nil, false, true)

	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if result.EmbeddingCreated {
		t.Error("expected EmbeddingCreated=false when doEmbed=false")
	}
	if embed.embedCalls != 0 {
		t.Errorf("EmbedBatch called %d times, want 0 when doEmbed=false", embed.embedCalls)
	}
}

// TestSave_EmbedDisabled_SkipsEmbedding verifies that a disabled embedder
// (Enabled()=false) is never called even when doEmbed=true.
// Ported from TestHandleAddNote_EmbedDisabled_SkipsEmbedding.
func TestSave_EmbedDisabled_SkipsEmbedding(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{enabled: false}

	result, errMsg := Save(context.Background(), docs, chunks, embed,
		model.SourceLLMMemory, "Test Note", "Some content.", "", nil, true, true)

	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if result.EmbeddingCreated {
		t.Error("expected EmbeddingCreated=false when embedder is disabled")
	}
	if embed.embedCalls != 0 {
		t.Errorf("EmbedBatch called %d times, want 0 when embedder disabled", embed.embedCalls)
	}
}

// TestSave_SourceIDAutoGenerated verifies that an empty sourceID is replaced
// with a generated UUID. Ported from TestHandleAddNote_SourceIDAutoGenerated.
func TestSave_SourceIDAutoGenerated(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{}

	_, errMsg := Save(context.Background(), docs, chunks, embed,
		model.SourceLLMMemory, "Title", "Content.", "", nil, false, true)

	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if _, err := uuid.Parse(docs.lastDoc.SourceID); err != nil {
		t.Errorf("expected auto-generated source_id to be a valid UUID, got %q: %v",
			docs.lastDoc.SourceID, err)
	}
}

// TestSave_ExplicitSourceID_Preserved verifies that a supplied sourceID
// survives unchanged. Ported from TestHandleAddNote_ExplicitSourceID_Preserved.
func TestSave_ExplicitSourceID_Preserved(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{}

	const wantSourceID = "my-stable-note-id"

	_, errMsg := Save(context.Background(), docs, chunks, embed,
		model.SourceLLMMemory, "Title", "Content.", wantSourceID, nil, false, true)

	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if docs.lastDoc.SourceID != wantSourceID {
		t.Errorf("source_id = %q, want %q", docs.lastDoc.SourceID, wantSourceID)
	}
}

// TestSave_UpsertError_ReturnsError verifies that a document-store upsert
// failure propagates as a user-facing error.
// Ported from TestHandleAddNote_UpsertError_ReturnsError.
func TestSave_UpsertError_ReturnsError(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{upsertErr: errors.New("DB down")}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{}

	_, errMsg := Save(context.Background(), docs, chunks, embed,
		model.SourceLLMMemory, "Title", "Content.", "", nil, false, true)

	if errMsg == "" {
		t.Error("expected error when upsert fails, got none")
	}
}

// TestSave_ChunkReplaceError_ReturnsError verifies that a chunk-store
// ReplaceDocument failure propagates as a user-facing error.
// Ported from TestHandleAddNote_ChunkReplaceError_ReturnsError.
func TestSave_ChunkReplaceError_ReturnsError(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{replaceErr: errors.New("chunk store unavailable")}
	embed := &fakeEmbedder{}

	_, errMsg := Save(context.Background(), docs, chunks, embed,
		model.SourceLLMMemory, "Title", "Content.", "", nil, false, true)

	if errMsg == "" {
		t.Error("expected error when chunk replace fails, got none")
	}
}

// TestSave_EmbedError_NonFatal is the load-bearing regression test for the
// capture-backend contract: a note MUST survive being saved even when the
// embedding backend is down. If embedding errors are ever made fatal, this
// test must fail. Ported from TestHandleAddNote_EmbedError_NonFatal.
func TestSave_EmbedError_NonFatal(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{
		enabled:  true,
		embedErr: errors.New("embedding service timeout"),
	}

	result, errMsg := Save(context.Background(), docs, chunks, embed,
		model.SourceLLMMemory, "Title", "Content.", "", nil, true, true)

	// Embedding error must not propagate to the caller — the note must still
	// be considered saved.
	if errMsg != "" {
		t.Errorf("expected no error when embedding fails (non-fatal), got: %s", errMsg)
	}
	if result == nil {
		t.Fatal("expected non-nil result even when embedding fails")
	}
	if result.EmbeddingCreated {
		t.Error("expected EmbeddingCreated=false when embedding fails")
	}
	// The document itself must have been persisted despite the embedding
	// failure — this is the core of the "survives embedding outage" contract.
	if docs.lastDoc == nil {
		t.Error("expected the document to be upserted despite the embedding failure")
	}
	if chunks.replaceCalls != 1 {
		t.Errorf("ReplaceDocument called %d times, want 1 despite the embedding failure", chunks.replaceCalls)
	}
}

// TestSave_MetadataPropagated verifies that the caller-supplied metadata map
// reaches the persisted document. Ported from TestHandleAddNote_MetadataPropagated.
func TestSave_MetadataPropagated(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{}

	meta := map[string]any{"source": "test", "priority": float64(1)}

	_, errMsg := Save(context.Background(), docs, chunks, embed,
		model.SourceLLMMemory, "Title", "Content.", "", meta, false, true)

	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if docs.lastDoc.Metadata == nil {
		t.Fatal("expected metadata to be set on document, got nil")
	}
	if docs.lastDoc.Metadata["source"] != "test" {
		t.Errorf("metadata[source] = %v, want %q", docs.lastDoc.Metadata["source"], "test")
	}
}

// TestSave_TitleTooLong verifies the MaxTitleBytes upper boundary is
// rejected. Ported from TestHandleAddNote_TitleTooLong.
func TestSave_TitleTooLong(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{}

	longTitle := make([]byte, MaxTitleBytes+1)
	for i := range longTitle {
		longTitle[i] = 'x'
	}

	_, errMsg := Save(context.Background(), docs, chunks, embed,
		model.SourceLLMMemory, string(longTitle), "Some content.", "", nil, false, true)

	if errMsg == "" {
		t.Error("expected error for title exceeding MaxTitleBytes, got none")
	}
}

// TestSave_TitleAtLimit_Accepted verifies a title of exactly MaxTitleBytes
// is accepted (the boundary is inclusive).
// Ported from TestHandleAddNote_TitleAtLimit_Accepted.
func TestSave_TitleAtLimit_Accepted(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{}

	limitTitle := make([]byte, MaxTitleBytes)
	for i := range limitTitle {
		limitTitle[i] = 'x'
	}

	_, errMsg := Save(context.Background(), docs, chunks, embed,
		model.SourceLLMMemory, string(limitTitle), "Some content.", "", nil, false, true)

	if errMsg != "" {
		t.Errorf("expected no error for title at exactly MaxTitleBytes, got: %s", errMsg)
	}
}

// TestEmbedChunks_ExceedsLimit_ReturnsErrEmbedSkipped verifies that
// embedChunks returns ErrEmbedSkipped (not nil) when the chunk count exceeds
// MaxEmbedChunks, and that EmbedBatch is never called in that case.
// Ported from TestEmbedNoteChunks_ExceedsLimit_ReturnsErrEmbedSkipped.
func TestEmbedChunks_ExceedsLimit_ReturnsErrEmbedSkipped(t *testing.T) {
	t.Parallel()

	overLimit := make([]store.Chunk, MaxEmbedChunks+1)
	for i := range overLimit {
		overLimit[i] = store.Chunk{
			DocumentID: uuid.Nil,
			ChunkIndex: i,
			Content:    "x",
			ByteSize:   1,
		}
	}

	chunkW := &fakeChunkWriter{}
	embedder := &fakeEmbedder{enabled: true}

	err := embedChunks(context.Background(), uuid.Nil, overLimit, chunkW, embedder)

	if !errors.Is(err, ErrEmbedSkipped) {
		t.Errorf("want ErrEmbedSkipped, got %v", err)
	}
	if embedder.embedCalls != 0 {
		t.Errorf("EmbedBatch called %d times, want 0 when chunk count exceeds limit", embedder.embedCalls)
	}
}

// TestEmbedChunks_AtLimit_Embeds verifies a chunk count exactly equal to
// MaxEmbedChunks does NOT trigger the skip guard.
// Ported from TestEmbedNoteChunks_AtLimit_Embeds.
func TestEmbedChunks_AtLimit_Embeds(t *testing.T) {
	t.Parallel()

	atLimit := make([]store.Chunk, MaxEmbedChunks)
	for i := range atLimit {
		atLimit[i] = store.Chunk{
			DocumentID: uuid.Nil,
			ChunkIndex: i,
			Content:    "x",
			ByteSize:   1,
		}
	}

	// Prime ListByDocument with matching stored chunks so UpdateChunkEmbeddings succeeds.
	chunkW := &fakeChunkWriter{chunks: atLimit}
	for i := range chunkW.chunks {
		chunkW.chunks[i].ID = int64(i + 1)
	}
	embedder := &fakeEmbedder{enabled: true}

	err := embedChunks(context.Background(), uuid.Nil, atLimit, chunkW, embedder)

	if errors.Is(err, ErrEmbedSkipped) {
		t.Error("got ErrEmbedSkipped at exactly MaxEmbedChunks, want embedding to proceed")
	}
	if embedder.embedCalls != 1 {
		t.Errorf("EmbedBatch called %d times, want 1", embedder.embedCalls)
	}
}

// TestSave_EmbedSkippedSentinel_EmbeddingCreatedFalse verifies the
// end-to-end contract via Save: when embedChunks returns ErrEmbedSkipped,
// EmbeddingCreated must be false (not true) and no user-facing error is
// surfaced. Ported from TestHandleAddNote_EmbedSkippedSentinel_EmbeddingCreatedFalse.
//
// Implementation note (carried over from the original): Save builds
// chunkSlice from chunker.Split, so an over-limit slice cannot be injected
// directly here — that boundary is covered by TestEmbedChunks_ExceedsLimit_ReturnsErrEmbedSkipped
// and by TestSave_EmbedSkipped_ChunkCountExceedsLimit (real over-limit
// content). This test isolates the switch-case branch in Save via a
// controlled embedder that simulates the sentinel return.
func TestSave_EmbedSkippedSentinel_EmbeddingCreatedFalse(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunkW := &fakeChunkWriter{}
	embed := &skipSentinelEmbedder{}

	result, errMsg := Save(context.Background(), docs, chunkW, embed,
		model.SourceLLMMemory, "Title", "Content.", "", nil, true, true)

	if errMsg != "" {
		t.Fatalf("unexpected user-facing error: %s", errMsg)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.EmbeddingCreated {
		t.Error("EmbeddingCreated must be false when embedChunks returns ErrEmbedSkipped")
	}
}
