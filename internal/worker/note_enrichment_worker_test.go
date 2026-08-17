package worker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// fakeNoteLister is an in-memory NoteEnrichmentLister for unit testing.
//
// Every method can be made to fail, not just ListPendingNotes. That is
// load-bearing: while only the list call had an error hook, every retry-policy
// test necessarily ran in a world where bookkeeping always succeeds — the one
// world in which "bookkeeping runs on an expired context" and "a failed commit
// write is swallowed" are both invisible. Both bugs shipped through a green
// suite for exactly that reason.
//
// The fake also models the store's state transitions (attempt counters are
// written back into the document's Metadata; a terminal note leaves the
// pending list) so multi-tick retry behaviour can be asserted end-to-end
// rather than one isolated call at a time.
type fakeNoteLister struct {
	mu                 sync.Mutex
	docs               []*model.Document
	listErr            error
	enrichedErr        error
	attemptFailedErr   error
	terminalErr        error
	enrichedCalls      []enrichedCall
	attemptFailedCalls []attemptFailedCall
	terminalCalls      []terminalCall
}

type enrichedCall struct {
	documentID uuid.UUID
	title      string
	metadata   map[string]any
	ctxErr     error // ctx.Err() observed at call time — nil means a live budget
}
type attemptFailedCall struct {
	documentID  uuid.UUID
	attempts    int
	reason      string
	nextRetryAt time.Time
	ctxErr      error
}
type terminalCall struct {
	documentID uuid.UUID
	reason     string
	ctxErr     error
}

func (f *fakeNoteLister) ListPendingNotes(_ context.Context, limit int) ([]*model.Document, error) {
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

func (f *fakeNoteLister) MarkNoteEnriched(ctx context.Context, id uuid.UUID, title string, metadata map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enrichedCalls = append(f.enrichedCalls, enrichedCall{id, title, metadata, ctx.Err()})
	return f.enrichedErr
}

func (f *fakeNoteLister) MarkNoteEnrichmentAttemptFailed(ctx context.Context, id uuid.UUID, attempts int, reason string, nextRetryAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attemptFailedCalls = append(f.attemptFailedCalls, attemptFailedCall{id, attempts, reason, nextRetryAt, ctx.Err()})
	if f.attemptFailedErr != nil {
		return f.attemptFailedErr
	}
	// Model the store: the counter is durably persisted, so the next tick
	// sees the incremented value.
	f.setAttemptsLocked(id, attempts)
	return nil
}

func (f *fakeNoteLister) MarkNoteEnrichmentTerminal(ctx context.Context, id uuid.UUID, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminalCalls = append(f.terminalCalls, terminalCall{id, reason, ctx.Err()})
	if f.terminalErr != nil {
		return f.terminalErr
	}
	// Model the store: enrichment_status="failed" drops the note out of
	// ListPendingNotes until an explicit retry-enrichment call revives it.
	kept := f.docs[:0]
	for _, d := range f.docs {
		if d.ID != id {
			kept = append(kept, d)
		}
	}
	f.docs = kept
	return nil
}

// setAttemptsLocked writes the persisted attempt counter back into the
// in-memory document. Caller must hold f.mu.
func (f *fakeNoteLister) setAttemptsLocked(id uuid.UUID, attempts int) {
	for _, d := range f.docs {
		if d.ID == id {
			if d.Metadata == nil {
				d.Metadata = map[string]any{}
			}
			d.Metadata["enrichment_attempts"] = float64(attempts)
		}
	}
}

// pendingCount reports how many notes are still listed as pending.
func (f *fakeNoteLister) pendingCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.docs)
}

// fakeInsightWriter records insight writes performed during a tick.
type fakeInsightWriter struct {
	mu     sync.Mutex
	writes []insightWrite
	err    error // when non-nil, every SaveInsight call fails
}

type insightWrite struct {
	content  string
	sourceID string
	metadata map[string]any
}

func (f *fakeInsightWriter) SaveInsight(_ context.Context, content, sourceID string, metadata map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.writes = append(f.writes, insightWrite{content, sourceID, metadata})
	return nil
}

func (f *fakeInsightWriter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

// snapshot returns a copy of the recorded insight writes.
func (f *fakeInsightWriter) snapshot() []insightWrite {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]insightWrite, len(f.writes))
	copy(out, f.writes)
	return out
}

func newNoteDoc(id uuid.UUID, attempts int) *model.Document {
	return &model.Document{
		ID:         id,
		SourceType: model.SourceNote,
		SourceID:   "note-source",
		Title:      "",
		Content:    "Original raw content that must never be mutated.",
		Metadata: map[string]any{
			"enrichment_status":   "pending",
			"enrichment_attempts": float64(attempts), // JSONB round-trips numbers as float64
		},
	}
}

// newTestNoteWorker builds a NoteEnrichmentWorker with an injectable
// enrichFn, mirroring the entity_worker_test.go newTestWorker pattern.
func newTestNoteWorker(
	lister NoteEnrichmentLister,
	insights InsightWriter,
	entities EntityLinker,
	fn func(context.Context, llm.Completer, *model.Document) (*NoteEnrichmentResult, error),
) *NoteEnrichmentWorker {
	return &NoteEnrichmentWorker{
		store:               lister,
		insights:            insights,
		entities:            entities,
		llm:                 &fakeLLM{},
		interval:            time.Minute,
		batchSize:           10,
		maxAttempts:         3,
		retryBackoff:        []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute},
		workTimeoutV:        30 * time.Second,
		persistTimeoutV:     30 * time.Second,
		bookkeepingTimeoutV: 30 * time.Second,
		enrichFn:            fn,
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestNoteEnrichmentWorker_Success_MergesMetadataLinksEntities verifies a
// single successful tick performs all three effects: MarkNoteEnriched with
// the generated title, entity linking, and insight-doc creation — all from
// ONE enrichFn call (spec §6.3 single-LLM-call requirement).
func TestNoteEnrichmentWorker_Success_MergesMetadataLinksEntities(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	lister := &fakeNoteLister{docs: []*model.Document{newNoteDoc(docID, 0)}}
	insights := &fakeInsightWriter{}
	entities := &fakeEntityLinker{}

	calls := 0
	w := newTestNoteWorker(lister, insights, entities,
		func(_ context.Context, _ llm.Completer, _ *model.Document) (*NoteEnrichmentResult, error) {
			calls++
			return &NoteEnrichmentResult{
				Title:    "Generated title",
				Summary:  "Generated summary",
				Tags:     []string{"budget"},
				Entities: []model.Entity{{Name: "Alice", Type: model.EntityTypePerson}},
				Insights: []NoteInsightCandidate{
					{Content: "Budget may be tight", Confidence: 0.6, SourceSpan: "line 1"},
				},
			}, nil
		},
	)
	w.tick(context.Background())

	if calls != 1 {
		t.Fatalf("enrichFn called %d times, want 1 (single LLM call per note)", calls)
	}
	if len(lister.enrichedCalls) != 1 || lister.enrichedCalls[0].title != "Generated title" {
		t.Errorf("MarkNoteEnriched calls = %+v, want one call with title %q", lister.enrichedCalls, "Generated title")
	}
	if entities.callCount() != 1 {
		t.Errorf("UpsertAndLinkEntities called %d times, want 1", entities.callCount())
	}
	if insights.count() != 1 {
		t.Errorf("insight docs created = %d, want 1", insights.count())
	}
}

// TestNoteEnrichmentWorker_ContentNeverMutated pins the write-once invariant
// (spec §3.1): after a successful tick, the note document's Content field
// held by the store must be byte-identical to its pre-tick value. The
// worker structurally cannot violate this — MarkNoteEnriched's signature has
// no content parameter — but this test pins the invariant at the
// integration seam so a future signature change cannot silently break it.
func TestNoteEnrichmentWorker_ContentNeverMutated(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	doc := newNoteDoc(docID, 0)
	originalContent := doc.Content
	lister := &fakeNoteLister{docs: []*model.Document{doc}}
	insights := &fakeInsightWriter{}
	entities := &fakeEntityLinker{}

	w := newTestNoteWorker(lister, insights, entities,
		func(_ context.Context, _ llm.Completer, _ *model.Document) (*NoteEnrichmentResult, error) {
			return &NoteEnrichmentResult{Title: "New title", Summary: "New summary"}, nil
		},
	)
	w.tick(context.Background())

	if doc.Content != originalContent {
		t.Errorf("Content mutated: got %q, want unchanged %q", doc.Content, originalContent)
	}
}

// TestNoteEnrichmentWorker_InsightCapAt3 verifies that even when the LLM
// returns more than 3 insight candidates, only 3 insight documents are
// created (spec §6.4 hard cap).
func TestNoteEnrichmentWorker_InsightCapAt3(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	lister := &fakeNoteLister{docs: []*model.Document{newNoteDoc(docID, 0)}}
	insights := &fakeInsightWriter{}
	entities := &fakeEntityLinker{}

	candidates := make([]NoteInsightCandidate, 5)
	for i := range candidates {
		candidates[i] = NoteInsightCandidate{Content: "candidate", Confidence: 0.5, SourceSpan: "x"}
	}
	w := newTestNoteWorker(lister, insights, entities,
		func(_ context.Context, _ llm.Completer, _ *model.Document) (*NoteEnrichmentResult, error) {
			return &NoteEnrichmentResult{Title: "t", Insights: candidates}, nil
		},
	)
	w.tick(context.Background())

	if insights.count() != maxInsightsPerNote {
		t.Errorf("insight docs created = %d, want %d (hard cap)", insights.count(), maxInsightsPerNote)
	}
}

// TestNoteEnrichmentWorker_RetryPolicy_BelowMax_NotTerminal verifies the
// 2nd failure (attempts goes 1 -> 2) stays non-terminal.
func TestNoteEnrichmentWorker_RetryPolicy_BelowMax_NotTerminal(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	lister := &fakeNoteLister{docs: []*model.Document{newNoteDoc(docID, 1)}} // already failed once
	insights := &fakeInsightWriter{}
	entities := &fakeEntityLinker{}

	w := newTestNoteWorker(lister, insights, entities,
		func(_ context.Context, _ llm.Completer, _ *model.Document) (*NoteEnrichmentResult, error) {
			return nil, errors.New("LLM timeout")
		},
	)
	w.tick(context.Background())

	if len(lister.terminalCalls) != 0 {
		t.Errorf("MarkNoteEnrichmentTerminal called %d times, want 0 (2nd failure is not terminal)", len(lister.terminalCalls))
	}
	if len(lister.attemptFailedCalls) != 1 || lister.attemptFailedCalls[0].attempts != 2 {
		t.Errorf("attemptFailedCalls = %+v, want one call with attempts=2", lister.attemptFailedCalls)
	}
}

// TestNoteEnrichmentWorker_RetryPolicy_AtMax_MarksTerminal verifies the 3rd
// failure (attempts goes 2 -> 3) marks the note terminal "failed" and does
// NOT call MarkNoteEnrichmentAttemptFailed (spec §6.3: 3-attempt cap).
func TestNoteEnrichmentWorker_RetryPolicy_AtMax_MarksTerminal(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	lister := &fakeNoteLister{docs: []*model.Document{newNoteDoc(docID, 2)}} // already failed twice
	insights := &fakeInsightWriter{}
	entities := &fakeEntityLinker{}

	w := newTestNoteWorker(lister, insights, entities,
		func(_ context.Context, _ llm.Completer, _ *model.Document) (*NoteEnrichmentResult, error) {
			return nil, errors.New("LLM timeout")
		},
	)
	w.tick(context.Background())

	if len(lister.terminalCalls) != 1 {
		t.Fatalf("MarkNoteEnrichmentTerminal called %d times, want 1 (3rd failure is terminal)", len(lister.terminalCalls))
	}
	if len(lister.attemptFailedCalls) != 0 {
		t.Errorf("MarkNoteEnrichmentAttemptFailed called %d times, want 0 when hitting the terminal attempt", len(lister.attemptFailedCalls))
	}
}

// TestNoteEnrichmentWorker_EmptyBatch_NoOp verifies tick is a no-op when
// ListPendingNotes returns nothing.
func TestNoteEnrichmentWorker_EmptyBatch_NoOp(t *testing.T) {
	t.Parallel()

	lister := &fakeNoteLister{docs: nil}
	insights := &fakeInsightWriter{}
	entities := &fakeEntityLinker{}

	w := newTestNoteWorker(lister, insights, entities,
		func(_ context.Context, _ llm.Completer, _ *model.Document) (*NoteEnrichmentResult, error) {
			t.Error("enrichFn should not be called when no documents are returned")
			return nil, nil
		},
	)
	w.tick(context.Background())

	if len(lister.enrichedCalls) != 0 || insights.count() != 0 {
		t.Error("expected no side effects on an empty batch")
	}
}

// ---------------------------------------------------------------------------
// Additional contract tests (design invariants beyond the brief's six)
// ---------------------------------------------------------------------------

// TestNoteEnrichmentWorker_InsightsAreSeparateDocuments pins spec §3.2 /
// §6.5: an inference is NEVER merged into the note. It must become its own
// document that (a) carries model.SourceInsight — so ListPendingNotes'
// source_type='note' filter structurally prevents it from re-entering
// enrichment (insight-of-insight is impossible), (b) records provenance back
// to the origin note, and (c) never leaks the note's verbatim Content into
// the note's own Metadata.
func TestNoteEnrichmentWorker_InsightsAreSeparateDocuments(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	note := newNoteDoc(docID, 0)
	lister := &fakeNoteLister{docs: []*model.Document{note}}
	insights := &fakeInsightWriter{}
	entities := &fakeEntityLinker{}

	w := newTestNoteWorker(lister, insights, entities,
		func(_ context.Context, _ llm.Completer, _ *model.Document) (*NoteEnrichmentResult, error) {
			return &NoteEnrichmentResult{
				Title: "t",
				Insights: []NoteInsightCandidate{
					{Content: "자금 사정이 어렵다", Confidence: 0.42, SourceSpan: "예산 얘기를 피했다"},
				},
			}, nil
		},
	)
	w.tick(context.Background())

	writes := insights.snapshot()
	if len(writes) != 1 {
		t.Fatalf("insight writes = %d, want 1", len(writes))
	}
	ins := writes[0]

	if ins.content != "자금 사정이 어렵다" {
		t.Errorf("insight content = %q, want the inference text", ins.content)
	}
	if ins.content == note.Content {
		t.Error("insight content must not be a copy of the note's verbatim content")
	}
	if !strings.HasPrefix(ins.sourceID, "insight:"+docID.String()+":") {
		t.Errorf("insight sourceID = %q, want deterministic prefix %q", ins.sourceID, "insight:"+docID.String()+":")
	}

	prov, ok := ins.metadata["provenance"].(map[string]any)
	if !ok {
		t.Fatalf("insight metadata[provenance] = %#v, want map[string]any", ins.metadata["provenance"])
	}
	if prov["source_note_id"] != docID.String() {
		t.Errorf("provenance.source_note_id = %v, want %s", prov["source_note_id"], docID)
	}
	if ins.metadata["confidence"] != 0.42 {
		t.Errorf("insight metadata[confidence] = %v, want 0.42 (auditability)", ins.metadata["confidence"])
	}
	if ins.metadata["source_span"] != "예산 얘기를 피했다" {
		t.Errorf("insight metadata[source_span] = %v, want the quoted excerpt", ins.metadata["source_span"])
	}

	// The note's own metadata write must not carry the inference or content.
	if len(lister.enrichedCalls) != 1 {
		t.Fatalf("MarkNoteEnriched calls = %d, want 1", len(lister.enrichedCalls))
	}
	meta := lister.enrichedCalls[0].metadata
	if _, exists := meta["content"]; exists {
		t.Error("MarkNoteEnriched metadata must never contain a content key (spec §3.1 write-once)")
	}
	if _, exists := meta["insights"]; exists {
		t.Error("MarkNoteEnriched metadata must never contain insights (spec §3.2: inferences stay separate)")
	}
	if meta["enrichment_status"] != "done" {
		t.Errorf("metadata[enrichment_status] = %v, want \"done\"", meta["enrichment_status"])
	}
}

// TestNoteEnrichmentWorker_InsightSourceIDsAreStableAcrossRetries verifies the
// insight source_id is derived deterministically from (note id, index). The
// store upserts on (source_type, source_id), so a note re-processed after a
// crash between insight creation and MarkNoteEnriched overwrites its own
// insights instead of duplicating them — this is what makes the 3-insight cap
// hold across retries rather than per-attempt.
func TestNoteEnrichmentWorker_InsightSourceIDsAreStableAcrossRetries(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	lister := &fakeNoteLister{docs: []*model.Document{newNoteDoc(docID, 0)}}
	insights := &fakeInsightWriter{}
	entities := &fakeEntityLinker{}

	w := newTestNoteWorker(lister, insights, entities,
		func(_ context.Context, _ llm.Completer, _ *model.Document) (*NoteEnrichmentResult, error) {
			return &NoteEnrichmentResult{
				Title: "t",
				Insights: []NoteInsightCandidate{
					{Content: "a", Confidence: 0.1, SourceSpan: "s1"},
					{Content: "b", Confidence: 0.2, SourceSpan: "s2"},
				},
			}, nil
		},
	)
	w.tick(context.Background())
	w.tick(context.Background())

	writes := insights.snapshot()
	if len(writes) != 4 {
		t.Fatalf("insight writes = %d, want 4 (2 insights x 2 ticks)", len(writes))
	}
	if writes[0].sourceID != writes[2].sourceID || writes[1].sourceID != writes[3].sourceID {
		t.Errorf("insight source_ids not stable across ticks: %q/%q then %q/%q",
			writes[0].sourceID, writes[1].sourceID, writes[2].sourceID, writes[3].sourceID)
	}
	if writes[0].sourceID == writes[1].sourceID {
		t.Errorf("insights within one note must have distinct source_ids, both were %q", writes[0].sourceID)
	}
}

// ---------------------------------------------------------------------------
// Ordering doubles: record the sequence of side effects within a tick.
// ---------------------------------------------------------------------------

type orderRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *orderRecorder) add(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *orderRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	copy(out, r.events)
	return out
}

type orderingLister struct {
	*fakeNoteLister
	rec *orderRecorder
}

func (o *orderingLister) MarkNoteEnriched(ctx context.Context, id uuid.UUID, title string, metadata map[string]any) error {
	o.rec.add("mark_enriched")
	return o.fakeNoteLister.MarkNoteEnriched(ctx, id, title, metadata)
}

type orderingWriter struct {
	*fakeInsightWriter
	rec *orderRecorder
}

func (o *orderingWriter) SaveInsight(ctx context.Context, content, sourceID string, metadata map[string]any) error {
	o.rec.add("insight_write")
	return o.fakeInsightWriter.SaveInsight(ctx, content, sourceID, metadata)
}

type orderingLinker struct {
	*fakeEntityLinker
	rec *orderRecorder
}

func (o *orderingLinker) UpsertAndLinkEntities(ctx context.Context, id uuid.UUID, entities []model.Entity) error {
	o.rec.add("link_entities")
	return o.fakeEntityLinker.UpsertAndLinkEntities(ctx, id, entities)
}

// TestNoteEnrichmentWorker_MarkEnrichedIsTheCommitPoint verifies derived
// artifacts (insight documents, entity links) are written BEFORE
// MarkNoteEnriched. MarkNoteEnriched flips enrichment_status to "done", after
// which ListPendingNotes never returns the note again — so it must be the last
// write. A crash before it leaves the note pending and the (idempotent,
// stable-source_id) insight writes are simply redone on the next tick; a crash
// after the brief's original order would have lost the insights permanently.
func TestNoteEnrichmentWorker_MarkEnrichedIsTheCommitPoint(t *testing.T) {
	t.Parallel()

	rec := &orderRecorder{}
	docID := uuid.New()
	lister := &orderingLister{fakeNoteLister: &fakeNoteLister{docs: []*model.Document{newNoteDoc(docID, 0)}}, rec: rec}
	insights := &orderingWriter{fakeInsightWriter: &fakeInsightWriter{}, rec: rec}
	entities := &orderingLinker{fakeEntityLinker: &fakeEntityLinker{}, rec: rec}

	w := newTestNoteWorker(lister, insights, entities,
		func(_ context.Context, _ llm.Completer, _ *model.Document) (*NoteEnrichmentResult, error) {
			return &NoteEnrichmentResult{
				Title:    "t",
				Entities: []model.Entity{{Name: "Alice", Type: model.EntityTypePerson}},
				Insights: []NoteInsightCandidate{{Content: "a", Confidence: 0.3, SourceSpan: "s"}},
			}, nil
		},
	)
	w.tick(context.Background())

	events := rec.snapshot()
	if len(events) != 3 {
		t.Fatalf("recorded events = %v, want 3 (insight_write, link_entities, mark_enriched)", events)
	}
	if events[len(events)-1] != "mark_enriched" {
		t.Errorf("event order = %v, want mark_enriched last (it is the commit point)", events)
	}
}

// TestNoteEnrichmentWorker_ContextCancelled_StopsLaunchingWork verifies tick
// stops starting NEW per-note work once the caller's context is cancelled, so
// a SIGTERM drain window is bounded by one note's timeout regardless of
// BatchSize (the lesson from summarizer.go's whole-tick-deadline rework).
func TestNoteEnrichmentWorker_ContextCancelled_StopsLaunchingWork(t *testing.T) {
	t.Parallel()

	lister := &fakeNoteLister{docs: []*model.Document{
		newNoteDoc(uuid.New(), 0),
		newNoteDoc(uuid.New(), 0),
		newNoteDoc(uuid.New(), 0),
	}}
	insights := &fakeInsightWriter{}
	entities := &fakeEntityLinker{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0
	w := newTestNoteWorker(lister, insights, entities,
		func(_ context.Context, _ llm.Completer, _ *model.Document) (*NoteEnrichmentResult, error) {
			calls++
			cancel() // parent shutdown arrives while the first note is in flight
			return &NoteEnrichmentResult{Title: "t"}, nil
		},
	)
	w.tick(ctx)

	if calls != 1 {
		t.Errorf("enrichFn called %d times after cancellation, want 1 (no new work launched)", calls)
	}
	// The in-flight note still completes its writes: cancellation must not
	// truncate a note halfway through its own side effects.
	if len(lister.enrichedCalls) != 1 {
		t.Errorf("MarkNoteEnriched calls = %d, want 1 (in-flight note completes)", len(lister.enrichedCalls))
	}
}

// TestNoteEnrichmentWorker_Run_StopsOnContextCancel verifies Run returns when
// its context is cancelled (the worker is started as a goroutine by
// cmd/collector/main.go).
func TestNoteEnrichmentWorker_Run_StopsOnContextCancel(t *testing.T) {
	t.Parallel()

	lister := &fakeNoteLister{}
	w := newTestNoteWorker(lister, &fakeInsightWriter{}, &fakeEntityLinker{},
		func(_ context.Context, _ llm.Completer, _ *model.Document) (*NoteEnrichmentResult, error) {
			return &NoteEnrichmentResult{}, nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of context cancellation")
	}
}

// ---------------------------------------------------------------------------
// EnrichNote / parseEnrichmentResponse
// ---------------------------------------------------------------------------

// countingLLM records CompleteWithMessages invocations and returns a canned
// response, so EnrichNote's single-call contract can be asserted directly.
type countingLLM struct {
	mu       sync.Mutex
	calls    int
	system   string
	messages []llm.Message
	response string
	err      error
	disabled bool
}

func (c *countingLLM) Enabled() bool { return !c.disabled }

func (c *countingLLM) CompleteWithMessages(_ context.Context, system string, messages []llm.Message) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.system = system
	c.messages = messages
	return c.response, c.err
}

// TestEnrichNote_SingleLLMCall verifies EnrichNote issues exactly ONE LLM call
// per note (spec §6.3) and parses every field of the structured response.
func TestEnrichNote_SingleLLMCall(t *testing.T) {
	t.Parallel()

	client := &countingLLM{response: `{
		"title": "예산 회의",
		"summary": "예산 논의가 지연되었다.",
		"tags": ["budget", "meeting"],
		"entities": [{"name": "김대표", "type": "PERSON"}, {"name": "ACME", "type": "org"}],
		"insights": [
			{"content": "i1", "confidence": 0.7, "source_span": "s1"},
			{"content": "i2", "confidence": 0.5, "source_span": "s2"},
			{"content": "i3", "confidence": 0.3, "source_span": "s3"},
			{"content": "i4", "confidence": 0.2, "source_span": "s4"},
			{"content": "i5", "confidence": 0.1, "source_span": "s5"}
		]
	}`}

	doc := newNoteDoc(uuid.New(), 0)
	result, err := EnrichNote(context.Background(), client, doc)
	if err != nil {
		t.Fatalf("EnrichNote returned error: %v", err)
	}
	if client.calls != 1 {
		t.Errorf("CompleteWithMessages called %d times, want 1", client.calls)
	}
	if len(client.messages) != 1 || client.messages[0].Role != "user" {
		t.Errorf("messages = %+v, want a single user turn (content is data, not instructions)", client.messages)
	}
	if !strings.Contains(client.messages[0].Content, doc.Content) {
		t.Errorf("user turn %q does not carry the note content", client.messages[0].Content)
	}
	if result.Title != "예산 회의" || result.Summary != "예산 논의가 지연되었다." {
		t.Errorf("title/summary = %q/%q, want the parsed values", result.Title, result.Summary)
	}
	if len(result.Tags) != 2 {
		t.Errorf("tags = %v, want 2", result.Tags)
	}
	if len(result.Entities) != 2 || result.Entities[1].Type != model.EntityTypeOrg {
		t.Errorf("entities = %+v, want 2 with canonicalized types", result.Entities)
	}
	// EnrichNote parses everything the model returned; the hard cap is applied
	// by the worker when persisting, so over-generation stays observable here.
	if len(result.Insights) != 5 {
		t.Errorf("parsed insights = %d, want 5 (cap is enforced at persistence, not parse)", len(result.Insights))
	}
}

// TestEnrichNote_LLMDisabled_ReturnsError verifies EnrichNote fails fast (and
// therefore burns a retry attempt rather than silently succeeding) when the
// LLM client is not configured.
func TestEnrichNote_LLMDisabled_ReturnsError(t *testing.T) {
	t.Parallel()

	client := &countingLLM{disabled: true}
	if _, err := EnrichNote(context.Background(), client, newNoteDoc(uuid.New(), 0)); err == nil {
		t.Fatal("EnrichNote returned nil error with a disabled LLM client, want error")
	}
	if client.calls != 0 {
		t.Errorf("CompleteWithMessages called %d times with a disabled client, want 0", client.calls)
	}
}

// TestParseEnrichmentResponse_StripsFenceAndSkipsEmpty verifies markdown-fenced
// responses parse, and that entities/insights with empty content are dropped
// rather than persisted as blank documents.
func TestParseEnrichmentResponse_StripsFenceAndSkipsEmpty(t *testing.T) {
	t.Parallel()

	raw := "```json\n" + `{
		"title": "  spaced  ",
		"entities": [{"name": "  ", "type": "PERSON"}, {"name": "Bob", "type": "unknown"}],
		"insights": [{"content": "   ", "confidence": 0.1}, {"content": "kept", "confidence": 0.9, "source_span": "s"}]
	}` + "\n```"

	result, err := parseEnrichmentResponse(raw)
	if err != nil {
		t.Fatalf("parseEnrichmentResponse returned error: %v", err)
	}
	if result.Title != "spaced" {
		t.Errorf("title = %q, want %q", result.Title, "spaced")
	}
	if len(result.Entities) != 1 || result.Entities[0].Name != "Bob" || result.Entities[0].Type != model.EntityTypeOther {
		t.Errorf("entities = %+v, want only Bob with OTHER type", result.Entities)
	}
	if len(result.Insights) != 1 || result.Insights[0].Content != "kept" {
		t.Errorf("insights = %+v, want only the non-empty candidate", result.Insights)
	}
}

// TestParseEnrichmentResponse_InvalidJSON_ReturnsError verifies unparseable
// output is an error, so the note keeps its pending status and consumes one
// bounded retry attempt instead of being marked done with garbage.
func TestParseEnrichmentResponse_InvalidJSON_ReturnsError(t *testing.T) {
	t.Parallel()

	if _, err := parseEnrichmentResponse("I'm sorry, I cannot do that."); err == nil {
		t.Fatal("parseEnrichmentResponse returned nil error for non-JSON output, want error")
	}
}

// TestNewNoteEnrichmentWorker_Defaults verifies the constructor's fallback
// values, notably the 3-attempt cap that bounds LLM spend per note.
func TestNewNoteEnrichmentWorker_Defaults(t *testing.T) {
	t.Parallel()

	w := NewNoteEnrichmentWorker(NoteEnrichmentWorkerConfig{
		Store:    &fakeNoteLister{},
		Insights: &fakeInsightWriter{},
		Entities: &fakeEntityLinker{},
		LLM:      &fakeLLM{},
	})

	if w.maxAttempts != 3 {
		t.Errorf("maxAttempts = %d, want 3", w.maxAttempts)
	}
	if len(w.retryBackoff) != 3 {
		t.Errorf("retryBackoff = %v, want 3 entries", w.retryBackoff)
	}
	if w.batchSize <= 0 || w.interval <= 0 {
		t.Errorf("batchSize/interval = %d/%v, want positive defaults", w.batchSize, w.interval)
	}
	if w.workTimeout() <= 0 || w.persistTimeout() <= 0 || w.bookkeepingTimeout() <= 0 {
		t.Errorf("work/persist/bookkeeping timeouts = %v/%v/%v, want positive defaults",
			w.workTimeout(), w.persistTimeout(), w.bookkeepingTimeout())
	}
	if w.enrichFn == nil {
		t.Error("enrichFn must default to EnrichNote")
	}
}

// ---------------------------------------------------------------------------
// Insight persistence: failed writes must fail the attempt
// ---------------------------------------------------------------------------

// TestNoteEnrichmentWorker_InsightWriteFails_DrivesRetryPath verifies a failed
// insight write is treated as a failed enrichment attempt, not a success.
// Marking the note "done" would drop the inference permanently with no retry
// and no trace. The write is idempotent (stable source_ids), so retrying is
// safe.
func TestNoteEnrichmentWorker_InsightWriteFails_DrivesRetryPath(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	lister := &fakeNoteLister{docs: []*model.Document{newNoteDoc(docID, 0)}}
	insights := &fakeInsightWriter{err: errors.New("chunk write failed")}
	entities := &fakeEntityLinker{}

	w := newTestNoteWorker(lister, insights, entities,
		func(_ context.Context, _ llm.Completer, _ *model.Document) (*NoteEnrichmentResult, error) {
			return &NoteEnrichmentResult{
				Title:    "t",
				Entities: []model.Entity{{Name: "Alice", Type: model.EntityTypePerson}},
				Insights: []NoteInsightCandidate{{Content: "a", Confidence: 0.3, SourceSpan: "s"}},
			}, nil
		},
	)
	w.tick(context.Background())

	if len(lister.enrichedCalls) != 0 {
		t.Errorf("MarkNoteEnriched called %d times after a failed insight write, want 0 (the note must stay pending)",
			len(lister.enrichedCalls))
	}
	if len(lister.attemptFailedCalls) != 1 {
		t.Fatalf("MarkNoteEnrichmentAttemptFailed called %d times, want 1 (failed insight write burns an attempt)",
			len(lister.attemptFailedCalls))
	}
	if got := lister.attemptFailedCalls[0].attempts; got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
	if !strings.Contains(lister.attemptFailedCalls[0].reason, "chunk write failed") {
		t.Errorf("failure reason = %q, want it to carry the underlying insight error", lister.attemptFailedCalls[0].reason)
	}
	if entities.callCount() != 0 {
		t.Errorf("UpsertAndLinkEntities called %d times, want 0 (the attempt aborts at the failed insight)",
			entities.callCount())
	}
}

// TestNoteEnrichmentWorker_InsightWriteFails_AtMaxAttempts_MarksTerminal
// verifies the insight-write failure path shares the same bounded retry policy
// as an LLM failure: it cannot retry forever.
func TestNoteEnrichmentWorker_InsightWriteFails_AtMaxAttempts_MarksTerminal(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	lister := &fakeNoteLister{docs: []*model.Document{newNoteDoc(docID, 2)}} // already failed twice
	insights := &fakeInsightWriter{err: errors.New("db down")}

	w := newTestNoteWorker(lister, insights, &fakeEntityLinker{},
		func(_ context.Context, _ llm.Completer, _ *model.Document) (*NoteEnrichmentResult, error) {
			return &NoteEnrichmentResult{
				Title:    "t",
				Insights: []NoteInsightCandidate{{Content: "a", Confidence: 0.3, SourceSpan: "s"}},
			}, nil
		},
	)
	w.tick(context.Background())

	if len(lister.terminalCalls) != 1 {
		t.Errorf("MarkNoteEnrichmentTerminal called %d times, want 1 (3rd failure is terminal)", len(lister.terminalCalls))
	}
	if len(lister.enrichedCalls) != 0 {
		t.Errorf("MarkNoteEnriched called %d times, want 0", len(lister.enrichedCalls))
	}
}

// TestNoteEnrichmentWorker_BlankInsightCandidate_Skipped verifies a blank
// candidate is dropped rather than persisted. note.Save rejects empty content,
// so persisting one would fail every attempt and drive an otherwise-fine note
// terminal — one malformed model output would cost the note its enrichment.
func TestNoteEnrichmentWorker_BlankInsightCandidate_Skipped(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	lister := &fakeNoteLister{docs: []*model.Document{newNoteDoc(docID, 0)}}
	insights := &fakeInsightWriter{}

	w := newTestNoteWorker(lister, insights, &fakeEntityLinker{},
		func(_ context.Context, _ llm.Completer, _ *model.Document) (*NoteEnrichmentResult, error) {
			return &NoteEnrichmentResult{
				Title: "t",
				Insights: []NoteInsightCandidate{
					{Content: "   ", Confidence: 0.1, SourceSpan: "s"},
					{Content: "kept", Confidence: 0.4, SourceSpan: "s"},
				},
			}, nil
		},
	)
	w.tick(context.Background())

	writes := insights.snapshot()
	if len(writes) != 1 || writes[0].content != "kept" {
		t.Errorf("insight writes = %+v, want only the non-blank candidate", writes)
	}
	if len(lister.enrichedCalls) != 1 {
		t.Errorf("MarkNoteEnriched calls = %d, want 1 (a blank candidate must not fail the note)", len(lister.enrichedCalls))
	}
}

// ---------------------------------------------------------------------------
// insightSaver: the production InsightWriter (note.Save-backed)
// ---------------------------------------------------------------------------

// fakeNoteDocs is a note.DocumentUpserter recording the documents note.Save
// persists.
type fakeNoteDocs struct {
	mu   sync.Mutex
	docs []*model.Document
	err  error
}

func (f *fakeNoteDocs) Upsert(_ context.Context, doc *model.Document) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if doc.ID == (uuid.UUID{}) {
		doc.ID = uuid.New()
	}
	f.docs = append(f.docs, doc)
	return nil
}

// fakeNoteChunks is a note.ChunkWriter recording chunk and embedding writes.
type fakeNoteChunks struct {
	mu         sync.Mutex
	chunks     []store.Chunk
	embeddings []store.ChunkEmbedding
	replaceErr error
}

func (f *fakeNoteChunks) ReplaceDocument(_ context.Context, _ uuid.UUID, chunks []store.Chunk) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.replaceErr != nil {
		return f.replaceErr
	}
	f.chunks = make([]store.Chunk, 0, len(chunks))
	for i, c := range chunks {
		c.ID = int64(i + 1) // the store assigns ids on insert
		f.chunks = append(f.chunks, c)
	}
	return nil
}

func (f *fakeNoteChunks) ListByDocument(_ context.Context, _ uuid.UUID) ([]store.Chunk, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chunks, nil
}

func (f *fakeNoteChunks) UpdateChunkEmbeddings(_ context.Context, embeddings []store.ChunkEmbedding) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.embeddings = append(f.embeddings, embeddings...)
	return nil
}

// fakeNoteEmbedder is a note.Embedder recording the texts submitted for
// embedding.
type fakeNoteEmbedder struct {
	mu       sync.Mutex
	disabled bool
	err      error
	texts    []string
}

func (f *fakeNoteEmbedder) Enabled() bool { return !f.disabled }

func (f *fakeNoteEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.texts = append(f.texts, texts...)
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{0.1, 0.2, 0.3}
	}
	return out, nil
}

// TestInsightSaver_PersistsChunksAndEmbeds verifies an insight is not merely
// upserted as a bare row: it is chunked and embedded, which is what makes it
// retrievable by the /ask vector lane (spec §6.5). A row with no chunks and no
// vector is an inference the system drew and can never surface again.
func TestInsightSaver_PersistsChunksAndEmbeds(t *testing.T) {
	t.Parallel()

	docs := &fakeNoteDocs{}
	chunks := &fakeNoteChunks{}
	embed := &fakeNoteEmbedder{}
	saver := NewInsightSaver(docs, chunks, embed)

	noteID := uuid.New()
	sourceID := insightSourceID(noteID, 0)
	metadata := map[string]any{
		"confidence":  0.55,
		"source_span": "예산 얘기를 피했다",
		"provenance":  map[string]any{"source_note_id": noteID.String()},
	}

	if err := saver.SaveInsight(context.Background(), "자금 사정이 어렵다", sourceID, metadata); err != nil {
		t.Fatalf("SaveInsight returned error: %v", err)
	}

	if len(docs.docs) != 1 {
		t.Fatalf("documents upserted = %d, want 1", len(docs.docs))
	}
	doc := docs.docs[0]
	if doc.SourceType != model.SourceInsight {
		t.Errorf("SourceType = %q, want %q (an insight must never be a note: it would re-enter enrichment)",
			doc.SourceType, model.SourceInsight)
	}
	if doc.SourceID != sourceID {
		t.Errorf("SourceID = %q, want %q", doc.SourceID, sourceID)
	}
	if doc.Content != "자금 사정이 어렵다" {
		t.Errorf("Content = %q, want the inference text", doc.Content)
	}
	if doc.Title != "" {
		t.Errorf("Title = %q, want empty (the origin note holds the title)", doc.Title)
	}
	if doc.Status != "active" || doc.CollectedAt.IsZero() {
		t.Errorf("Status/CollectedAt = %q/%v, want active and non-zero", doc.Status, doc.CollectedAt)
	}
	prov, ok := doc.Metadata["provenance"].(map[string]any)
	if !ok || prov["source_note_id"] != noteID.String() {
		t.Errorf("Metadata[provenance] = %#v, want source_note_id %s", doc.Metadata["provenance"], noteID)
	}

	if len(chunks.chunks) == 0 {
		t.Fatal("no chunks written: an unchunked insight is invisible to chunk-level search")
	}
	if len(embed.texts) == 0 {
		t.Fatal("no embedding requested: an unembedded insight is invisible to the /ask vector lane")
	}
	if len(chunks.embeddings) != len(chunks.chunks) {
		t.Errorf("embeddings persisted = %d, want %d (one per chunk)", len(chunks.embeddings), len(chunks.chunks))
	}
}

// TestInsightSaver_UpsertFailure_ReturnsError verifies a durability failure
// surfaces as an error, so the worker fails the enrichment attempt instead of
// marking the note done.
func TestInsightSaver_UpsertFailure_ReturnsError(t *testing.T) {
	t.Parallel()

	saver := NewInsightSaver(&fakeNoteDocs{err: errors.New("connection refused")}, &fakeNoteChunks{}, &fakeNoteEmbedder{})
	err := saver.SaveInsight(context.Background(), "content", insightSourceID(uuid.New(), 0), map[string]any{})
	if err == nil {
		t.Fatal("SaveInsight returned nil error when the document upsert failed, want error")
	}
}

// TestInsightSaver_ChunkFailure_ReturnsError verifies a chunk-write failure is
// also a durability failure: an insight with no chunks is not retrievable.
func TestInsightSaver_ChunkFailure_ReturnsError(t *testing.T) {
	t.Parallel()

	saver := NewInsightSaver(&fakeNoteDocs{}, &fakeNoteChunks{replaceErr: errors.New("deadlock")}, &fakeNoteEmbedder{})
	err := saver.SaveInsight(context.Background(), "content", insightSourceID(uuid.New(), 0), map[string]any{})
	if err == nil {
		t.Fatal("SaveInsight returned nil error when the chunk write failed, want error")
	}
}

// TestInsightSaver_EmbedFailure_Tolerated pins the one deliberately tolerated
// sub-case: an embedding failure does NOT fail the attempt. The document and
// its chunks are durably stored and the existing chunk-embedding backfill
// fills the vector in later, so retrying the whole enrichment would burn a
// paid LLM call for work a backfill already does for free.
func TestInsightSaver_EmbedFailure_Tolerated(t *testing.T) {
	t.Parallel()

	docs := &fakeNoteDocs{}
	chunks := &fakeNoteChunks{}
	saver := NewInsightSaver(docs, chunks, &fakeNoteEmbedder{err: errors.New("embed service down")})

	if err := saver.SaveInsight(context.Background(), "content", insightSourceID(uuid.New(), 0), map[string]any{}); err != nil {
		t.Fatalf("SaveInsight returned error on embedding failure: %v, want nil (document + chunks are stored)", err)
	}
	if len(docs.docs) != 1 || len(chunks.chunks) == 0 {
		t.Errorf("documents = %d, chunks = %d; want the insight and its chunks persisted despite the embed failure",
			len(docs.docs), len(chunks.chunks))
	}
	if len(chunks.embeddings) != 0 {
		t.Errorf("embeddings persisted = %d, want 0 when the embedder failed", len(chunks.embeddings))
	}
}

// TestInsightSaver_NilEmbedder_StillPersists verifies embedding is optional:
// with no embedder configured the insight is still stored and chunked.
func TestInsightSaver_NilEmbedder_StillPersists(t *testing.T) {
	t.Parallel()

	docs := &fakeNoteDocs{}
	chunks := &fakeNoteChunks{}
	saver := NewInsightSaver(docs, chunks, nil)

	if err := saver.SaveInsight(context.Background(), "content", insightSourceID(uuid.New(), 0), map[string]any{}); err != nil {
		t.Fatalf("SaveInsight returned error with a nil embedder: %v", err)
	}
	if len(docs.docs) != 1 || len(chunks.chunks) == 0 {
		t.Errorf("documents = %d, chunks = %d; want both persisted", len(docs.docs), len(chunks.chunks))
	}
}
