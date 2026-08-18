package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/action"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// parseRelationActionResponse (parser slice)
// ---------------------------------------------------------------------------
//
// Named parseRelationActionResponse, NOT parseExtractionResponse: the latter
// name is already taken in this package by entity_extractor.go's
// func parseExtractionResponse(raw string) ([]model.Entity, error) — a
// same-package name collision the plan's draft code did not account for.
// This is a deliberate deviation from the plan text (see Task 11 report).

// TestParseRelationActionResponse_ValidJSON verifies the happy path parses
// entities, relations, and actions from a single well-formed JSON object.
func TestParseRelationActionResponse_ValidJSON(t *testing.T) {
	t.Parallel()
	raw := `{"entities":[{"name":"Alice","type":"PERSON"}],
	"relations":[{"from":"Alice","from_type":"PERSON","to":"Bob","to_type":"PERSON","type":"communicated_with"}],
	"actions":[{"kind":"my_commitment","summary":"Send report Friday","counterpart":"Bob","counterpart_type":"PERSON","confidence":0.8}]}`
	got, err := parseRelationActionResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Entities) != 1 || len(got.Relations) != 1 || len(got.Actions) != 1 {
		t.Fatalf("got %+v", got)
	}
}

// TestParseRelationActionResponse_TrailingGarbage_StillParses is the direct
// regression test for the reported defect: "invalid character 'W' after
// top-level value". json.NewDecoder(...).Decode(...) (not json.Unmarshal)
// stops after the first complete JSON value and ignores what follows.
func TestParseRelationActionResponse_TrailingGarbage_StillParses(t *testing.T) {
	t.Parallel()
	raw := `{"entities":[],"relations":[],"actions":[]}WWW some trailing note the model appended`
	got, err := parseRelationActionResponse(raw)
	if err != nil {
		t.Fatalf("expected trailing garbage to be tolerated, got error: %v", err)
	}
	if len(got.Entities) != 0 {
		t.Errorf("got %+v, want empty result", got)
	}
}

// TestParseRelationActionResponse_MarkdownFenced_StillParses verifies a
// ```json ... ``` fenced response (defensive, mirrors parseEnrichmentResponse).
func TestParseRelationActionResponse_MarkdownFenced_StillParses(t *testing.T) {
	t.Parallel()
	raw := "```json\n{\"entities\":[],\"relations\":[],\"actions\":[]}\n```"
	if _, err := parseRelationActionResponse(raw); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestParseRelationActionResponse_TotalGarbage_ReturnsError verifies
// completely non-JSON output is a hard error (not a silently-empty result),
// so the caller applies the retry policy instead of marking success.
func TestParseRelationActionResponse_TotalGarbage_ReturnsError(t *testing.T) {
	t.Parallel()
	if _, err := parseRelationActionResponse("not json at all"); err == nil {
		t.Fatal("expected error for non-JSON input")
	}
}

// TestParseRelationActionResponse_LeadingProse_FallbackParses verifies the
// secondary fallback path: leading prose before the JSON object still parses
// via the first-'{'-to-last-'}' substring retry.
func TestParseRelationActionResponse_LeadingProse_FallbackParses(t *testing.T) {
	t.Parallel()
	raw := `Sure, here is the JSON: {"entities":[],"relations":[],"actions":[]} Hope that helps!`
	got, err := parseRelationActionResponse(raw)
	if err != nil {
		t.Fatalf("expected fallback substring parse to succeed, got error: %v", err)
	}
	if len(got.Entities) != 0 || len(got.Relations) != 0 || len(got.Actions) != 0 {
		t.Errorf("got %+v, want empty result", got)
	}
}

// TestParseRelationActionResponse_InvalidRelationType_Downgraded verifies the
// parser preserves the RAW type string unchanged — downgrade to
// model.RelationRelatedTo happens at persistence (via
// model.DowngradeRelationType), not at parse time.
func TestParseRelationActionResponse_InvalidRelationType_Downgraded(t *testing.T) {
	t.Parallel()
	raw := `{"entities":[],"relations":[{"from":"A","from_type":"PERSON","to":"B","to_type":"PERSON","type":"friends_with"}],"actions":[]}`
	got, err := parseRelationActionResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Relations) != 1 || got.Relations[0].RawType != "friends_with" {
		t.Fatalf("parser must preserve the RAW type string — downgrade happens at persistence, not parsing: got %+v", got)
	}
	if model.DowngradeRelationType(got.Relations[0].RawType) != model.RelationRelatedTo {
		t.Errorf("DowngradeRelationType(%q) = %q, want %q",
			got.Relations[0].RawType, model.DowngradeRelationType(got.Relations[0].RawType), model.RelationRelatedTo)
	}
}

// ---------------------------------------------------------------------------
// Test doubles for the ExtractionWorker fake-interface tests
// ---------------------------------------------------------------------------

// fakeExtractionLister is an in-memory ExtractionLister for unit testing.
// Every method can be made to fail — see fakeNoteLister's doc comment in
// note_enrichment_worker_test.go for why that matters (bookkeeping-on-a-live-
// context bugs are invisible unless bookkeeping calls can themselves fail).
type fakeExtractionLister struct {
	mu                 sync.Mutex
	docs               []*model.Document
	listErr            error
	markExtractedErr   error
	attemptFailedErr   error
	terminalErr        error
	markExtractedCalls []uuid.UUID
	attemptFailedCalls []extractionAttemptFailedCall
	terminalCalls      []extractionTerminalCall
}

type extractionAttemptFailedCall struct {
	documentID  uuid.UUID
	attempts    int
	reason      string
	nextRetryAt time.Time
}

type extractionTerminalCall struct {
	documentID uuid.UUID
	reason     string
}

func (f *fakeExtractionLister) ListPendingForExtraction(_ context.Context, limit int) ([]*model.Document, error) {
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

func (f *fakeExtractionLister) MarkRelationsExtracted(_ context.Context, documentID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markExtractedCalls = append(f.markExtractedCalls, documentID)
	return f.markExtractedErr
}

func (f *fakeExtractionLister) MarkExtractionAttemptFailed(_ context.Context, documentID uuid.UUID, attempts int, reason string, nextRetryAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attemptFailedCalls = append(f.attemptFailedCalls, extractionAttemptFailedCall{documentID, attempts, reason, nextRetryAt})
	if f.attemptFailedErr != nil {
		return f.attemptFailedErr
	}
	f.setAttemptsLocked(documentID, attempts)
	return nil
}

func (f *fakeExtractionLister) MarkExtractionTerminal(_ context.Context, documentID uuid.UUID, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminalCalls = append(f.terminalCalls, extractionTerminalCall{documentID, reason})
	return f.terminalErr
}

func (f *fakeExtractionLister) setAttemptsLocked(id uuid.UUID, attempts int) {
	for _, d := range f.docs {
		if d.ID == id {
			if d.Metadata == nil {
				d.Metadata = map[string]any{}
			}
			d.Metadata["extraction_attempts"] = float64(attempts)
		}
	}
}

// fakeRelationEntityResolver implements RelationEntityResolver
// (EntityLinker + UpsertEntity). It assigns a stable, deterministic id per
// (name, type) pair so relation/action persistence tests can assert on it.
type fakeRelationEntityResolver struct {
	mu        sync.Mutex
	nextID    int64
	ids       map[string]int64
	linkCalls int
	upsertErr error
	linkErr   error
}

func (f *fakeRelationEntityResolver) UpsertAndLinkEntities(_ context.Context, _ uuid.UUID, _ []model.Entity) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.linkCalls++
	return f.linkErr
}

func (f *fakeRelationEntityResolver) UpsertEntity(_ context.Context, name string, entityType model.EntityType) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return 0, f.upsertErr
	}
	if f.ids == nil {
		f.ids = map[string]int64{}
	}
	key := name + "|" + string(entityType)
	if id, ok := f.ids[key]; ok {
		return id, nil
	}
	f.nextID++
	f.ids[key] = f.nextID
	return f.nextID, nil
}

// fakeRelationWriter records UpsertEntityRelations calls.
type fakeRelationWriter struct {
	mu    sync.Mutex
	calls [][]model.EntityRelation
	err   error
}

func (f *fakeRelationWriter) UpsertEntityRelations(_ context.Context, relations []model.EntityRelation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, relations)
	return f.err
}

func (f *fakeRelationWriter) totalRelations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		n += len(c)
	}
	return n
}

// fakeActionWriter records UpsertAction and EnsureOpenStatus calls.
type fakeActionWriter struct {
	mu              sync.Mutex
	upsertedActions []model.Action
	openStatusCalls []string
	upsertErr       error
	openStatusErr   error
}

func (f *fakeActionWriter) UpsertAction(_ context.Context, a model.Action) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.upsertedActions = append(f.upsertedActions, a)
	return nil
}

func (f *fakeActionWriter) EnsureOpenStatus(_ context.Context, identityKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.openStatusErr != nil {
		return f.openStatusErr
	}
	f.openStatusCalls = append(f.openStatusCalls, identityKey)
	return nil
}

func newExtractionDoc(id uuid.UUID, attempts int) *model.Document {
	return &model.Document{
		ID:         id,
		SourceType: model.SourceGmail,
		SourceID:   "gmail-source",
		Title:      "Re: Budget",
		Content:    "Let's discuss the Q3 budget.",
		Metadata: map[string]any{
			"thread_id":           "thread-1",
			"from":                "counterpart@example.com",
			"extraction_attempts": float64(attempts),
		},
	}
}

// newTestExtractionWorker builds an ExtractionWorker with an injectable
// extractFn, mirroring newTestNoteWorker's pattern.
func newTestExtractionWorker(
	lister ExtractionLister,
	entities RelationEntityResolver,
	relations RelationWriter,
	actions ActionWriter,
	fn func(context.Context, llm.Completer, *model.Document, []string) (*ExtractionResult, error),
) *ExtractionWorker {
	return &ExtractionWorker{
		store:               lister,
		entities:            entities,
		relations:           relations,
		actions:             actions,
		llm:                 &fakeLLM{},
		userAddrs:           []string{"me@example.com"},
		interval:            time.Minute,
		batchSize:           10,
		maxAttempts:         3,
		retryBackoff:        []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute},
		workTimeoutV:        30 * time.Second,
		persistTimeoutV:     30 * time.Second,
		bookkeepingTimeoutV: 30 * time.Second,
		extractFn:           fn,
	}
}

// ---------------------------------------------------------------------------
// ExtractionWorker tests
// ---------------------------------------------------------------------------

// TestExtractionWorker_Success_MarksRelationsExtracted verifies a successful
// tick calls MarkRelationsExtracted exactly once and persists the relation.
func TestExtractionWorker_Success_MarksRelationsExtracted(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	lister := &fakeExtractionLister{docs: []*model.Document{newExtractionDoc(docID, 0)}}
	entities := &fakeRelationEntityResolver{}
	relations := &fakeRelationWriter{}
	actions := &fakeActionWriter{}

	calls := 0
	w := newTestExtractionWorker(lister, entities, relations, actions,
		func(_ context.Context, _ llm.Completer, _ *model.Document, _ []string) (*ExtractionResult, error) {
			calls++
			return &ExtractionResult{
				Relations: []rawRelation{
					{FromName: "Alice", FromType: model.EntityTypePerson, ToName: "Bob", ToType: model.EntityTypePerson, RawType: "communicated_with", Confidence: 0.8},
				},
			}, nil
		},
	)
	w.tick(context.Background())

	if calls != 1 {
		t.Fatalf("extractFn called %d times, want 1 (single LLM call per document)", calls)
	}
	if len(lister.markExtractedCalls) != 1 || lister.markExtractedCalls[0] != docID {
		t.Errorf("MarkRelationsExtracted calls = %v, want one call for %s", lister.markExtractedCalls, docID)
	}
	if relations.totalRelations() != 1 {
		t.Errorf("relations persisted = %d, want 1", relations.totalRelations())
	}
}

// TestExtractionWorker_RelationWriteFails_RoutesToHandleFailure verifies a
// RelationWriter error is treated as a failed extraction attempt (attempts=1),
// not silently swallowed.
func TestExtractionWorker_RelationWriteFails_RoutesToHandleFailure(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	lister := &fakeExtractionLister{docs: []*model.Document{newExtractionDoc(docID, 0)}}
	entities := &fakeRelationEntityResolver{}
	relations := &fakeRelationWriter{err: errors.New("db down")}
	actions := &fakeActionWriter{}

	w := newTestExtractionWorker(lister, entities, relations, actions,
		func(_ context.Context, _ llm.Completer, _ *model.Document, _ []string) (*ExtractionResult, error) {
			return &ExtractionResult{
				Relations: []rawRelation{
					{FromName: "Alice", FromType: model.EntityTypePerson, ToName: "Bob", ToType: model.EntityTypePerson, RawType: "communicated_with", Confidence: 0.8},
				},
			}, nil
		},
	)
	w.tick(context.Background())

	if len(lister.markExtractedCalls) != 0 {
		t.Errorf("MarkRelationsExtracted called %d times, want 0 (a failed relation write must not commit)", len(lister.markExtractedCalls))
	}
	if len(lister.attemptFailedCalls) != 1 || lister.attemptFailedCalls[0].attempts != 1 {
		t.Fatalf("attemptFailedCalls = %+v, want one call with attempts=1", lister.attemptFailedCalls)
	}
}

// TestExtractionWorker_ThirdFailure_MarksTerminal verifies the 3rd
// consecutive failure calls MarkExtractionTerminal instead of
// MarkExtractionAttemptFailed (mirrors NoteEnrichmentWorker's 3-attempt cap
// — see plan Global Constraints, adopted over EntityWorker's unbounded retry).
func TestExtractionWorker_ThirdFailure_MarksTerminal(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	lister := &fakeExtractionLister{docs: []*model.Document{newExtractionDoc(docID, 2)}} // already failed twice
	entities := &fakeRelationEntityResolver{}
	relations := &fakeRelationWriter{}
	actions := &fakeActionWriter{}

	w := newTestExtractionWorker(lister, entities, relations, actions,
		func(_ context.Context, _ llm.Completer, _ *model.Document, _ []string) (*ExtractionResult, error) {
			return nil, errors.New("LLM timeout")
		},
	)
	w.tick(context.Background())

	if len(lister.terminalCalls) != 1 {
		t.Fatalf("MarkExtractionTerminal called %d times, want 1 (3rd failure is terminal)", len(lister.terminalCalls))
	}
	if len(lister.attemptFailedCalls) != 0 {
		t.Errorf("MarkExtractionAttemptFailed called %d times, want 0 when hitting the terminal attempt", len(lister.attemptFailedCalls))
	}
}

// TestExtractionWorker_AwaitingMyReply_UserOwnAddress_Dropped exercises
// action.CounterpartIdentity's ok=false path end-to-end through the worker: a
// (test-only, prompt-violating) extractFn returns kind=awaiting_my_reply for
// a document whose "from" metadata IS the configured user address — the
// worker must silently drop it (a message the user sent themselves cannot be
// "awaiting my reply").
func TestExtractionWorker_AwaitingMyReply_UserOwnAddress_Dropped(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	doc := newExtractionDoc(docID, 0)
	doc.Metadata["from"] = "me@example.com" // matches newTestExtractionWorker's userAddrs
	lister := &fakeExtractionLister{docs: []*model.Document{doc}}
	entities := &fakeRelationEntityResolver{}
	relations := &fakeRelationWriter{}
	actions := &fakeActionWriter{}

	w := newTestExtractionWorker(lister, entities, relations, actions,
		func(_ context.Context, _ llm.Completer, _ *model.Document, _ []string) (*ExtractionResult, error) {
			return &ExtractionResult{
				Actions: []rawAction{
					{Kind: string(model.KindAwaitingMyReply), Summary: "should be ignored"},
				},
			}, nil
		},
	)
	w.tick(context.Background())

	if len(actions.upsertedActions) != 0 {
		t.Errorf("UpsertAction called %d times, want 0 (own-address awaiting_my_reply must be dropped)", len(actions.upsertedActions))
	}
	if len(lister.markExtractedCalls) != 1 {
		t.Errorf("MarkRelationsExtracted calls = %d, want 1 (dropping one action must not fail the tick)", len(lister.markExtractedCalls))
	}
}

// TestExtractionWorker_ValidAction_PersistsAndOpensStatus verifies a
// well-formed non-awaiting_my_reply action is persisted via UpsertAction
// followed by EnsureOpenStatus for the same identity_key.
func TestExtractionWorker_ValidAction_PersistsAndOpensStatus(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	lister := &fakeExtractionLister{docs: []*model.Document{newExtractionDoc(docID, 0)}}
	entities := &fakeRelationEntityResolver{}
	relations := &fakeRelationWriter{}
	actions := &fakeActionWriter{}

	w := newTestExtractionWorker(lister, entities, relations, actions,
		func(_ context.Context, _ llm.Completer, _ *model.Document, _ []string) (*ExtractionResult, error) {
			return &ExtractionResult{
				Actions: []rawAction{
					{Kind: string(model.KindMyCommitment), Summary: "Send report Friday", Counterpart: "Bob", CounterpartType: model.EntityTypePerson, Confidence: 0.7},
				},
			}, nil
		},
	)
	w.tick(context.Background())

	if len(actions.upsertedActions) != 1 {
		t.Fatalf("UpsertAction called %d times, want 1", len(actions.upsertedActions))
	}
	got := actions.upsertedActions[0]
	if got.Kind != model.KindMyCommitment || got.DetectedBy != model.DetectedLLM {
		t.Errorf("action = %+v, want kind=%q detected_by=%q", got, model.KindMyCommitment, model.DetectedLLM)
	}
	if len(actions.openStatusCalls) != 1 || actions.openStatusCalls[0] != got.IdentityKey {
		t.Errorf("EnsureOpenStatus calls = %v, want one call for %q", actions.openStatusCalls, got.IdentityKey)
	}
}

// TestExtractionWorker_InvalidActionKind_Dropped verifies an action naming a
// kind outside the closed vocabulary is dropped without failing the tick.
func TestExtractionWorker_InvalidActionKind_Dropped(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	lister := &fakeExtractionLister{docs: []*model.Document{newExtractionDoc(docID, 0)}}
	entities := &fakeRelationEntityResolver{}
	relations := &fakeRelationWriter{}
	actions := &fakeActionWriter{}

	w := newTestExtractionWorker(lister, entities, relations, actions,
		func(_ context.Context, _ llm.Completer, _ *model.Document, _ []string) (*ExtractionResult, error) {
			return &ExtractionResult{
				Actions: []rawAction{
					{Kind: "invented_kind", Summary: "not a real kind"},
				},
			}, nil
		},
	)
	w.tick(context.Background())

	if len(actions.upsertedActions) != 0 {
		t.Errorf("UpsertAction called %d times, want 0 (invalid kind must be dropped)", len(actions.upsertedActions))
	}
	if len(lister.markExtractedCalls) != 1 {
		t.Errorf("MarkRelationsExtracted calls = %d, want 1", len(lister.markExtractedCalls))
	}
}

// TestExtractionWorker_EmptyBatch_NoOp verifies tick is a no-op when
// ListPendingForExtraction returns nothing.
func TestExtractionWorker_EmptyBatch_NoOp(t *testing.T) {
	t.Parallel()

	lister := &fakeExtractionLister{docs: nil}
	entities := &fakeRelationEntityResolver{}
	relations := &fakeRelationWriter{}
	actions := &fakeActionWriter{}

	w := newTestExtractionWorker(lister, entities, relations, actions,
		func(_ context.Context, _ llm.Completer, _ *model.Document, _ []string) (*ExtractionResult, error) {
			t.Error("extractFn should not be called when no documents are returned")
			return nil, nil
		},
	)
	w.tick(context.Background())

	if len(lister.markExtractedCalls) != 0 {
		t.Error("expected no side effects on an empty batch")
	}
}

// TestNewExtractionWorker_Defaults verifies the constructor's fallback
// values, notably the 3-attempt cap that bounds LLM spend per document.
func TestNewExtractionWorker_Defaults(t *testing.T) {
	t.Parallel()

	w := NewExtractionWorker(ExtractionWorkerConfig{
		Store:     &fakeExtractionLister{},
		Entities:  &fakeRelationEntityResolver{},
		Relations: &fakeRelationWriter{},
		Actions:   &fakeActionWriter{},
		LLM:       &fakeLLM{},
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
	if w.extractFn == nil {
		t.Error("extractFn must default to ExtractRelationsAndActions")
	}
}

// TestExtractionWorker_AwaitingMyReply_CarriesCounterpartLabel closes the
// second half of the "nameless action card" defect on the LLM path. The
// counterpart of an awaiting_my_reply action is fixed by document metadata
// (never named by the LLM, so the structural and LLM candidates converge on
// one identity_key) — which used to mean it was never resolved to an entity
// either, leaving counterpart_entity_id NULL. The worker now states the
// display label and the store resolves it, WITHOUT that label entering the
// identity_key.
func TestExtractionWorker_AwaitingMyReply_CarriesCounterpartLabel(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	doc := newExtractionDoc(docID, 0)
	doc.Metadata["from"] = `"테스트발신자" <counterpart@example.com>`
	lister := &fakeExtractionLister{docs: []*model.Document{doc}}
	actions := &fakeActionWriter{}

	w := newTestExtractionWorker(lister, &fakeRelationEntityResolver{}, &fakeRelationWriter{}, actions,
		func(_ context.Context, _ llm.Completer, _ *model.Document, _ []string) (*ExtractionResult, error) {
			return &ExtractionResult{
				Actions: []rawAction{
					{Kind: string(model.KindAwaitingMyReply), Summary: "dummy llm summary", Confidence: 0.6},
				},
			}, nil
		},
	)
	w.tick(context.Background())

	if len(actions.upsertedActions) != 1 {
		t.Fatalf("UpsertAction called %d times, want 1", len(actions.upsertedActions))
	}
	got := actions.upsertedActions[0]
	if got.CounterpartName != "테스트발신자" || got.CounterpartType != model.EntityTypePerson {
		t.Errorf("counterpart label = (%q, %q), want (%q, %q)", got.CounterpartName, got.CounterpartType, "테스트발신자", model.EntityTypePerson)
	}
	wantKey := action.BuildIdentityKey("gmail:thread-1", string(model.KindAwaitingMyReply), `"테스트발신자" <counterpart@example.com>`, "")
	if got.IdentityKey != wantKey {
		t.Errorf("IdentityKey = %q, want %q (the raw From header, not the display label, is hashed)", got.IdentityKey, wantKey)
	}
}
