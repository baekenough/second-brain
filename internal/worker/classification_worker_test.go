package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/baekenough/second-brain/internal/classify"
	"github.com/baekenough/second-brain/internal/model"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

type mergeCall struct {
	id      uuid.UUID
	updates map[string]any
}

type attemptCall struct {
	id       uuid.UUID
	attempts int
}

// fakeClassificationStore is an in-memory ClassificationDocumentStore double.
type fakeClassificationStore struct {
	mu sync.Mutex

	unclassified []*model.Document
	legacy       []*model.Document

	listUnclassifiedErr error
	listLegacyErr       error
	mergeErr            error

	merged   []mergeCall
	attempts []attemptCall
}

func (f *fakeClassificationStore) ListUnclassified(_ context.Context, limit int, _ int) ([]*model.Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listUnclassifiedErr != nil {
		return nil, f.listUnclassifiedErr
	}
	out := f.unclassified
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeClassificationStore) ListLegacyForRecheck(_ context.Context, limit int) ([]*model.Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listLegacyErr != nil {
		return nil, f.listLegacyErr
	}
	out := f.legacy
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeClassificationStore) MergeClassificationMetadata(_ context.Context, id uuid.UUID, updates map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mergeErr != nil {
		return f.mergeErr
	}
	f.merged = append(f.merged, mergeCall{id: id, updates: updates})
	return nil
}

func (f *fakeClassificationStore) IncrementClassificationAttempts(_ context.Context, id uuid.UUID, attempts int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts = append(f.attempts, attemptCall{id: id, attempts: attempts})
	return nil
}

func (f *fakeClassificationStore) mergeFor(id uuid.UUID) (map[string]any, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.merged {
		if m.id == id {
			return m.updates, true
		}
	}
	return nil, false
}

// fakeGateEvaluator returns a pre-programmed Gate per document (keyed by
// SourceID), or an error when configured.
type fakeGateEvaluator struct {
	gates map[string]classify.Gate
	err   error
}

func (f *fakeGateEvaluator) Evaluate(_ context.Context, doc *model.Document) (classify.Gate, error) {
	if f.err != nil {
		return classify.Gate{}, f.err
	}
	return f.gates[doc.SourceID], nil
}

// fakeClassifier is a ClassificationJevClassifier double with call counters.
type fakeClassifier struct {
	mu sync.Mutex

	jevEnabled bool
	jevErr     error
	jevResult  *classify.Result
	jevTokens  int

	jevCalls int
}

func (f *fakeClassifier) JevEnabled() bool { return f.jevEnabled }

func (f *fakeClassifier) ClassifyDeterministic(gate classify.Gate) *classify.Result {
	return &classify.Result{
		Segment:     gate.Decided.Segment,
		Retention:   gate.Decided.Retention,
		Classifier:  "rule",
		ClassifierP: 1.0,
		Gate:        gate,
	}
}

func (f *fakeClassifier) ClassifyWithJev(_ context.Context, _ *model.Document, gate classify.Gate) (*classify.Result, int, error) {
	f.mu.Lock()
	f.jevCalls++
	f.mu.Unlock()
	if f.jevErr != nil {
		return nil, 0, f.jevErr
	}
	result := *f.jevResult
	result.Gate = gate
	return &result, f.jevTokens, nil
}

func (f *fakeClassifier) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.jevCalls
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func classDoc(sourceID string, sourceType model.SourceType, meta map[string]any) *model.Document {
	return &model.Document{
		ID:         uuid.New(),
		SourceID:   sourceID,
		SourceType: sourceType,
		Content:    "irrelevant to these stubs",
		Metadata:   meta,
	}
}

// newTestClassificationWorker builds a worker with fixed low-level fields
// (bypassing NewClassificationWorker's env-driven defaults) for deterministic
// tests.
func newTestClassificationWorker(store ClassificationDocumentStore, classifier ClassificationJevClassifier, evaluator ClassificationGateEvaluator, recheckLegacy bool, maxCalls int) *ClassificationWorker {
	return &ClassificationWorker{
		store:      store,
		classifier: classifier,
		newEvaluator: func() ClassificationGateEvaluator {
			return evaluator
		},
		interval:        time.Minute,
		batchSize:       50,
		backfillDays:    0,
		recheckLegacy:   recheckLegacy,
		maxCallsPerTick: maxCalls,
	}
}

// ---------------------------------------------------------------------------
// Unclassified-queue tests
// ---------------------------------------------------------------------------

func TestTick_Unclassified_RuleDecided_NoJevCall(t *testing.T) {
	doc := classDoc("sms-1", model.SourceSMS, nil)
	store := &fakeClassificationStore{unclassified: []*model.Document{doc}}
	evaluator := &fakeGateEvaluator{gates: map[string]classify.Gate{
		"sms-1": {Decided: &classify.Tag{Segment: "auth_transient", Retention: model.RetentionDisposable}},
	}}
	classifier := &fakeClassifier{jevEnabled: true}
	w := newTestClassificationWorker(store, classifier, evaluator, false, 200)

	w.tick(context.Background())

	if classifier.callCount() != 0 {
		t.Errorf("jevCalls = %d, want 0 for a rule-decided document", classifier.callCount())
	}
	updates, ok := store.mergeFor(doc.ID)
	if !ok {
		t.Fatal("MergeClassificationMetadata was not called for the rule-decided document")
	}
	if updates["segment"] != "auth_transient" || updates["retention"] != model.RetentionDisposable {
		t.Errorf("updates = %+v, want segment=auth_transient retention=disposable", updates)
	}
	if updates["classifier"] != "rule" {
		t.Errorf("updates[classifier] = %v, want rule", updates["classifier"])
	}
}

func TestTick_Unclassified_JevRequired_CallsJevAndTags(t *testing.T) {
	doc := classDoc("sms-2", model.SourceSMS, nil)
	store := &fakeClassificationStore{unclassified: []*model.Document{doc}}
	evaluator := &fakeGateEvaluator{gates: map[string]classify.Gate{"sms-2": {}}}
	classifier := &fakeClassifier{
		jevEnabled: true,
		jevResult:  &classify.Result{Segment: "personal_comm", Retention: model.RetentionKeep, Classifier: "jev-latest", ClassifierP: 0.8},
		jevTokens:  30,
	}
	w := newTestClassificationWorker(store, classifier, evaluator, false, 200)

	w.tick(context.Background())

	if classifier.callCount() != 1 {
		t.Fatalf("jevCalls = %d, want 1", classifier.callCount())
	}
	updates, ok := store.mergeFor(doc.ID)
	if !ok {
		t.Fatal("MergeClassificationMetadata was not called after a successful Jev classification")
	}
	if updates["segment"] != "personal_comm" || updates["classifier"] != "jev-latest" {
		t.Errorf("updates = %+v, want segment=personal_comm classifier=jev-latest", updates)
	}
}

func TestTick_Unclassified_JevDisabled_LeavesDocumentUntouched(t *testing.T) {
	doc := classDoc("sms-3", model.SourceSMS, nil)
	store := &fakeClassificationStore{unclassified: []*model.Document{doc}}
	evaluator := &fakeGateEvaluator{gates: map[string]classify.Gate{"sms-3": {}}}
	classifier := &fakeClassifier{jevEnabled: false}
	w := newTestClassificationWorker(store, classifier, evaluator, false, 200)

	w.tick(context.Background())

	if classifier.callCount() != 0 {
		t.Errorf("jevCalls = %d, want 0 when Jev is disabled", classifier.callCount())
	}
	if _, ok := store.mergeFor(doc.ID); ok {
		t.Error("MergeClassificationMetadata was called even though Jev is disabled and the gate made no decision")
	}
	if len(store.attempts) != 0 {
		t.Error("IncrementClassificationAttempts was called even though no attempt was made (Jev disabled, not a failure)")
	}
}

func TestTick_Unclassified_JevCallFails_IncrementsAttempts(t *testing.T) {
	doc := classDoc("sms-4", model.SourceSMS, nil)
	store := &fakeClassificationStore{unclassified: []*model.Document{doc}}
	evaluator := &fakeGateEvaluator{gates: map[string]classify.Gate{"sms-4": {}}}
	classifier := &fakeClassifier{jevEnabled: true, jevErr: errors.New("jev: timeout")}
	w := newTestClassificationWorker(store, classifier, evaluator, false, 200)

	w.tick(context.Background())

	if len(store.attempts) != 1 {
		t.Fatalf("attempts recorded = %d, want 1", len(store.attempts))
	}
	if store.attempts[0].id != doc.ID || store.attempts[0].attempts != 1 {
		t.Errorf("attempts[0] = %+v, want {id: %v, attempts: 1}", store.attempts[0], doc.ID)
	}
	if _, ok := store.mergeFor(doc.ID); ok {
		t.Error("MergeClassificationMetadata must not be called for a failed classification")
	}
}

func TestTick_Unclassified_JevCallFails_AttemptsIncrementFromExistingCount(t *testing.T) {
	doc := classDoc("sms-5", model.SourceSMS, map[string]any{"classifier_attempts": float64(2)})
	store := &fakeClassificationStore{unclassified: []*model.Document{doc}}
	evaluator := &fakeGateEvaluator{gates: map[string]classify.Gate{"sms-5": {}}}
	classifier := &fakeClassifier{jevEnabled: true, jevErr: errors.New("jev: timeout")}
	w := newTestClassificationWorker(store, classifier, evaluator, false, 200)

	w.tick(context.Background())

	if len(store.attempts) != 1 || store.attempts[0].attempts != 3 {
		t.Fatalf("attempts = %+v, want a single record with attempts=3 (2 existing + 1)", store.attempts)
	}
}

// TestTick_MaxCallsPerTick_SharedAcrossQueues verifies the Jev call budget is
// shared between the unclassified and legacy-recheck queues within one tick.
func TestTick_MaxCallsPerTick_SharedAcrossQueues(t *testing.T) {
	docA := classDoc("sms-a", model.SourceSMS, nil)
	docB := classDoc("sms-b", model.SourceSMS, nil)
	legacyDoc := classDoc("gmail-legacy", model.SourceGmail, map[string]any{
		"retention": model.RetentionKeep, "classifier": "ox-alpha",
	})

	store := &fakeClassificationStore{
		unclassified: []*model.Document{docA, docB},
		legacy:       []*model.Document{legacyDoc},
	}
	evaluator := &fakeGateEvaluator{gates: map[string]classify.Gate{
		"sms-a":        {},                 // needs Jev
		"sms-b":        {},                 // needs Jev
		"gmail-legacy": {BulkSender: true}, // contradicts keep -> needs Jev
	}}
	classifier := &fakeClassifier{
		jevEnabled: true,
		jevResult:  &classify.Result{Segment: "notification", Retention: model.RetentionLow, Classifier: "jev-latest"},
	}
	w := newTestClassificationWorker(store, classifier, evaluator, true, 1)

	w.tick(context.Background())

	if classifier.callCount() != 1 {
		t.Fatalf("jevCalls = %d, want exactly 1 (MaxCallsPerTick=1 shared across both queues)", classifier.callCount())
	}
	if len(store.merged) != 1 {
		t.Errorf("merged = %d, want exactly 1 document tagged", len(store.merged))
	}
}

// ---------------------------------------------------------------------------
// Legacy-recheck queue tests
// ---------------------------------------------------------------------------

func TestTick_Legacy_PersonSignalContradiction_FixedWithoutJev(t *testing.T) {
	doc := classDoc("sms-legacy-1", model.SourceSMS, map[string]any{
		"retention":  model.RetentionDisposable,
		"classifier": "ox-alpha",
		"segment":    "ad",
	})
	store := &fakeClassificationStore{legacy: []*model.Document{doc}}
	evaluator := &fakeGateEvaluator{gates: map[string]classify.Gate{"sms-legacy-1": {PersonSignal: true}}}
	classifier := &fakeClassifier{jevEnabled: true}
	w := newTestClassificationWorker(store, classifier, evaluator, true, 200)

	w.tick(context.Background())

	if classifier.callCount() != 0 {
		t.Errorf("jevCalls = %d, want 0 — PersonSignal contradiction must be fixed without a Jev call (spec)", classifier.callCount())
	}
	updates, ok := store.mergeFor(doc.ID)
	if !ok {
		t.Fatal("MergeClassificationMetadata was not called for the PersonSignal contradiction")
	}
	if updates["retention"] != model.RetentionLow {
		t.Errorf("updates[retention] = %v, want low", updates["retention"])
	}
	if updates["needs_review"] != true {
		t.Errorf("updates[needs_review] = %v, want true", updates["needs_review"])
	}
	if updates["segment"] != "ad" {
		t.Errorf("updates[segment] = %v, want ad (preserved from the existing tag)", updates["segment"])
	}
}

func TestTick_Legacy_BulkSenderContradiction_CallsJev(t *testing.T) {
	doc := classDoc("gmail-legacy-1", model.SourceGmail, map[string]any{
		"retention": model.RetentionKeep, "classifier": "ox-alpha",
	})
	store := &fakeClassificationStore{legacy: []*model.Document{doc}}
	evaluator := &fakeGateEvaluator{gates: map[string]classify.Gate{"gmail-legacy-1": {BulkSender: true}}}
	classifier := &fakeClassifier{
		jevEnabled: true,
		jevResult:  &classify.Result{Segment: "newsletter", Retention: model.RetentionLow, Classifier: "jev-latest"},
	}
	w := newTestClassificationWorker(store, classifier, evaluator, true, 200)

	w.tick(context.Background())

	if classifier.callCount() != 1 {
		t.Fatalf("jevCalls = %d, want 1 — BulkSender contradiction must re-run Jev (spec)", classifier.callCount())
	}
	updates, ok := store.mergeFor(doc.ID)
	if !ok {
		t.Fatal("MergeClassificationMetadata was not called after the Jev re-classification")
	}
	if updates["segment"] != "newsletter" {
		t.Errorf("updates[segment] = %v, want newsletter", updates["segment"])
	}
}

func TestTick_Legacy_NoContradiction_MarksGateCheckedWithoutRetagging(t *testing.T) {
	doc := classDoc("gmail-legacy-2", model.SourceGmail, map[string]any{
		"retention": model.RetentionKeep, "classifier": "ox-alpha", "segment": "work_comm",
	})
	store := &fakeClassificationStore{legacy: []*model.Document{doc}}
	evaluator := &fakeGateEvaluator{gates: map[string]classify.Gate{"gmail-legacy-2": {}}}
	classifier := &fakeClassifier{jevEnabled: true}
	w := newTestClassificationWorker(store, classifier, evaluator, true, 200)

	w.tick(context.Background())

	if classifier.callCount() != 0 {
		t.Errorf("jevCalls = %d, want 0 — no contradiction means no re-classification", classifier.callCount())
	}
	updates, ok := store.mergeFor(doc.ID)
	if !ok {
		t.Fatal("MergeClassificationMetadata was not called to mark the row gate-checked")
	}
	if _, hasSegment := updates["segment"]; hasSegment {
		t.Errorf("updates = %+v, must not rewrite segment/retention when there is no contradiction", updates)
	}
	if _, hasMarker := updates["classifier_gate_checked_at"]; !hasMarker {
		t.Errorf("updates = %+v, want classifier_gate_checked_at marker so this row is not re-listed forever", updates)
	}
}

func TestTick_Legacy_GateDecided_OverridesRegardlessOfCurrentTag(t *testing.T) {
	doc := classDoc("call-legacy-1", model.SourceCallTranscript, map[string]any{
		"retention": model.RetentionKeep, "classifier": "ox-alpha",
	})
	store := &fakeClassificationStore{legacy: []*model.Document{doc}}
	evaluator := &fakeGateEvaluator{gates: map[string]classify.Gate{
		"call-legacy-1": {Decided: &classify.Tag{Segment: "short_call", Retention: model.RetentionLow}},
	}}
	classifier := &fakeClassifier{jevEnabled: true}
	w := newTestClassificationWorker(store, classifier, evaluator, true, 200)

	w.tick(context.Background())

	updates, ok := store.mergeFor(doc.ID)
	if !ok {
		t.Fatal("MergeClassificationMetadata was not called for a Decided gate on a legacy row")
	}
	if updates["segment"] != "short_call" || updates["retention"] != model.RetentionLow {
		t.Errorf("updates = %+v, want segment=short_call retention=low (rule overrides the stale keep tag)", updates)
	}
}

func TestTick_RecheckLegacyDisabled_NeverListsLegacyQueue(t *testing.T) {
	doc := classDoc("gmail-legacy-3", model.SourceGmail, map[string]any{"retention": model.RetentionKeep, "classifier": "ox-alpha"})
	store := &fakeClassificationStore{legacy: []*model.Document{doc}}
	evaluator := &fakeGateEvaluator{gates: map[string]classify.Gate{}}
	classifier := &fakeClassifier{jevEnabled: true}
	w := newTestClassificationWorker(store, classifier, evaluator, false, 200)

	w.tick(context.Background())

	if len(store.merged) != 0 {
		t.Errorf("merged = %d, want 0 — RecheckLegacy=false must skip the legacy queue entirely", len(store.merged))
	}
}

// ---------------------------------------------------------------------------
// Error-path tests
// ---------------------------------------------------------------------------

func TestTick_ListUnclassifiedError_DoesNotPanicAndStillRunsLegacyQueue(t *testing.T) {
	legacyDoc := classDoc("gmail-legacy-4", model.SourceGmail, map[string]any{"retention": model.RetentionKeep, "classifier": "ox-alpha"})
	store := &fakeClassificationStore{
		listUnclassifiedErr: errors.New("db: connection reset"),
		legacy:              []*model.Document{legacyDoc},
	}
	evaluator := &fakeGateEvaluator{gates: map[string]classify.Gate{"gmail-legacy-4": {}}}
	classifier := &fakeClassifier{jevEnabled: true}
	w := newTestClassificationWorker(store, classifier, evaluator, true, 200)

	w.tick(context.Background())

	if _, ok := store.mergeFor(legacyDoc.ID); !ok {
		t.Error("a ListUnclassified error must not prevent the legacy-recheck queue from running")
	}
}

func TestTick_GateEvaluationError_SkipsDocumentWithoutCrashing(t *testing.T) {
	doc := classDoc("sms-err", model.SourceSMS, nil)
	store := &fakeClassificationStore{unclassified: []*model.Document{doc}}
	evaluator := &fakeGateEvaluator{err: errors.New("sender lookup failed")}
	classifier := &fakeClassifier{jevEnabled: true}
	w := newTestClassificationWorker(store, classifier, evaluator, false, 200)

	w.tick(context.Background())

	if _, ok := store.mergeFor(doc.ID); ok {
		t.Error("a gate evaluation error must not result in a metadata write")
	}
	if classifier.callCount() != 0 {
		t.Error("a gate evaluation error must not result in a Jev call")
	}
}
