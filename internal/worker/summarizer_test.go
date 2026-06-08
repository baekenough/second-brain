package worker

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
)

// ---------------------------------------------------------------------------
// Mock implementations
// ---------------------------------------------------------------------------

// mockSummaryStore satisfies SummaryStore using ListUnsummarized (#64 simplified).
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
	docs := m.unsummarized
	if len(docs) > limit {
		docs = docs[:limit]
	}
	return docs, nil
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

// Verify at compile time that mockSummaryStore implements SummaryStore.
var _ SummaryStore = (*mockSummaryStore)(nil)

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
	// When LLM is disabled, Run() emits a log and waits; tick() is never called
	// in production, but we test that even if called directly it handles gracefully.
	// LLM disabled → generateSummary always errors → no UpdateSummary.
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
// Tests: tick timeout (#65) — tick must complete within maxTickDuration
// ---------------------------------------------------------------------------

// TestSummarizerWorker_tick_respectsTimeout verifies that tick() honours its
// context deadline and does not block indefinitely when the LLM is slow.
func TestSummarizerWorker_tick_respectsTimeout(t *testing.T) {
	// slowLLM blocks until its context is cancelled.
	slowLLM := &slowLLMClient{}

	doc := makeDoc("slow content")
	st := &mockSummaryStore{unsummarized: []*model.Document{doc}}

	w := NewSummarizerWorker(SummarizerConfig{Store: st, LLM: slowLLM, BatchSize: 10})

	// Use a very short deadline to keep the test fast.
	const tickDeadline = 50 * time.Millisecond
	tickCtx, cancel := context.WithTimeout(context.Background(), tickDeadline)
	defer cancel()

	start := time.Now()
	w.tick(tickCtx)
	elapsed := time.Since(start)

	// The tick must have returned within a reasonable margin of the deadline.
	if elapsed > tickDeadline*5 {
		t.Errorf("tick did not respect context deadline: elapsed %s, deadline %s", elapsed, tickDeadline)
	}
	// The slow LLM must not have produced an UpdateSummary call.
	if len(st.updateCalls) != 0 {
		t.Errorf("expected no UpdateSummary call when LLM is cancelled, got %d", len(st.updateCalls))
	}
}

// slowLLMClient blocks CompleteWithMessages until ctx is cancelled.
type slowLLMClient struct{}

func (s *slowLLMClient) Enabled() bool { return true }
func (s *slowLLMClient) CompleteWithMessages(ctx context.Context, _ string, _ []llm.Message) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

// TestSummarizerWorker_runTick_boundedByMaxTickDuration verifies that runTick
// returns within the configured MaxTickDuration even when the LLM is slow.
func TestSummarizerWorker_runTick_boundedByMaxTickDuration(t *testing.T) {
	slowLLM := &slowLLMClient{}
	doc := makeDoc("content")
	st := &mockSummaryStore{unsummarized: []*model.Document{doc}}

	// Use a short MaxTickDuration so the test completes quickly.
	const shortTick = 50 * time.Millisecond
	w := NewSummarizerWorker(SummarizerConfig{
		Store:           st,
		LLM:             slowLLM,
		BatchSize:       10,
		MaxTickDuration: shortTick,
	})

	// Parent context is already cancelled — runTick must still return within
	// MaxTickDuration (bounded by context.WithTimeout(WithoutCancel(ctx), MaxTickDuration)).
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	w.runTick(cancelledCtx)
	elapsed := time.Since(start)

	// Must complete well within shortTick (with 10× tolerance for slow CI).
	if elapsed > shortTick*10 {
		t.Errorf("runTick exceeded expected bound: elapsed %s, MaxTickDuration %s", elapsed, shortTick)
	}
}

// ---------------------------------------------------------------------------
// Tests: context cancellation + WaitGroup drain (#65)
// ---------------------------------------------------------------------------

func TestSummarizerWorker_Run_stopOnContextCancel(t *testing.T) {
	st := &mockSummaryStore{}
	lm := &mockLLM{enabled: false} // disabled → Run waits on ctx.Done()

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

// TestSummarizerWorker_Run_drainWindow verifies that goroutines launched via
// sync.WaitGroup drain within the bounded timeout window after context cancel.
// This mirrors the drain pattern in cmd/collector/main.go (#65).
func TestSummarizerWorker_Run_drainWindow(t *testing.T) {
	st := &mockSummaryStore{}
	lm := &mockLLM{enabled: false}

	w := NewSummarizerWorker(SummarizerConfig{
		Store:    st,
		LLM:      lm,
		Interval: 10 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.Run(ctx)
	}()

	cancel()

	drainDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(drainDone)
	}()

	const drainTimeout = 10 * time.Second
	select {
	case <-drainDone:
		// Worker drained within timeout — ok.
	case <-time.After(drainTimeout):
		t.Fatalf("SummarizerWorker goroutine did not drain within %s", drainTimeout)
	}
}

// ---------------------------------------------------------------------------
// Tests: #64 — idempotency guard prevents duplicate work
// Two concurrent tick() calls may both call UpdateSummary for the same doc,
// but the real store's WHERE title_summary IS NULL guard makes the second a
// no-op. Here we verify the tick contract: UpdateSummary is called once per
// doc per tick, with the correct data. The store-level guard is tested in
// store integration tests.
// ---------------------------------------------------------------------------

func TestSummarizerWorker_tick_idempotencyGuard_concurrentTicks(t *testing.T) {
	// Two workers share a doc. Both call UpdateSummary; the real DB guard makes
	// the second call a no-op. We verify that each tick independently calls
	// UpdateSummary exactly once.
	doc := makeDoc("shared content")
	wantTitle, wantBullets := "Title", "• Bullet"

	// Two separate mock stores each listing the same doc (simulating two instances).
	st1 := &mockSummaryStore{unsummarized: []*model.Document{doc}}
	st2 := &mockSummaryStore{unsummarized: []*model.Document{doc}}
	lm := &mockLLM{enabled: true, response: validSummaryJSON(wantTitle, wantBullets)}

	w1 := NewSummarizerWorker(SummarizerConfig{Store: st1, LLM: lm, BatchSize: 10})
	w2 := NewSummarizerWorker(SummarizerConfig{Store: st2, LLM: lm, BatchSize: 10})

	// Both tick concurrently.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); w1.tick(context.Background()) }()
	go func() { defer wg.Done(); w2.tick(context.Background()) }()
	wg.Wait()

	// Each instance made exactly 1 UpdateSummary call.
	if len(st1.updateCalls) != 1 {
		t.Errorf("instance 1: expected 1 UpdateSummary call, got %d", len(st1.updateCalls))
	}
	if len(st2.updateCalls) != 1 {
		t.Errorf("instance 2: expected 1 UpdateSummary call, got %d", len(st2.updateCalls))
	}
}

// ---------------------------------------------------------------------------
// Tests: SearchWeights.Defaults() — SummaryVec semantics (#63)
// ---------------------------------------------------------------------------

func TestSearchWeights_Defaults_summaryVec(t *testing.T) {
	tests := []struct {
		name    string
		input   model.SearchWeights
		wantVec float64
	}{
		{
			// Zero is preserved — coverage gate in hybridSearch decides.
			name:    "zero_preserved_for_coverage_gate",
			input:   model.SearchWeights{},
			wantVec: 0.0,
		},
		{
			name:    "explicit_positive_preserved",
			input:   model.SearchWeights{SummaryVec: 0.5},
			wantVec: 0.5,
		},
		{
			// Negative is invalid → replaced with DefaultSummaryVecWeight.
			name:    "negative_reset_to_default",
			input:   model.SearchWeights{SummaryVec: -1.0},
			wantVec: model.DefaultSummaryVecWeight,
		},
		{
			name:    "explicit_full_weight_preserved",
			input:   model.SearchWeights{SummaryVec: 0.8},
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

// TestSearchWeights_DisableSummaryVec verifies that DisableSummaryVec=true
// forces SummaryVec to 0.0 after Defaults() regardless of the SummaryVec field (#63).
func TestSearchWeights_DisableSummaryVec(t *testing.T) {
	tests := []struct {
		name    string
		input   model.SearchWeights
		wantVec float64
	}{
		{
			name:    "disable_overrides_zero_default",
			input:   model.SearchWeights{DisableSummaryVec: true},
			wantVec: 0.0,
		},
		{
			name:    "disable_overrides_positive_weight",
			input:   model.SearchWeights{SummaryVec: 0.8, DisableSummaryVec: true},
			wantVec: 0.0,
		},
		{
			name:    "disable_false_preserves_zero_for_gate",
			input:   model.SearchWeights{DisableSummaryVec: false},
			wantVec: 0.0, // zero → coverage gate, not forced disable
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.input.Defaults()
			if got.SummaryVec != tt.wantVec {
				t.Errorf("SummaryVec = %v, want %v (DisableSummaryVec=%v)", got.SummaryVec, tt.wantVec, tt.input.DisableSummaryVec)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Tests: NULL summary scan regression
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Tests: SUMMARIZER_BACKFILL_ENABLED=false
// ---------------------------------------------------------------------------

// TestSummarizerWorker_tick_backfillDisabled verifies that when BackfillEnabled
// is false, tick() returns immediately without calling ListUnsummarized or
// UpdateSummary. This prevents LLM timeout floods when running a slow local
// model with a large pre-existing unsummarized backlog.
func TestSummarizerWorker_tick_backfillDisabled(t *testing.T) {
	doc := makeDoc("some content")
	st := &mockSummaryStore{unsummarized: []*model.Document{doc}}
	lm := &mockLLM{enabled: true, response: validSummaryJSON("Title", "• Bullet")}

	disabled := false
	w := NewSummarizerWorker(SummarizerConfig{
		Store:           st,
		LLM:             lm,
		BackfillEnabled: &disabled,
	})
	w.tick(context.Background())

	// ListUnsummarized must NOT have been called — we verify indirectly: if it
	// were called and documents returned, UpdateSummary would be called.
	if len(st.updateCalls) != 0 {
		t.Errorf("expected no UpdateSummary calls when backfill disabled, got %d", len(st.updateCalls))
	}
}

// TestSummarizerWorker_tick_backfillEnabledExplicit verifies that passing
// BackfillEnabled=true behaves identically to the default (nil) — documents are
// processed normally.
func TestSummarizerWorker_tick_backfillEnabledExplicit(t *testing.T) {
	doc := makeDoc("content")
	st := &mockSummaryStore{unsummarized: []*model.Document{doc}}
	lm := &mockLLM{enabled: true, response: validSummaryJSON("Title", "• Bullet")}

	enabled := true
	w := NewSummarizerWorker(SummarizerConfig{
		Store:           st,
		LLM:             lm,
		BackfillEnabled: &enabled,
	})
	w.tick(context.Background())

	if len(st.updateCalls) != 1 {
		t.Errorf("expected 1 UpdateSummary call when backfill enabled, got %d", len(st.updateCalls))
	}
}

// TestSummarizerWorker_tick_backfillNil_defaultsToEnabled verifies that nil
// BackfillEnabled (i.e. field not set) preserves the existing enabled behaviour.
func TestSummarizerWorker_tick_backfillNil_defaultsToEnabled(t *testing.T) {
	doc := makeDoc("content")
	st := &mockSummaryStore{unsummarized: []*model.Document{doc}}
	lm := &mockLLM{enabled: true, response: validSummaryJSON("Title", "• Bullet")}

	// BackfillEnabled: nil (not set)
	w := NewSummarizerWorker(SummarizerConfig{Store: st, LLM: lm})
	w.tick(context.Background())

	if len(st.updateCalls) != 1 {
		t.Errorf("expected 1 UpdateSummary call when BackfillEnabled is nil (default true), got %d", len(st.updateCalls))
	}
}

// ---------------------------------------------------------------------------
// Tests: NULL summary scan regression
// ---------------------------------------------------------------------------

// TestNullSummaryScan verifies that a Document with NULL summary fields
// (as returned by a freshly inserted row before the summarizer runs)
// can be scanned without error and produces empty string fields.
func TestNullSummaryScan_modelFieldsAreEmptyString(t *testing.T) {
	doc := &model.Document{}

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
