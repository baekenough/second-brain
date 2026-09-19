package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/baekenough/second-brain/internal/classify"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// ClassificationDocumentStore is the document-store dependency of
// ClassificationWorker. *store.DocumentStore satisfies it.
type ClassificationDocumentStore interface {
	// ListUnclassified returns up to limit active documents (sms/gmail/call)
	// that have never been tagged with retention.
	ListUnclassified(ctx context.Context, limit int, backfillDays int) ([]*model.Document, error)
	// ListLegacyForRecheck returns up to limit already retention-tagged
	// documents (any classifier value except "user") that have not yet been
	// gate-audited (classifier_gate_checked_at unset).
	ListLegacyForRecheck(ctx context.Context, limit int) ([]*model.Document, error)
	// MergeClassificationMetadata merges updates into a document's
	// metadata, refusing to touch classifier="user" documents.
	MergeClassificationMetadata(ctx context.Context, documentID uuid.UUID, updates map[string]any) error
	// IncrementClassificationAttempts records a failed classification
	// attempt, eventually excluding the document from future listings.
	IncrementClassificationAttempts(ctx context.Context, documentID uuid.UUID, attempts int) error
}

// ClassificationJevClassifier is the classify.Classifier dependency of
// ClassificationWorker, narrowed to an interface for testability.
type ClassificationJevClassifier interface {
	JevEnabled() bool
	ClassifyDeterministic(gate classify.Gate) *classify.Result
	ClassifyWithJev(ctx context.Context, doc *model.Document, gate classify.Gate) (*classify.Result, int, error)
}

// ClassificationGateEvaluator is the gate-evaluation dependency of
// ClassificationWorker, narrowed to an interface so tests can substitute a
// fixed-response fake instead of a real *classify.Evaluator (which needs a
// SenderLookup). A fresh instance MUST be constructed per tick — see
// classify.NewEvaluator's doc comment on its per-tick cache.
type ClassificationGateEvaluator interface {
	Evaluate(ctx context.Context, doc *model.Document) (classify.Gate, error)
}

// ClassificationGateEvaluatorFactory constructs a ClassificationGateEvaluator
// for a single tick. Defaults to classify.NewEvaluator wrapping the
// configured SenderLookup.
type ClassificationGateEvaluatorFactory func() ClassificationGateEvaluator

// ClassificationWorkerConfig holds configuration for ClassificationWorker.
type ClassificationWorkerConfig struct {
	// Store provides document listing and metadata write-back.
	Store ClassificationDocumentStore
	// Classifier performs the deterministic/Jev classification decision.
	Classifier ClassificationJevClassifier
	// NewEvaluator constructs a fresh gate evaluator for each tick. Required.
	NewEvaluator ClassificationGateEvaluatorFactory
	// Interval controls how often the worker polls. Defaults to 10 minutes.
	Interval time.Duration
	// BatchSize is the number of documents processed per tick (across both
	// the unclassified and legacy-recheck queues combined). Defaults to 50.
	BatchSize int
	// BackfillDays restricts ListUnclassified to documents collected within
	// the last N days. 0 (default) means no limit — the entire history is
	// eligible.
	BackfillDays int
	// RecheckLegacy enables the legacy-recheck queue. Defaults to true.
	RecheckLegacy bool
	// MaxCallsPerTick caps the number of Jev API calls made in a single
	// tick, shared across both queues. Defaults to 200.
	MaxCallsPerTick int
}

// classificationTickStats accumulates the counters logged once per tick (spec:
// listed/tagged_rule/tagged_jev/rechecked/skipped_gate/failed/input_tokens).
// Never populated with document content, sender addresses, or phone
// numbers — only counts.
type classificationTickStats struct {
	listed      int
	taggedRule  int
	taggedJev   int
	rechecked   int
	skippedGate int
	failed      int
	inputTokens int
}

// ClassificationWorker is a background worker that tags SMS/Gmail/call
// documents with a segment/retention classification (rule-based where
// possible, Jev-based otherwise), and separately re-audits legacy
// (pre-worker) tags against the deterministic Gate.
//
// See internal/classify's package doc for the Gate's asymmetric design and
// internal/jev for the TypeSafe Jev client this worker calls through
// Classifier.
type ClassificationWorker struct {
	store           ClassificationDocumentStore
	classifier      ClassificationJevClassifier
	newEvaluator    ClassificationGateEvaluatorFactory
	interval        time.Duration
	batchSize       int
	backfillDays    int
	recheckLegacy   bool
	maxCallsPerTick int
}

// NewClassificationWorker constructs a ClassificationWorker from cfg.
// Panics when required fields are nil (programming error).
func NewClassificationWorker(cfg ClassificationWorkerConfig) *ClassificationWorker {
	if cfg.Store == nil {
		panic("ClassificationWorkerConfig.Store must not be nil")
	}
	if cfg.Classifier == nil {
		panic("ClassificationWorkerConfig.Classifier must not be nil")
	}
	if cfg.NewEvaluator == nil {
		panic("ClassificationWorkerConfig.NewEvaluator must not be nil")
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 50
	}
	maxCalls := cfg.MaxCallsPerTick
	if maxCalls <= 0 {
		maxCalls = 200
	}
	return &ClassificationWorker{
		store:           cfg.Store,
		classifier:      cfg.Classifier,
		newEvaluator:    cfg.NewEvaluator,
		interval:        interval,
		batchSize:       batchSize,
		backfillDays:    cfg.BackfillDays,
		recheckLegacy:   cfg.RecheckLegacy,
		maxCallsPerTick: maxCalls,
	}
}

// Run starts the classification loop. It blocks until ctx is cancelled.
// When Jev is not configured, tagging still proceeds for documents the
// deterministic Gate fully decides (auth/ad/short-call rules) — only the
// Jev-requiring documents are skipped, one diagnostic log per tick via tick's
// own logging, not a permanent idle like EntityWorker/SummarizerWorker (whose
// entire pipeline is LLM-only).
func (w *ClassificationWorker) Run(ctx context.Context) {
	slog.Info("classification worker started",
		"interval", w.interval, "batch_size", w.batchSize,
		"backfill_days", w.backfillDays, "recheck_legacy", w.recheckLegacy,
		"jev_enabled", w.classifier.JevEnabled())
	w.tick(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("classification worker stopped")
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

// tick processes one batch from the unclassified queue and, when enabled,
// one batch from the legacy-recheck queue, sharing a single per-tick gate
// evaluator (for its SenderLookup cache) and Jev call budget.
func (w *ClassificationWorker) tick(ctx context.Context) {
	evaluator := w.newEvaluator()
	stats := classificationTickStats{}
	callsRemaining := w.maxCallsPerTick

	docs, err := w.store.ListUnclassified(ctx, w.batchSize, w.backfillDays)
	if err != nil {
		slog.Warn("classification worker: list unclassified failed", "error", err)
	} else {
		stats.listed += len(docs)
		for _, doc := range docs {
			callsRemaining = w.classifyOne(ctx, doc, evaluator, callsRemaining, &stats)
		}
	}

	if w.recheckLegacy {
		legacy, err := w.store.ListLegacyForRecheck(ctx, w.batchSize)
		if err != nil {
			slog.Warn("classification worker: list legacy for recheck failed", "error", err)
		} else {
			stats.listed += len(legacy)
			for _, doc := range legacy {
				callsRemaining = w.recheckOne(ctx, doc, evaluator, callsRemaining, &stats)
			}
		}
	}

	if stats.listed == 0 {
		slog.Debug("classification worker: tick idle (nothing listed)")
		return
	}

	slog.Info("classification worker: tick complete",
		"listed", stats.listed,
		"tagged_rule", stats.taggedRule,
		"tagged_jev", stats.taggedJev,
		"rechecked", stats.rechecked,
		"skipped_gate", stats.skippedGate,
		"failed", stats.failed,
		"input_tokens", stats.inputTokens)
}

// classifyOne classifies a single unclassified-queue document, calling Jev
// only when the Gate did not already decide the tag and budget remains.
// Returns the updated callsRemaining.
func (w *ClassificationWorker) classifyOne(ctx context.Context, doc *model.Document, evaluator ClassificationGateEvaluator, callsRemaining int, stats *classificationTickStats) int {
	gate, err := evaluator.Evaluate(ctx, doc)
	if err != nil {
		slog.Warn("classification worker: gate evaluation failed", "doc_id", doc.ID, "error", err)
		stats.failed++
		return callsRemaining
	}

	if gate.Decided != nil {
		result := w.classifier.ClassifyDeterministic(gate)
		if w.apply(ctx, doc.ID, result, stats) {
			stats.taggedRule++
		}
		return callsRemaining
	}

	if !w.classifier.JevEnabled() || callsRemaining <= 0 {
		// Leave unclassified for a future tick; no attempts increment since
		// no call was actually attempted.
		return callsRemaining
	}

	result, tokens, err := w.classifier.ClassifyWithJev(ctx, doc, gate)
	stats.inputTokens += tokens
	callsRemaining--
	if err != nil {
		w.recordFailure(ctx, doc, stats, err)
		return callsRemaining
	}
	if w.apply(ctx, doc.ID, result, stats) {
		stats.taggedJev++
	}
	return callsRemaining
}

// recheckOne re-audits a single legacy-tagged document against the Gate. A
// contradiction is fixed (via rule when the Gate itself decided, or via a
// fresh Jev call otherwise); a non-contradiction is simply marked
// gate-checked so ListLegacyForRecheck stops returning it. Returns the
// updated callsRemaining.
func (w *ClassificationWorker) recheckOne(ctx context.Context, doc *model.Document, evaluator ClassificationGateEvaluator, callsRemaining int, stats *classificationTickStats) int {
	stats.rechecked++

	gate, err := evaluator.Evaluate(ctx, doc)
	if err != nil {
		slog.Warn("classification worker: legacy gate evaluation failed", "doc_id", doc.ID, "error", err)
		stats.failed++
		return callsRemaining
	}

	currentRetention, _ := doc.RetentionTag()

	switch {
	case gate.Decided != nil:
		// A rule now fully decides this document (e.g. content matches the
		// auth/ad pattern) — apply it directly regardless of the prior tag.
		result := w.classifier.ClassifyDeterministic(gate)
		w.apply(ctx, doc.ID, result, stats)
		stats.taggedRule++
		return callsRemaining

	case gate.PersonSignal && currentRetention == model.RetentionDisposable:
		// PersonSignal contradiction: floor to low+needs_review without a
		// Jev call — spec: "PersonSignal이면서 disposable → low+needs_review,
		// Jev 불필요". The existing segment is preserved; only retention and
		// needs_review change.
		segment, _ := doc.Metadata["segment"].(string)
		result := &classify.Result{
			Segment:     segment,
			Retention:   model.RetentionLow,
			Classifier:  "rule",
			ClassifierP: 1.0,
			NeedsReview: true,
			Gate:        gate,
		}
		w.apply(ctx, doc.ID, result, stats)
		stats.taggedRule++
		return callsRemaining

	case gate.BulkSender && currentRetention == model.RetentionKeep:
		// BulkSender contradiction: the legacy classifier called this a
		// keep-worthy conversation despite bulk-sender signals — spec:
		// "BulkSender이면서 keep → Jev 재판정". Re-run through Jev so the
		// segment itself gets re-evaluated, not just the retention value.
		if !w.classifier.JevEnabled() || callsRemaining <= 0 {
			return callsRemaining
		}
		result, tokens, err := w.classifier.ClassifyWithJev(ctx, doc, gate)
		stats.inputTokens += tokens
		callsRemaining--
		if err != nil {
			w.recordFailure(ctx, doc, stats, err)
			return callsRemaining
		}
		w.apply(ctx, doc.ID, result, stats)
		stats.taggedJev++
		return callsRemaining

	default:
		// No contradiction: mark gate-checked so this row is not re-listed
		// on every future tick (see ListLegacyForRecheck's doc comment).
		stats.skippedGate++
		if err := w.store.MergeClassificationMetadata(ctx, doc.ID, map[string]any{
			"classifier_gate_checked_at": time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			slog.Warn("classification worker: mark gate-checked failed", "doc_id", doc.ID, "error", err)
		}
		return callsRemaining
	}
}

// apply merges result into doc's metadata. Returns true on success (used by
// callers to decide whether to count the tag in tagged_rule/tagged_jev).
func (w *ClassificationWorker) apply(ctx context.Context, documentID uuid.UUID, result *classify.Result, stats *classificationTickStats) bool {
	if err := w.store.MergeClassificationMetadata(ctx, documentID, result.Metadata(time.Now())); err != nil {
		slog.Warn("classification worker: apply classification failed", "doc_id", documentID, "error", err)
		stats.failed++
		return false
	}
	return true
}

// recordFailure increments the document's classifier_attempts counter (spec:
// 3-attempt cap) and logs a warning. Never logs doc.Content or err's
// underlying message if it might embed request/response bodies — the
// jev package's own errors are already shape-only (see internal/jev's
// package doc comment).
func (w *ClassificationWorker) recordFailure(ctx context.Context, doc *model.Document, stats *classificationTickStats, causeErr error) {
	stats.failed++
	attempts := 1
	if v, ok := doc.Metadata["classifier_attempts"]; ok {
		switch n := v.(type) {
		case float64:
			attempts = int(n) + 1
		case int:
			attempts = n + 1
		}
	}
	slog.Warn("classification worker: jev classification failed",
		"doc_id", doc.ID, "source_type", doc.SourceType, "attempts", attempts, "error", causeErr)
	if err := w.store.IncrementClassificationAttempts(ctx, doc.ID, attempts); err != nil {
		slog.Warn("classification worker: record attempt failure failed", "doc_id", doc.ID, "error", err)
	}
}
