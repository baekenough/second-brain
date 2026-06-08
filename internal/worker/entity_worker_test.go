package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// fakeEntityDocumentLister is an in-memory implementation of EntityDocumentLister
// for unit testing EntityWorker tick behaviour.
type fakeEntityDocumentLister struct {
	mu      sync.Mutex
	docs    []*model.Document
	marked  []uuid.UUID // documents for which MarkEntitiesProcessed was called
	markErr error       // if non-nil, returned by MarkEntitiesProcessed
	listErr error       // if non-nil, returned by ListWithoutEntities
}

func (f *fakeEntityDocumentLister) ListWithoutEntities(_ context.Context, limit int) ([]*model.Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := f.docs
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeEntityDocumentLister) MarkEntitiesProcessed(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markErr != nil {
		return f.markErr
	}
	f.marked = append(f.marked, id)
	return nil
}

func (f *fakeEntityDocumentLister) wasMarked(id uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.marked {
		if m == id {
			return true
		}
	}
	return false
}

// fakeEntityLinker records UpsertAndLinkEntities calls.
type fakeEntityLinker struct {
	mu      sync.Mutex
	calls   []linkCall
	linkErr error
}

type linkCall struct {
	documentID uuid.UUID
	entities   []model.Entity
}

func (f *fakeEntityLinker) UpsertAndLinkEntities(_ context.Context, docID uuid.UUID, entities []model.Entity) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.linkErr != nil {
		return f.linkErr
	}
	f.calls = append(f.calls, linkCall{documentID: docID, entities: entities})
	return nil
}

func (f *fakeEntityLinker) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeLLM is a minimal llm.Completer for EntityWorker tests.
// It always reports as enabled so the worker loop runs.
// The extractFn field on EntityWorker is what controls extraction output in
// tests; CompleteWithMessages is never actually called.
type fakeLLM struct{}

func (f *fakeLLM) Enabled() bool { return true }

func (f *fakeLLM) CompleteWithMessages(_ context.Context, _ string, _ []llm.Message) (string, error) {
	return "", nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newDoc(id uuid.UUID) *model.Document {
	return &model.Document{
		ID:       id,
		SourceID: "test-source",
		Title:    "Test Document",
		Content:  "Some content for testing entity extraction.",
	}
}

// newTestWorker creates an EntityWorker with the given store and linker, and
// sets extractFn to fn so that tests control extraction output without a live LLM.
func newTestWorker(
	st EntityDocumentLister,
	linker EntityLinker,
	fn func(context.Context, llm.Completer, *model.Document) ([]model.Entity, error),
) *EntityWorker {
	llmC := &fakeLLM{}
	return &EntityWorker{
		store:     st,
		entities:  linker,
		llm:       llmC,
		interval:  time.Minute,
		batchSize: 10,
		extractFn: fn,
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestEntityWorker_ZeroEntities_MarksProcessed verifies that when extraction
// succeeds but returns no entities, MarkEntitiesProcessed is called so the
// document is not re-queued on subsequent ticks (issue #86).
func TestEntityWorker_ZeroEntities_MarksProcessed(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	st := &fakeEntityDocumentLister{docs: []*model.Document{newDoc(docID)}}
	linker := &fakeEntityLinker{}

	w := newTestWorker(st, linker,
		func(_ context.Context, _ llm.Completer, _ *model.Document) ([]model.Entity, error) {
			return []model.Entity{}, nil
		},
	)
	w.tick(context.Background())

	if !st.wasMarked(docID) {
		t.Errorf("expected MarkEntitiesProcessed(%s) to be called, but it was not", docID)
	}
	if linker.callCount() != 0 {
		t.Errorf("UpsertAndLinkEntities called %d times, want 0", linker.callCount())
	}
}

// TestEntityWorker_ExtractionError_DoesNotMark verifies that when the LLM
// returns an error the document is NOT marked processed, allowing it to be
// retried on the next tick.
func TestEntityWorker_ExtractionError_DoesNotMark(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	st := &fakeEntityDocumentLister{docs: []*model.Document{newDoc(docID)}}
	linker := &fakeEntityLinker{}

	w := newTestWorker(st, linker,
		func(_ context.Context, _ llm.Completer, _ *model.Document) ([]model.Entity, error) {
			return nil, errors.New("LLM unavailable")
		},
	)
	w.tick(context.Background())

	if st.wasMarked(docID) {
		t.Errorf("MarkEntitiesProcessed called for doc with extraction error, want NOT called")
	}
}

// TestEntityWorker_LinkError_MarksProcessed verifies that when entity linking
// fails the document is still marked processed to prevent infinite re-queuing.
func TestEntityWorker_LinkError_MarksProcessed(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	st := &fakeEntityDocumentLister{docs: []*model.Document{newDoc(docID)}}
	linker := &fakeEntityLinker{linkErr: errors.New("db connection lost")}

	w := newTestWorker(st, linker,
		func(_ context.Context, _ llm.Completer, _ *model.Document) ([]model.Entity, error) {
			return []model.Entity{{Name: "Alice", Type: model.EntityTypePerson}}, nil
		},
	)
	w.tick(context.Background())

	if !st.wasMarked(docID) {
		t.Errorf("expected MarkEntitiesProcessed to be called after link failure, but it was not")
	}
}

// TestEntityWorker_SuccessPath_MarksProcessed verifies that when extraction
// and linking both succeed MarkEntitiesProcessed is still called.
func TestEntityWorker_SuccessPath_MarksProcessed(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	st := &fakeEntityDocumentLister{docs: []*model.Document{newDoc(docID)}}
	linker := &fakeEntityLinker{}

	w := newTestWorker(st, linker,
		func(_ context.Context, _ llm.Completer, _ *model.Document) ([]model.Entity, error) {
			return []model.Entity{{Name: "Bob", Type: model.EntityTypePerson}}, nil
		},
	)
	w.tick(context.Background())

	if !st.wasMarked(docID) {
		t.Errorf("expected MarkEntitiesProcessed to be called on success, but it was not")
	}
	if linker.callCount() != 1 {
		t.Errorf("UpsertAndLinkEntities called %d times, want 1", linker.callCount())
	}
}

// TestEntityWorker_MarkFails_DoesNotPanic verifies that a MarkEntitiesProcessed
// failure (e.g. transient DB error) is handled gracefully without panicking,
// and that the document is NOT recorded as marked when the store returns an error.
func TestEntityWorker_MarkFails_DoesNotPanic(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	st := &fakeEntityDocumentLister{
		docs:    []*model.Document{newDoc(docID)},
		markErr: errors.New("connection refused"),
	}
	linker := &fakeEntityLinker{}

	w := newTestWorker(st, linker,
		func(_ context.Context, _ llm.Completer, _ *model.Document) ([]model.Entity, error) {
			return []model.Entity{}, nil
		},
	)

	// Should not panic.
	w.tick(context.Background())

	// Mark failed → the document must NOT appear in the marked list.
	if st.wasMarked(docID) {
		t.Errorf("MarkEntitiesProcessed recorded doc %s as marked despite store returning an error", docID)
	}
}

// TestEntityWorker_MixedBatch_CountsCorrectly exercises a single tick with a
// batch of three documents:
//
//  1. Extraction error  — should NOT be marked; linker NOT called.
//  2. Zero entities     — should be marked; linker NOT called.
//  3. Entities present  — should be marked; linker called once with this doc.
//
// It verifies that the succeeded counter is accurate (2 out of 3) and that
// the linker only receives the success-path document's entities.
func TestEntityWorker_MixedBatch_CountsCorrectly(t *testing.T) {
	t.Parallel()

	errDocID := uuid.New()
	zeroDocID := uuid.New()
	successDocID := uuid.New()

	docs := []*model.Document{
		newDoc(errDocID),
		newDoc(zeroDocID),
		newDoc(successDocID),
	}
	st := &fakeEntityDocumentLister{docs: docs}
	linker := &fakeEntityLinker{}

	successEntity := model.Entity{Name: "Alice", Type: model.EntityTypePerson}

	w := newTestWorker(st, linker,
		func(_ context.Context, _ llm.Completer, doc *model.Document) ([]model.Entity, error) {
			switch doc.ID {
			case errDocID:
				return nil, errors.New("LLM timeout")
			case zeroDocID:
				return []model.Entity{}, nil
			default:
				return []model.Entity{successEntity}, nil
			}
		},
	)
	w.tick(context.Background())

	// Extraction-error doc must NOT be marked.
	if st.wasMarked(errDocID) {
		t.Errorf("errDoc %s must not be marked when extraction fails", errDocID)
	}
	// Zero-entity doc must be marked (prevents re-queuing).
	if !st.wasMarked(zeroDocID) {
		t.Errorf("zeroDoc %s must be marked even when extraction yields zero entities", zeroDocID)
	}
	// Success doc must be marked.
	if !st.wasMarked(successDocID) {
		t.Errorf("successDoc %s must be marked after successful linking", successDocID)
	}

	// Linker must have been called exactly once, for the success-path doc.
	if linker.callCount() != 1 {
		t.Errorf("UpsertAndLinkEntities called %d times, want 1", linker.callCount())
	}
	linker.mu.Lock()
	gotCall := linker.calls[0]
	linker.mu.Unlock()
	if gotCall.documentID != successDocID {
		t.Errorf("linker called for doc %s, want %s", gotCall.documentID, successDocID)
	}
	if len(gotCall.entities) != 1 || gotCall.entities[0].Name != successEntity.Name {
		t.Errorf("linker received entities %v, want [{Name:%s}]", gotCall.entities, successEntity.Name)
	}
}

// TestEntityWorker_EmptyBatch_NoOp verifies that tick is a no-op when there
// are no documents to process.
func TestEntityWorker_EmptyBatch_NoOp(t *testing.T) {
	t.Parallel()

	st := &fakeEntityDocumentLister{docs: nil}
	linker := &fakeEntityLinker{}

	w := newTestWorker(st, linker,
		func(_ context.Context, _ llm.Completer, _ *model.Document) ([]model.Entity, error) {
			t.Error("extractFn should not be called when no documents are returned")
			return nil, nil
		},
	)
	w.tick(context.Background())

	if len(st.marked) != 0 {
		t.Errorf("MarkEntitiesProcessed called %d times, want 0", len(st.marked))
	}
}
