package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// --- test doubles ---

type fakeNotesUpserter struct {
	docs         map[uuid.UUID]*model.Document
	upsertErr    error
	resetResult  bool
	resetErr     error
	deleteErr    error
	cascadeCount int
	cascadeErr   error
}

func newFakeNotesUpserter() *fakeNotesUpserter {
	return &fakeNotesUpserter{docs: map[uuid.UUID]*model.Document{}}
}

func (f *fakeNotesUpserter) Upsert(_ context.Context, doc *model.Document) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	if doc.ID == (uuid.UUID{}) {
		doc.ID = uuid.New()
	}
	f.docs[doc.ID] = doc
	return nil
}

func (f *fakeNotesUpserter) GetByID(_ context.Context, id uuid.UUID) (*model.Document, error) {
	doc, ok := f.docs[id]
	if !ok {
		return nil, errNotFoundStub
	}
	return doc, nil
}

func (f *fakeNotesUpserter) ResetNoteEnrichment(_ context.Context, _ uuid.UUID) (bool, error) {
	return f.resetResult, f.resetErr
}

func (f *fakeNotesUpserter) SoftDeleteByID(_ context.Context, id uuid.UUID) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if doc, ok := f.docs[id]; ok {
		doc.Status = "deleted"
	}
	return nil
}

func (f *fakeNotesUpserter) SoftDeleteInsightsByNoteID(_ context.Context, _ uuid.UUID) (int, error) {
	return f.cascadeCount, f.cascadeErr
}

// errNotFoundStub is a package-local sentinel; production GetByID
// implementations return pgx.ErrNoRows, but the handler only checks err !=
// nil (mirrors getDocumentHandler in document.go:68-72), so any non-nil
// error is sufficient here.
var errNotFoundStub = context.DeadlineExceeded

// newNotesTestServer wires a Server for the notes handlers only.
func newNotesTestServer(upserter *fakeNotesUpserter) *Server {
	srv := NewServer(nil, nil, nil, nil, nil, "", "test-key")
	srv.WithNotes(upserter, &stubIngestChunkWriter{}, &stubIngestEmbedder{enabled: false})
	return srv
}

func doNotesRequest(t *testing.T, srv *Server, method, path string, body any, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode request body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

// --- POST /api/v1/notes ---

func TestCreateNote_AuthRequired(t *testing.T) {
	t.Parallel()
	srv := newNotesTestServer(newFakeNotesUpserter())
	rr := doNotesRequest(t, srv, http.MethodPost, "/api/v1/notes", map[string]any{"content": "hi"}, "")
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusUnauthorized, rr.Body.String())
	}
}

func TestCreateNote_EmptyContent_Returns400(t *testing.T) {
	t.Parallel()
	srv := newNotesTestServer(newFakeNotesUpserter())
	rr := doNotesRequest(t, srv, http.MethodPost, "/api/v1/notes", map[string]any{"content": ""}, "Bearer test-key")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusBadRequest, rr.Body.String())
	}
}

func TestCreateNote_EmptyTitle_Accepted_Returns202(t *testing.T) {
	t.Parallel()
	upserter := newFakeNotesUpserter()
	srv := newNotesTestServer(upserter)
	rr := doNotesRequest(t, srv, http.MethodPost, "/api/v1/notes",
		map[string]any{"title": "", "content": "budget notes"}, "Bearer test-key")

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusAccepted, rr.Body.String())
	}
	var resp CreateNoteResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "pending" {
		t.Errorf("Status = %q, want %q", resp.Status, "pending")
	}
	id, err := uuid.Parse(resp.ID)
	if err != nil {
		t.Fatalf("invalid response ID: %v", err)
	}
	doc, ok := upserter.docs[id]
	if !ok {
		t.Fatal("note was not persisted")
	}
	if doc.SourceType != model.SourceNote {
		t.Errorf("SourceType = %q, want %q", doc.SourceType, model.SourceNote)
	}
	if doc.Title != "" {
		t.Errorf("Title = %q, want empty (no guessed title, spec §6.1)", doc.Title)
	}
	if doc.Metadata["enrichment_status"] != "pending" {
		t.Errorf("enrichment_status = %v, want %q", doc.Metadata["enrichment_status"], "pending")
	}
}

func TestCreateNote_ContentTooLarge_Returns413(t *testing.T) {
	t.Parallel()
	srv := newNotesTestServer(newFakeNotesUpserter())
	oversized := strings.Repeat("a", 10*1024*1024+1)
	rr := doNotesRequest(t, srv, http.MethodPost, "/api/v1/notes",
		map[string]any{"content": oversized}, "Bearer test-key")
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusRequestEntityTooLarge)
	}
}

// --- POST /api/v1/notes/{id}/retry-enrichment ---

func TestRetryEnrichment_AuthRequired(t *testing.T) {
	t.Parallel()
	srv := newNotesTestServer(newFakeNotesUpserter())
	id := uuid.New()
	rr := doNotesRequest(t, srv, http.MethodPost, "/api/v1/notes/"+id.String()+"/retry-enrichment", nil, "")
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
}

func TestRetryEnrichment_NotFound_Returns404(t *testing.T) {
	t.Parallel()
	srv := newNotesTestServer(newFakeNotesUpserter())
	id := uuid.New()
	rr := doNotesRequest(t, srv, http.MethodPost, "/api/v1/notes/"+id.String()+"/retry-enrichment", nil, "Bearer test-key")
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusNotFound, rr.Body.String())
	}
}

func TestRetryEnrichment_NotTerminal_Returns409(t *testing.T) {
	t.Parallel()
	upserter := newFakeNotesUpserter()
	id := uuid.New()
	upserter.docs[id] = &model.Document{ID: id, SourceType: model.SourceNote, Status: "active",
		Metadata: map[string]any{"enrichment_status": "pending"}}
	upserter.resetResult = false // ResetNoteEnrichment reports no matching terminal row
	srv := newNotesTestServer(upserter)

	rr := doNotesRequest(t, srv, http.MethodPost, "/api/v1/notes/"+id.String()+"/retry-enrichment", nil, "Bearer test-key")
	if rr.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusConflict, rr.Body.String())
	}
}

func TestRetryEnrichment_Terminal_Returns200(t *testing.T) {
	t.Parallel()
	upserter := newFakeNotesUpserter()
	id := uuid.New()
	upserter.docs[id] = &model.Document{ID: id, SourceType: model.SourceNote, Status: "active",
		Metadata: map[string]any{"enrichment_status": "failed"}}
	upserter.resetResult = true
	srv := newNotesTestServer(upserter)

	rr := doNotesRequest(t, srv, http.MethodPost, "/api/v1/notes/"+id.String()+"/retry-enrichment", nil, "Bearer test-key")
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
}

// --- DELETE /api/v1/notes/{id} ---

func TestDeleteNote_AuthRequired(t *testing.T) {
	t.Parallel()
	srv := newNotesTestServer(newFakeNotesUpserter())
	id := uuid.New()
	rr := doNotesRequest(t, srv, http.MethodDelete, "/api/v1/notes/"+id.String(), nil, "")
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
}

func TestDeleteNote_NotFound_Returns404(t *testing.T) {
	t.Parallel()
	srv := newNotesTestServer(newFakeNotesUpserter())
	id := uuid.New()
	rr := doNotesRequest(t, srv, http.MethodDelete, "/api/v1/notes/"+id.String(), nil, "Bearer test-key")
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusNotFound)
	}
}

// TestDeleteNote_Success_CascadesToInsights verifies the delete endpoint
// soft-deletes the note AND reports the cascaded insight count (spec §6.5 —
// the gap this plan closes: without this endpoint the cascade rule was
// unreachable).
func TestDeleteNote_Success_CascadesToInsights(t *testing.T) {
	t.Parallel()
	upserter := newFakeNotesUpserter()
	id := uuid.New()
	upserter.docs[id] = &model.Document{ID: id, SourceType: model.SourceNote, Status: "active"}
	upserter.cascadeCount = 2
	srv := newNotesTestServer(upserter)

	rr := doNotesRequest(t, srv, http.MethodDelete, "/api/v1/notes/"+id.String(), nil, "Bearer test-key")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["insights_deleted"].(float64) != 2 {
		t.Errorf("insights_deleted = %v, want 2", resp["insights_deleted"])
	}
	if upserter.docs[id].Status != "deleted" {
		t.Errorf("note Status = %q, want %q", upserter.docs[id].Status, "deleted")
	}
}

func TestDeleteNote_AlreadyDeleted_Returns409(t *testing.T) {
	t.Parallel()
	upserter := newFakeNotesUpserter()
	id := uuid.New()
	upserter.docs[id] = &model.Document{ID: id, SourceType: model.SourceNote, Status: "deleted"}
	srv := newNotesTestServer(upserter)

	rr := doNotesRequest(t, srv, http.MethodDelete, "/api/v1/notes/"+id.String(), nil, "Bearer test-key")
	if rr.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusConflict, rr.Body.String())
	}
}

// --- capture must not block on the embedding backend ---

// blockingNotesEmbedder records every EmbedBatch call and reports the backend
// as available but broken. If note capture ever embeds inline again, the
// recorded count is non-zero and the tests below fail.
type blockingNotesEmbedder struct {
	mu    sync.Mutex
	calls int
}

func (b *blockingNotesEmbedder) Enabled() bool { return true }

func (b *blockingNotesEmbedder) EmbedBatch(_ context.Context, _ []string) ([][]float32, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	return nil, errors.New("embedding backend unavailable")
}

func (b *blockingNotesEmbedder) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// TestCreateNote_DoesNotEmbedInline pins the binding contract: POST
// /api/v1/notes performs no remote embedding call, so a slow or degraded
// embedding endpoint cannot blow the request's write timeout and drive the
// client into a retry loop that mints a new note per attempt (each one
// independently enriched, each generating its own insights).
//
// The vectors are not lost — the scheduler's chunk-embedding backfill picks up
// chunks WHERE embedding IS NULL — so this asserts on the call count rather
// than on the absence of embeddings.
func TestCreateNote_DoesNotEmbedInline(t *testing.T) {
	t.Parallel()

	upserter := newFakeNotesUpserter()
	embedder := &blockingNotesEmbedder{}
	srv := NewServer(nil, nil, nil, nil, nil, "", "test-key")
	srv.WithNotes(upserter, &stubIngestChunkWriter{}, embedder)

	rr := doNotesRequest(t, srv, http.MethodPost, "/api/v1/notes",
		map[string]any{"content": strings.Repeat("budget notes. ", 500)}, "Bearer test-key")

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusAccepted, rr.Body.String())
	}
	if got := embedder.count(); got != 0 {
		t.Errorf("EmbedBatch called %d times during note capture, want 0 — capture must not block on a remote call", got)
	}
}

// TestCreateNote_EmbeddingBackendDown_StillPersists states the contract from
// the other side: with the embedding backend returning errors, the note is
// still durably stored and the client still gets its 202.
func TestCreateNote_EmbeddingBackendDown_StillPersists(t *testing.T) {
	t.Parallel()

	upserter := newFakeNotesUpserter()
	chunks := &stubIngestChunkWriter{}
	srv := NewServer(nil, nil, nil, nil, nil, "", "test-key")
	srv.WithNotes(upserter, chunks, &blockingNotesEmbedder{})

	rr := doNotesRequest(t, srv, http.MethodPost, "/api/v1/notes",
		map[string]any{"content": "the embedding backend is down"}, "Bearer test-key")

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusAccepted, rr.Body.String())
	}
	var resp CreateNoteResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	id, err := uuid.Parse(resp.ID)
	if err != nil {
		t.Fatalf("invalid response ID: %v", err)
	}
	doc, ok := upserter.docs[id]
	if !ok {
		t.Fatal("note was not persisted while the embedding backend was down")
	}
	if doc.Content != "the embedding backend is down" {
		t.Errorf("Content = %q, want the verbatim note text", doc.Content)
	}
	if doc.Metadata["enrichment_status"] != "pending" {
		t.Errorf("enrichment_status = %v, want %q", doc.Metadata["enrichment_status"], "pending")
	}
}
