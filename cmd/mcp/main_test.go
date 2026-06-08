package main

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// fakeDocUpserter records Upsert calls and controls the returned error.
type fakeDocUpserter struct {
	upsertErr error
	// lastDoc is the most recent document passed to Upsert.
	lastDoc *model.Document
}

func (f *fakeDocUpserter) Upsert(_ context.Context, doc *model.Document) error {
	if doc.ID == (uuid.UUID{}) {
		doc.ID = uuid.New()
	}
	f.lastDoc = doc
	return f.upsertErr
}

// fakeChunkWriter records ReplaceDocument and UpdateChunkEmbeddings calls.
type fakeChunkWriter struct {
	replaceErr  error
	updateErr   error
	chunks      []store.Chunk // returned by ListByDocument
	listErr     error
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

// fakeEmbedder controls Enabled() and EmbedBatch output.
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
	// Return zero vectors matching the input count.
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{0.1, 0.2}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// mcpAuthContextFunc tests
// ---------------------------------------------------------------------------

func TestMCPAuthContextFunc_Disabled_AllowsAll(t *testing.T) {
	t.Parallel()

	fn := mcpAuthContextFunc("") // no API key → auth disabled
	ctx := fn(context.Background(), fakeHTTPRequest(""))
	if !isAuthorized(ctx) {
		t.Error("expected isAuthorized=true when no API key is configured")
	}
}

func TestMCPAuthContextFunc_CorrectToken_Authorized(t *testing.T) {
	t.Parallel()

	fn := mcpAuthContextFunc("secret-key")
	ctx := fn(context.Background(), fakeHTTPRequest("Bearer secret-key"))
	if !isAuthorized(ctx) {
		t.Error("expected isAuthorized=true for correct token")
	}
}

func TestMCPAuthContextFunc_WrongToken_Unauthorized(t *testing.T) {
	t.Parallel()

	fn := mcpAuthContextFunc("secret-key")
	ctx := fn(context.Background(), fakeHTTPRequest("Bearer wrong-key"))
	if isAuthorized(ctx) {
		t.Error("expected isAuthorized=false for wrong token")
	}
}

func TestMCPAuthContextFunc_NoHeader_Unauthorized(t *testing.T) {
	t.Parallel()

	fn := mcpAuthContextFunc("secret-key")
	ctx := fn(context.Background(), fakeHTTPRequest(""))
	if isAuthorized(ctx) {
		t.Error("expected isAuthorized=false when Authorization header is absent")
	}
}

func TestMCPAuthContextFunc_NonBearerScheme_Unauthorized(t *testing.T) {
	t.Parallel()

	fn := mcpAuthContextFunc("secret-key")
	ctx := fn(context.Background(), fakeHTTPRequest("Basic secret-key"))
	if isAuthorized(ctx) {
		t.Error("expected isAuthorized=false for non-Bearer scheme")
	}
}

// ---------------------------------------------------------------------------
// handleAddNote tests
// ---------------------------------------------------------------------------

func TestHandleAddNote_Success_EmbedTrue(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{enabled: true}

	result, errMsg := handleAddNote(
		context.Background(),
		docs, chunks, embed,
		"Test Note", "Some content for the note.",
		"", nil, true,
	)

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
		t.Error("expected EmbeddingCreated=true when embed=true and embedder enabled")
	}
	if chunks.replaceCalls != 1 {
		t.Errorf("ReplaceDocument called %d times, want 1", chunks.replaceCalls)
	}
	if embed.embedCalls != 1 {
		t.Errorf("EmbedBatch called %d times, want 1", embed.embedCalls)
	}
}

func TestHandleAddNote_Success_EmbedFalse(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{enabled: true}

	result, errMsg := handleAddNote(
		context.Background(),
		docs, chunks, embed,
		"Test Note", "Some content for the note.",
		"", nil, false,
	)

	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if result.EmbeddingCreated {
		t.Error("expected EmbeddingCreated=false when embed=false")
	}
	if embed.embedCalls != 0 {
		t.Errorf("EmbedBatch called %d times, want 0 when embed=false", embed.embedCalls)
	}
}

func TestHandleAddNote_EmbedDisabled_SkipsEmbedding(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{enabled: false} // embedder disabled

	result, errMsg := handleAddNote(
		context.Background(),
		docs, chunks, embed,
		"Test Note", "Some content.", "", nil, true,
	)

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

func TestHandleAddNote_EmptyTitle_ReturnsError(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{}

	_, errMsg := handleAddNote(
		context.Background(),
		docs, chunks, embed,
		"  ", "Some content.", "", nil, true,
	)

	if errMsg == "" {
		t.Error("expected error for empty title, got none")
	}
}

func TestHandleAddNote_EmptyContent_ReturnsError(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{}

	_, errMsg := handleAddNote(
		context.Background(),
		docs, chunks, embed,
		"Title", "  ", "", nil, true,
	)

	if errMsg == "" {
		t.Error("expected error for empty content, got none")
	}
}

func TestHandleAddNote_ContentTooLarge_ReturnsError(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{}

	bigContent := make([]byte, maxNoteContentBytes+1)
	for i := range bigContent {
		bigContent[i] = 'a'
	}

	_, errMsg := handleAddNote(
		context.Background(),
		docs, chunks, embed,
		"Title", string(bigContent), "", nil, true,
	)

	if errMsg == "" {
		t.Error("expected error for oversized content, got none")
	}
}

func TestHandleAddNote_SourceIDAutoGenerated(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{}

	_, errMsg := handleAddNote(
		context.Background(),
		docs, chunks, embed,
		"Title", "Content.", "", nil, false,
	)

	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	// Verify the auto-generated source_id is a valid UUID.
	if _, err := uuid.Parse(docs.lastDoc.SourceID); err != nil {
		t.Errorf("expected auto-generated source_id to be a valid UUID, got %q: %v",
			docs.lastDoc.SourceID, err)
	}
}

func TestHandleAddNote_ExplicitSourceID_Preserved(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{}

	const wantSourceID = "my-stable-note-id"

	_, errMsg := handleAddNote(
		context.Background(),
		docs, chunks, embed,
		"Title", "Content.", wantSourceID, nil, false,
	)

	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if docs.lastDoc.SourceID != wantSourceID {
		t.Errorf("source_id = %q, want %q", docs.lastDoc.SourceID, wantSourceID)
	}
}

func TestHandleAddNote_UpsertError_ReturnsError(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{upsertErr: errors.New("DB down")}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{}

	_, errMsg := handleAddNote(
		context.Background(),
		docs, chunks, embed,
		"Title", "Content.", "", nil, false,
	)

	if errMsg == "" {
		t.Error("expected error when upsert fails, got none")
	}
}

func TestHandleAddNote_ChunkReplaceError_ReturnsError(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{replaceErr: errors.New("chunk store unavailable")}
	embed := &fakeEmbedder{}

	_, errMsg := handleAddNote(
		context.Background(),
		docs, chunks, embed,
		"Title", "Content.", "", nil, false,
	)

	if errMsg == "" {
		t.Error("expected error when chunk replace fails, got none")
	}
}

func TestHandleAddNote_EmbedError_NonFatal(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{
		enabled:  true,
		embedErr: errors.New("embedding service timeout"),
	}

	result, errMsg := handleAddNote(
		context.Background(),
		docs, chunks, embed,
		"Title", "Content.", "", nil, true,
	)

	// Embedding error must not propagate to the caller.
	if errMsg != "" {
		t.Errorf("expected no error when embedding fails (non-fatal), got: %s", errMsg)
	}
	if result == nil {
		t.Fatal("expected non-nil result even when embedding fails")
	}
	if result.EmbeddingCreated {
		t.Error("expected EmbeddingCreated=false when embedding fails")
	}
}

func TestHandleAddNote_MetadataPropagated(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{}

	meta := map[string]any{"source": "test", "priority": float64(1)}

	_, errMsg := handleAddNote(
		context.Background(),
		docs, chunks, embed,
		"Title", "Content.", "", meta, false,
	)

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

func TestHandleAddNote_TitleTooLong(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{}

	longTitle := make([]byte, maxNoteTitleBytes+1)
	for i := range longTitle {
		longTitle[i] = 'x'
	}

	_, errMsg := handleAddNote(
		context.Background(),
		docs, chunks, embed,
		string(longTitle), "Some content.", "", nil, false,
	)

	if errMsg == "" {
		t.Error("expected error for title exceeding maxNoteTitleBytes, got none")
	}
}

func TestHandleAddNote_TitleAtLimit_Accepted(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{}

	limitTitle := make([]byte, maxNoteTitleBytes)
	for i := range limitTitle {
		limitTitle[i] = 'x'
	}

	_, errMsg := handleAddNote(
		context.Background(),
		docs, chunks, embed,
		string(limitTitle), "Some content.", "", nil, false,
	)

	if errMsg != "" {
		t.Errorf("expected no error for title at exactly maxNoteTitleBytes, got: %s", errMsg)
	}
}

func TestHandleAddNote_SourceTypeLLMMemory(t *testing.T) {
	t.Parallel()

	docs := &fakeDocUpserter{}
	chunks := &fakeChunkWriter{}
	embed := &fakeEmbedder{}

	_, errMsg := handleAddNote(
		context.Background(),
		docs, chunks, embed,
		"Title", "Content.", "", nil, false,
	)

	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if docs.lastDoc.SourceType != model.SourceLLMMemory {
		t.Errorf("source_type = %q, want %q", docs.lastDoc.SourceType, model.SourceLLMMemory)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// fakeHTTPRequest builds a minimal *http.Request with the given Authorization
// header value. An empty string means no header is set.
func fakeHTTPRequest(authz string) *http.Request {
	req, _ := http.NewRequest(http.MethodPost, "/mcp", nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	return req
}
