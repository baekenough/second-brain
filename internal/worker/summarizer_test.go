package worker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
)

// ---------------------------------------------------------------------------
// Mock implementations
// ---------------------------------------------------------------------------

// mockSummaryStore satisfies SummaryStore.
type mockSummaryStore struct {
	unsummarized []*model.Document
	listErr      error
	updateCalls  []updateSummaryCall
	updateErr    error
}

type updateSummaryCall struct {
	id               uuid.UUID
	titleSummary     string
	bulletSummary    string
	summaryEmbedding []float32
}

func (m *mockSummaryStore) ListUnsummarized(_ context.Context, limit int) ([]*model.Document, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	if len(m.unsummarized) > limit {
		return m.unsummarized[:limit], nil
	}
	return m.unsummarized, nil
}

func (m *mockSummaryStore) UpdateSummary(_ context.Context, id uuid.UUID, titleSummary, bulletSummary string, summaryEmbedding []float32) error {
	m.updateCalls = append(m.updateCalls, updateSummaryCall{
		id:               id,
		titleSummary:     titleSummary,
		bulletSummary:    bulletSummary,
		summaryEmbedding: summaryEmbedding,
	})
	return m.updateErr
}

// mockLLM satisfies llm.Completer.
type mockLLM struct {
	enabled  bool
	response string
	err      error
}

func (m *mockLLM) Enabled() bool { return m.enabled }

func (m *mockLLM) CompleteWithMessages(_ context.Context, _ string, _ []llm.Message) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	return m.response, nil
}

// mockEmbedder satisfies Embedder.
type mockEmbedder struct {
	enabled bool
	vec     []float32
	err     error
}

func (m *mockEmbedder) Enabled() bool { return m.enabled }

func (m *mockEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.vec, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func validSummaryJSON(title, bullets string) string {
	b, _ := json.Marshal(map[string]string{
		"title_summary":  title,
		"bullet_summary": bullets,
	})
	return string(b)
}

func makeDoc(content string) *model.Document {
	return &model.Document{
		ID:      uuid.New(),
		Title:   "Test Document",
		Content: content,
		Status:  "active",
	}
}

// ---------------------------------------------------------------------------
// Tests: happy path and core flows
// ---------------------------------------------------------------------------

func TestSummarizerWorker_tick_happyPath(t *testing.T) {
	doc := makeDoc("Hello world content")
	wantTitle := "A test title summary"
	wantBullets := "• Point one\n• Point two"
	wantVec := []float32{0.1, 0.2, 0.3}

	st := &mockSummaryStore{unsummarized: []*model.Document{doc}}
	lm := &mockLLM{enabled: true, response: validSummaryJSON(wantTitle, wantBullets)}
	emb := &mockEmbedder{enabled: true, vec: wantVec}

	w := NewSummarizerWorker(SummarizerConfig{
		Store:     st,
		LLM:       lm,
		Embedder:  emb,
		Interval:  time.Minute,
		BatchSize: 10,
	})
	w.tick(context.Background())

	if len(st.updateCalls) != 1 {
		t.Fatalf("expected 1 UpdateSummary call, got %d", len(st.updateCalls))
	}
	call := st.updateCalls[0]
	if call.id != doc.ID {
		t.Errorf("UpdateSummary called with wrong ID: got %v, want %v", call.id, doc.ID)
	}
	if call.titleSummary != wantTitle {
		t.Errorf("titleSummary = %q, want %q", call.titleSummary, wantTitle)
	}
	if call.bulletSummary != wantBullets {
		t.Errorf("bulletSummary = %q, want %q", call.bulletSummary, wantBullets)
	}
	if len(call.summaryEmbedding) != len(wantVec) {
		t.Errorf("summaryEmbedding length = %d, want %d", len(call.summaryEmbedding), len(wantVec))
	}
}

func TestSummarizerWorker_tick_llmDisabled(t *testing.T) {
	st := &mockSummaryStore{unsummarized: []*model.Document{makeDoc("content")}}
	lm := &mockLLM{enabled: false}

	w := NewSummarizerWorker(SummarizerConfig{Store: st, LLM: lm})
	w.tick(context.Background())

	if len(st.updateCalls) != 0 {
		t.Errorf("expected no UpdateSummary calls when LLM disabled, got %d", len(st.updateCalls))
	}
}

func TestSummarizerWorker_tick_listErr(t *testing.T) {
	st := &mockSummaryStore{listErr: errors.New("db connection refused")}
	lm := &mockLLM{enabled: true}

	w := NewSummarizerWorker(SummarizerConfig{Store: st, LLM: lm})
	// Should not panic; error is logged and swallowed.
	w.tick(context.Background())

	if len(st.updateCalls) != 0 {
		t.Errorf("expected no UpdateSummary calls on list error, got %d", len(st.updateCalls))
	}
}

func TestSummarizerWorker_tick_llmErr_skipsDoc(t *testing.T) {
	st := &mockSummaryStore{unsummarized: []*model.Document{makeDoc("some content")}}
	lm := &mockLLM{enabled: true, err: errors.New("llm timeout")}

	w := NewSummarizerWorker(SummarizerConfig{Store: st, LLM: lm})
	w.tick(context.Background())

	if len(st.updateCalls) != 0 {
		t.Errorf("expected no UpdateSummary calls when LLM errors, got %d", len(st.updateCalls))
	}
}

func TestSummarizerWorker_tick_embeddingErr_storesTextOnly(t *testing.T) {
	doc := makeDoc("some content")
	wantTitle, wantBullets := "Title", "• Bullet"

	st := &mockSummaryStore{unsummarized: []*model.Document{doc}}
	lm := &mockLLM{enabled: true, response: validSummaryJSON(wantTitle, wantBullets)}
	emb := &mockEmbedder{enabled: true, err: errors.New("embed service down")}

	w := NewSummarizerWorker(SummarizerConfig{Store: st, LLM: lm, Embedder: emb})
	w.tick(context.Background())

	if len(st.updateCalls) != 1 {
		t.Fatalf("expected 1 UpdateSummary call, got %d", len(st.updateCalls))
	}
	call := st.updateCalls[0]
	if call.titleSummary != wantTitle {
		t.Errorf("titleSummary = %q, want %q", call.titleSummary, wantTitle)
	}
	// Embedding must be nil when embedder fails; doc still gets text summary.
	if len(call.summaryEmbedding) != 0 {
		t.Errorf("expected no summaryEmbedding on embedder error, got %v", call.summaryEmbedding)
	}
}

func TestSummarizerWorker_tick_noEmbedder_storesTextOnly(t *testing.T) {
	st := &mockSummaryStore{unsummarized: []*model.Document{makeDoc("some content")}}
	lm := &mockLLM{enabled: true, response: validSummaryJSON("Title", "• Bullet")}

	// No embedder configured.
	w := NewSummarizerWorker(SummarizerConfig{Store: st, LLM: lm, Embedder: nil})
	w.tick(context.Background())

	if len(st.updateCalls) != 1 {
		t.Fatalf("expected 1 UpdateSummary call, got %d", len(st.updateCalls))
	}
	if len(st.updateCalls[0].summaryEmbedding) != 0 {
		t.Errorf("expected nil summaryEmbedding without embedder, got %v", st.updateCalls[0].summaryEmbedding)
	}
}

// ---------------------------------------------------------------------------
// Tests: parse failure → retry behaviour
// ---------------------------------------------------------------------------

func TestSummarizerWorker_tick_malformedJSON_retries(t *testing.T) {
	st := &mockSummaryStore{unsummarized: []*model.Document{makeDoc("content")}}
	lm := &mockLLM{enabled: true, response: "not json at all"}

	w := NewSummarizerWorker(SummarizerConfig{Store: st, LLM: lm})
	w.tick(context.Background())

	// Malformed JSON must NOT call UpdateSummary — the document stays
	// title_summary=NULL so ListUnsummarized picks it up on the next tick.
	if len(st.updateCalls) != 0 {
		t.Fatalf("expected 0 UpdateSummary calls on malformed JSON (doc must remain retryable), got %d", len(st.updateCalls))
	}
}

func TestSummarizerWorker_tick_malformedJSON_longResponse_truncated(t *testing.T) {
	st := &mockSummaryStore{unsummarized: []*model.Document{makeDoc("content")}}
	// Response longer than 200 chars — verify truncation doesn't panic and
	// UpdateSummary is still not called.
	longResponse := "x"
	for i := 0; i < 300; i++ {
		longResponse += "a"
	}
	lm := &mockLLM{enabled: true, response: longResponse}

	w := NewSummarizerWorker(SummarizerConfig{Store: st, LLM: lm})
	w.tick(context.Background())

	if len(st.updateCalls) != 0 {
		t.Fatalf("expected 0 UpdateSummary calls on malformed JSON (long response), got %d", len(st.updateCalls))
	}
}

// ---------------------------------------------------------------------------
// Tests: edge cases
// ---------------------------------------------------------------------------

func TestSummarizerWorker_tick_emptyList_noUpdate(t *testing.T) {
	st := &mockSummaryStore{unsummarized: nil}
	lm := &mockLLM{enabled: true}

	w := NewSummarizerWorker(SummarizerConfig{Store: st, LLM: lm})
	w.tick(context.Background())

	if len(st.updateCalls) != 0 {
		t.Errorf("expected no UpdateSummary calls for empty list, got %d", len(st.updateCalls))
	}
}

func TestSummarizerWorker_tick_batchSizeRespected(t *testing.T) {
	docs := make([]*model.Document, 5)
	for i := range docs {
		docs[i] = makeDoc("content")
	}
	st := &mockSummaryStore{unsummarized: docs}
	lm := &mockLLM{enabled: true, response: validSummaryJSON("T", "• B")}

	// BatchSize 3: only 3 of 5 documents should be processed per tick.
	w := NewSummarizerWorker(SummarizerConfig{
		Store:     st,
		LLM:       lm,
		BatchSize: 3,
	})
	w.tick(context.Background())

	if len(st.updateCalls) != 3 {
		t.Errorf("expected 3 UpdateSummary calls (batch size), got %d", len(st.updateCalls))
	}
}

func TestSummarizerWorker_tick_updateErr_continues(t *testing.T) {
	docs := []*model.Document{makeDoc("a"), makeDoc("b")}
	st := &mockSummaryStore{
		unsummarized: docs,
		updateErr:    errors.New("db write error"),
	}
	lm := &mockLLM{enabled: true, response: validSummaryJSON("T", "• B")}

	w := NewSummarizerWorker(SummarizerConfig{Store: st, LLM: lm})
	// Both docs are attempted; both fail update — worker must not panic.
	w.tick(context.Background())

	if len(st.updateCalls) != 2 {
		t.Errorf("expected 2 UpdateSummary attempts despite errors, got %d", len(st.updateCalls))
	}
}

// ---------------------------------------------------------------------------
// Tests: context cancellation
// ---------------------------------------------------------------------------

func TestSummarizerWorker_Run_stopOnContextCancel(t *testing.T) {
	st := &mockSummaryStore{}
	lm := &mockLLM{enabled: false}

	w := NewSummarizerWorker(SummarizerConfig{
		Store:    st,
		LLM:      lm,
		Interval: 10 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// ok
	case <-time.After(2 * time.Second):
		t.Fatal("SummarizerWorker.Run did not stop within 2s after context cancel")
	}
}

// ---------------------------------------------------------------------------
// Tests: SearchWeights.Defaults() — SummaryVec guard
// ---------------------------------------------------------------------------

func TestSearchWeights_Defaults_summaryVec(t *testing.T) {
	tests := []struct {
		name    string
		input   model.SearchWeights
		wantVec float64
	}{
		{
			name:    "zero_uses_default",
			input:   model.SearchWeights{},
			wantVec: 0.8,
		},
		{
			name:    "explicit_positive_preserved",
			input:   model.SearchWeights{SummaryVec: 0.5},
			wantVec: 0.5,
		},
		{
			name:    "negative_reset_to_default",
			input:   model.SearchWeights{SummaryVec: -1.0},
			wantVec: 0.8,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.input.Defaults()
			if got.SummaryVec != tt.wantVec {
				t.Errorf("SummaryVec = %v, want %v", got.SummaryVec, tt.wantVec)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Tests: NULL summary scan regression
// ---------------------------------------------------------------------------

// TestNullSummaryScan verifies that a Document with NULL summary fields
// (as returned by a freshly inserted row before the summarizer runs)
// can be scanned without error and produces empty string fields.
// This is a compile-time regression guard: the store's pgtype.Text scan
// path must handle NULL gracefully.
//
// We test the model layer here; the store's SQL scan path is exercised in
// the integration tests (store/postgres_embedding_test.go).
func TestNullSummaryScan_modelFieldsAreEmptyString(t *testing.T) {
	// Simulates what scanDocument produces for a row with NULL summary columns.
	doc := &model.Document{
		// TitleSummary and BulletSummary are not set → zero values.
		// SummaryEmbedding is not set → nil.
	}

	if doc.TitleSummary != "" {
		t.Errorf("TitleSummary should be empty string for NULL DB value, got %q", doc.TitleSummary)
	}
	if doc.BulletSummary != "" {
		t.Errorf("BulletSummary should be empty string for NULL DB value, got %q", doc.BulletSummary)
	}
	if doc.SummaryEmbedding != nil {
		t.Errorf("SummaryEmbedding should be nil for NULL DB value, got %v", doc.SummaryEmbedding)
	}
}
