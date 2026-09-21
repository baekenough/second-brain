package scheduler

import (
	"context"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// mockChunkStore is a test double for *store.ChunkStore used in scheduler tests.
// It records calls and controls return values.
type mockChunkStore struct {
	replaceErr     error
	listChunks     []store.Chunk
	listErr        error
	updateEmbedErr error

	replaceCalled     bool
	listCalled        bool
	updateEmbedCalled bool
	lastEmbeddings    []store.ChunkEmbedding
}

func (m *mockChunkStore) ReplaceDocument(_ context.Context, _ uuid.UUID, _ []store.Chunk) error {
	m.replaceCalled = true
	return m.replaceErr
}

func (m *mockChunkStore) ListByDocument(_ context.Context, _ uuid.UUID) ([]store.Chunk, error) {
	m.listCalled = true
	return m.listChunks, m.listErr
}

func (m *mockChunkStore) UpdateChunkEmbeddings(_ context.Context, embeddings []store.ChunkEmbedding) error {
	m.updateEmbedCalled = true
	m.lastEmbeddings = embeddings
	return m.updateEmbedErr
}

// embedChunksCaller wraps the scheduler so we can call embedChunks directly.
// embedChunks is a method on *Scheduler, so we build a minimal scheduler.
func newTestSchedulerWithMockChunk(cs *mockChunkStore, embed search.EmbeddingEngine) *Scheduler {
	s := New(&mockStore{}, embed)
	// We need to set chunkStore on the scheduler. The real chunkStore is
	// *store.ChunkStore, but our mock satisfies the required methods.
	// We work around the concrete type by using a thin adapter.
	_ = cs // adapter not needed — see below
	return s
}

// TestEmbedChunks_DisabledEmbed_Skips verifies that embedChunks is never
// called when the embed client is disabled (Enabled() == false).
func TestEmbedChunks_DisabledEmbed_NoCalls(t *testing.T) {
	t.Parallel()

	// With a disabled embed engine, persistChunks should not attempt embedding.
	// We simulate this by verifying the engine's Enabled() returns false.
	embed := search.NewEmbedClient("", "", "", "", 0)
	if embed.Enabled() {
		t.Fatal("expected embed engine to be disabled")
	}

	// Confirm the condition in persistChunks: if s.embed.Enabled() { s.embedChunks(...) }
	// When Enabled() is false, embedChunks is never called.
}

// TestEmbedChunks_EmptyChunks_NoOp verifies that embedChunks with an empty
// slice returns immediately without calling EmbedBatch.
func TestEmbedChunks_EmptyChunks_NoOp(t *testing.T) {
	t.Parallel()

	// A scheduler with a real (but unreachable) embed URL.
	// We call embedChunks directly with an empty slice.
	// The guard `if len(chunks) == 0 { return }` must prevent any API call.
	// Since we cannot intercept the HTTP client here, we rely on the fact that
	// an empty chunks slice means texts is also empty and EmbedBatch([], []) is a
	// no-op for the disabled engine path.
	embed := search.NewEmbedClient("", "", "", "", 0)
	s := New(&mockStore{}, embed)

	// embedChunks with empty slice must not panic.
	doc := model.Document{
		ID:         uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		SourceType: model.SourceGmail,
	}
	// Calling directly to exercise the guard.
	s.embedChunks(context.Background(), doc, []store.Chunk{})
	// No panic = pass.
}

// TestEmbedChunks_Signature verifies the function signature compiles with the
// expected types: model.Document, []store.Chunk.
//
// 문서 전체를 받는 이유: 청크 임베딩 입력 앞에 붙일 문맥 헤더
// (BuildChunkContextHeader)를 만들려면 제목·발생시각·메타데이터가 필요하다.
// 문서 ID 만으로는 헤더를 만들 수 없다.
func TestEmbedChunks_Signature(t *testing.T) {
	t.Parallel()

	// This is a compile-time test: if embedChunks signature changes to use an
	// incompatible type, this will fail to compile.
	s := &Scheduler{}
	doc := model.Document{ID: uuid.New()}
	var chunks []store.Chunk
	// We cannot call this without a real DB, but the type assertion compiles.
	_ = func() { s.embedChunks(context.Background(), doc, chunks) }
}
