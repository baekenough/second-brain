package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Deadline-model regression tests (C-1).
//
// Every test in this file exists because the previous single-per-document
// deadline let bookkeeping writes run on an already-expired context. When that
// happens the write is rejected by the driver before it reaches the database,
// the attempt counter never moves, and the note is re-listed — and re-billed —
// on every tick forever. The assertions therefore check TWO things each time:
// that the bookkeeping call happened at all, and that the context it was
// handed was still live (ctxErr == nil). Only the second assertion catches a
// regression to the shared-deadline shape, because a shared deadline still
// *calls* the store, it just calls it with a dead context.
// ---------------------------------------------------------------------------

// deadlineEnricher returns an enrichFn that blocks until its context expires
// and then reports the context error, exactly as a real LLM client does when
// the caller's deadline fires mid-request.
func deadlineEnricher() func(context.Context, llm.Completer, *model.Document) (*NoteEnrichmentResult, error) {
	return func(ctx context.Context, _ llm.Completer, _ *model.Document) (*NoteEnrichmentResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
}

// newDeadlineTestWorker builds a worker whose work budget expires almost
// immediately while its bookkeeping budget stays generous — the configuration
// that reproduces the production inversion (LLM_TIMEOUT_SECONDS=120 against a
// 60 s per-document budget) in microseconds instead of minutes.
func newDeadlineTestWorker(lister NoteEnrichmentLister, insights InsightWriter) *NoteEnrichmentWorker {
	w := newTestNoteWorker(lister, insights, &fakeEntityLinker{}, deadlineEnricher())
	w.workTimeoutV = time.Millisecond
	return w
}

// TestNoteEnrichmentWorker_WorkDeadlineExpired_AttemptStillPersisted is the
// direct C-1 regression: the LLM call outruns the work budget, and the failure
// bookkeeping must still reach the store on a live context.
func TestNoteEnrichmentWorker_WorkDeadlineExpired_AttemptStillPersisted(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	lister := &fakeNoteLister{docs: []*model.Document{newNoteDoc(docID, 0)}}
	w := newDeadlineTestWorker(lister, &fakeInsightWriter{})

	w.tick(context.Background())

	if len(lister.attemptFailedCalls) != 1 {
		t.Fatalf("MarkNoteEnrichmentAttemptFailed calls = %d, want 1 — a work-deadline expiry must still record the attempt",
			len(lister.attemptFailedCalls))
	}
	call := lister.attemptFailedCalls[0]
	if call.ctxErr != nil {
		t.Errorf("bookkeeping ran on a dead context (ctx.Err() = %v); it must derive its own live budget, not inherit the expired work deadline", call.ctxErr)
	}
	if call.attempts != 1 {
		t.Errorf("attempts = %d, want 1", call.attempts)
	}
}

// TestNoteEnrichmentWorker_WorkDeadlineExpired_EventuallyTerminal pins the
// property the retry cap actually encodes: a note that times out on every
// attempt stops costing LLM calls. Without the C-1 fix this loops forever,
// because attempts never leave 0.
func TestNoteEnrichmentWorker_WorkDeadlineExpired_EventuallyTerminal(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	lister := &fakeNoteLister{docs: []*model.Document{newNoteDoc(docID, 0)}}
	w := newDeadlineTestWorker(lister, &fakeInsightWriter{})

	// Three ticks = the configured maxAttempts. A fourth is run to prove the
	// note is genuinely off the pending list rather than merely quiet.
	for i := 0; i < 4; i++ {
		w.tick(context.Background())
	}

	if len(lister.terminalCalls) != 1 {
		t.Fatalf("MarkNoteEnrichmentTerminal calls = %d, want exactly 1 after the attempt cap is reached (calls: attemptFailed=%d)",
			len(lister.terminalCalls), len(lister.attemptFailedCalls))
	}
	if err := lister.terminalCalls[0].ctxErr; err != nil {
		t.Errorf("terminal write ran on a dead context (ctx.Err() = %v)", err)
	}
	if len(lister.attemptFailedCalls) != 2 {
		t.Errorf("MarkNoteEnrichmentAttemptFailed calls = %d, want 2 (attempts 1 and 2; the 3rd is terminal)",
			len(lister.attemptFailedCalls))
	}
	if got := lister.pendingCount(); got != 0 {
		t.Errorf("pending notes = %d, want 0 — a terminal note must stop being re-listed and re-billed", got)
	}
}

// TestNoteEnrichmentWorker_WorkDeadlineDerivedFromLLMTimeout pins the
// reconciliation half of C-1: the LLM client's own timeout governs, and the
// worker's work budget is derived from it. A configuration that tries to make
// the work budget the tighter of the two is clamped up rather than honoured,
// because a work deadline firing before the client's own timeout is what put
// bookkeeping on a dead context in the first place.
func TestNoteEnrichmentWorker_WorkDeadlineDerivedFromLLMTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		llmTimeout  time.Duration
		workTimeout time.Duration
		want        time.Duration
	}{
		{
			name: "defaults mirror LLM_TIMEOUT_SECONDS=120",
			want: defaultNoteEnrichmentLLMTimeout + noteEnrichmentWorkMargin,
		},
		{
			name:       "derived from an explicit client timeout",
			llmTimeout: 300 * time.Second,
			want:       300*time.Second + noteEnrichmentWorkMargin,
		},
		{
			name:        "an override tighter than the client timeout is clamped up",
			llmTimeout:  120 * time.Second,
			workTimeout: 60 * time.Second, // the old defaultNoteEnrichmentDocTimeout
			want:        120*time.Second + noteEnrichmentWorkMargin,
		},
		{
			name:        "a wider override is honoured",
			llmTimeout:  30 * time.Second,
			workTimeout: 10 * time.Minute,
			want:        10 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := NewNoteEnrichmentWorker(NoteEnrichmentWorkerConfig{
				Store:       &fakeNoteLister{},
				Insights:    &fakeInsightWriter{},
				Entities:    &fakeEntityLinker{},
				LLM:         &fakeLLM{},
				LLMTimeout:  tt.llmTimeout,
				WorkTimeout: tt.workTimeout,
			})
			if got := w.workTimeout(); got != tt.want {
				t.Errorf("workTimeout() = %v, want %v", got, tt.want)
			}
			if w.workTimeout() <= tt.llmTimeout {
				t.Errorf("workTimeout() = %v must exceed the LLM client timeout %v", w.workTimeout(), tt.llmTimeout)
			}
		})
	}
}

// blockingInsightWriter blocks until its context expires, simulating a slow
// insight upsert/embed round-trip that outruns the persist budget.
type blockingInsightWriter struct{}

func (blockingInsightWriter) SaveInsight(ctx context.Context, _, _ string, _ map[string]any) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestNoteEnrichmentWorker_PersistDeadlineExpired_BookkeepingStillLive covers
// the success-path half of C-1: the LLM returned fine, but the derived-artifact
// writes exhausted their own budget. The resulting failure bookkeeping must not
// inherit that exhausted budget either.
func TestNoteEnrichmentWorker_PersistDeadlineExpired_BookkeepingStillLive(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	lister := &fakeNoteLister{docs: []*model.Document{newNoteDoc(docID, 0)}}
	w := newTestNoteWorker(lister, blockingInsightWriter{}, &fakeEntityLinker{},
		func(_ context.Context, _ llm.Completer, _ *model.Document) (*NoteEnrichmentResult, error) {
			return &NoteEnrichmentResult{
				Title:    "t",
				Insights: []NoteInsightCandidate{{Content: "an inference", Confidence: 0.5, SourceSpan: "s"}},
			}, nil
		},
	)
	w.persistTimeoutV = time.Millisecond

	w.tick(context.Background())

	if len(lister.attemptFailedCalls) != 1 {
		t.Fatalf("MarkNoteEnrichmentAttemptFailed calls = %d, want 1 when the persist budget expires", len(lister.attemptFailedCalls))
	}
	if err := lister.attemptFailedCalls[0].ctxErr; err != nil {
		t.Errorf("bookkeeping ran on the exhausted persist context (ctx.Err() = %v)", err)
	}
	if len(lister.enrichedCalls) != 0 {
		t.Errorf("MarkNoteEnriched calls = %d, want 0 — a note whose insights were not stored is not enriched", len(lister.enrichedCalls))
	}
}

// TestNoteEnrichmentWorker_ShutdownDuringLLMCall_DoesNotConsumeAttempt pins the
// deliberate asymmetry in processNote: a work-deadline expiry is the note's
// fault and burns an attempt, whereas a caller cancellation is the operator's
// and must not. Without the distinction, a few restart cycles would push
// healthy notes to terminal "failed".
func TestNoteEnrichmentWorker_ShutdownDuringLLMCall_DoesNotConsumeAttempt(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	lister := &fakeNoteLister{docs: []*model.Document{newNoteDoc(docID, 0)}}
	w := newTestNoteWorker(lister, &fakeInsightWriter{}, &fakeEntityLinker{},
		func(ctx context.Context, _ llm.Completer, _ *model.Document) (*NoteEnrichmentResult, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	w.tick(ctx)

	if len(lister.attemptFailedCalls) != 0 || len(lister.terminalCalls) != 0 {
		t.Errorf("shutdown consumed a retry: attemptFailed=%d terminal=%d, want 0/0",
			len(lister.attemptFailedCalls), len(lister.terminalCalls))
	}
	if lister.pendingCount() != 1 {
		t.Errorf("pending notes = %d, want 1 — the note must remain enrichable after a restart", lister.pendingCount())
	}
}

// TestNoteEnrichmentWorker_MaxDetachedWriteDuration_BoundsTheDrainWindow pins
// the contract cmd/collector/main.go relies on to size its SIGTERM drain. Only
// the write phases are detached; the LLM work budget (minutes) must NOT appear
// here, or the collector would hold a shutdown open for a call it has already
// cancelled.
func TestNoteEnrichmentWorker_MaxDetachedWriteDuration_BoundsTheDrainWindow(t *testing.T) {
	t.Parallel()

	w := NewNoteEnrichmentWorker(NoteEnrichmentWorkerConfig{
		Store:              &fakeNoteLister{},
		Insights:           &fakeInsightWriter{},
		Entities:           &fakeEntityLinker{},
		LLM:                &fakeLLM{},
		LLMTimeout:         120 * time.Second,
		PersistTimeout:     30 * time.Second,
		BookkeepingTimeout: 10 * time.Second,
	})

	if got, want := w.MaxDetachedWriteDuration(), 50*time.Second; got != want {
		t.Errorf("MaxDetachedWriteDuration() = %v, want %v (persist + commit write + failure write)", got, want)
	}
	if w.MaxDetachedWriteDuration() >= w.workTimeout() {
		t.Errorf("MaxDetachedWriteDuration() = %v must exclude the cancellable LLM work budget %v",
			w.MaxDetachedWriteDuration(), w.workTimeout())
	}
}

// TestNoteEnrichmentWorker_BookkeepingFailure_IsSurfacedNotSwallowed verifies
// the store's own failure hooks are reachable, i.e. that the fake can actually
// model a failing bookkeeping write. A test suite in which these calls cannot
// fail is a test suite that cannot observe C-1 or C-2.
func TestNoteEnrichmentWorker_BookkeepingFailure_IsSurfacedNotSwallowed(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	lister := &fakeNoteLister{
		docs:             []*model.Document{newNoteDoc(docID, 0)},
		attemptFailedErr: errors.New("connection refused"),
	}
	w := newTestNoteWorker(lister, &fakeInsightWriter{}, &fakeEntityLinker{},
		func(_ context.Context, _ llm.Completer, _ *model.Document) (*NoteEnrichmentResult, error) {
			return nil, errors.New("LLM 429")
		},
	)

	w.tick(context.Background())
	w.tick(context.Background())

	// The counter never persisted, so both ticks report attempts=1. This is
	// the honest outcome of an unreachable database, and it is distinguishable
	// from the C-1 bug only because the calls are made with a live context.
	if len(lister.attemptFailedCalls) != 2 {
		t.Fatalf("attemptFailedCalls = %d, want 2", len(lister.attemptFailedCalls))
	}
	for i, c := range lister.attemptFailedCalls {
		if c.ctxErr != nil {
			t.Errorf("attemptFailedCalls[%d] ran on a dead context: %v", i, c.ctxErr)
		}
	}
}
