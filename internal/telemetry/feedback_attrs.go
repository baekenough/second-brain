package telemetry

// Span names and attribute keys for the search-feedback loop: evidence-level
// thumbs collection (internal/api), weight tuning (cmd/tune, internal/tune)
// and weight promotion/rollback (internal/store/weights_history.go).
//
// Why span attributes rather than a metrics pipeline: this package exports
// traces to Langfuse over OTLP and configures no metric exporter (otel.go).
// Adding one for a handful of counters would mean a second delivery path to
// operate. A promotion span carrying tune.ndcg_gain plotted over time in
// Langfuse *is* the "holdout score trend" chart, and a feedback.evidence
// span per vote *is* the collection-rate series — the trace timeline answers
// "did the change make things better" without a second system.
//
// What may appear here, and what may not:
//
//   - Allowed: document UUIDs, closed-domain enums (thumbs, split, gate
//     reason codes), and aggregate scalars.
//   - Not allowed: query text, and — unlike the local logs, which may carry
//     dataset.QueryHash — not even the query hash. Langfuse is a hosted
//     service whose retention outlives this project's logs, so the safe
//     amount of user speech to send there is none. Reproducing a decision
//     needs feedback.split and search_weights_history, both of which stay in
//     Postgres.
//
// TestAttrKeys_MatchAllowlist parses this file and fails if the constant set
// drifts from the allowlist in the test, so a new attribute cannot be added
// without someone reading the paragraph above.
const (
	// SpanFeedbackEvidence is emitted once per successfully recorded evidence
	// vote. Failed requests do not emit a span — a failure is an HTTP-layer
	// concern and is already logged there.
	SpanFeedbackEvidence = "feedback.evidence.recorded"
	// SpanTuneDatasetLoaded reports the size of each split at the start of a
	// tuning run: this is the "training set growth" series.
	SpanTuneDatasetLoaded = "tune.dataset.loaded"
	// SpanTuneCoordinate reports the outcome of coordinate search on the
	// training set (train-set objectives only, never holdout).
	SpanTuneCoordinate = "tune.coordinate.completed"
	// SpanTunePromotion reports the holdout verdict and the gate decision.
	SpanTunePromotion = "tune.promotion.decision"
	// SpanWeightsAction reports an actual state change in
	// search_weights_history: propose, promote or rollback.
	SpanWeightsAction = "weights.action.applied"
)

const (
	// AttrFeedbackDocumentID is the voted-on document's UUID. An opaque
	// identifier, not content.
	AttrFeedbackDocumentID = "feedback.document_id"
	// AttrFeedbackThumbs is the vote the database settled on: -1, 0 (cleared)
	// or 1.
	AttrFeedbackThumbs = "feedback.thumbs"
	// AttrFeedbackSplit is "train" or "holdout" — which half of the labelled
	// set this vote landed in.
	AttrFeedbackSplit = "feedback.split"

	// AttrTuneTrainQueries and AttrTuneHoldoutQueries are the two split sizes.
	AttrTuneTrainQueries   = "tune.train_queries"
	AttrTuneHoldoutQueries = "tune.holdout_queries"
	// AttrTuneTotalRows is the total number of labelled evaluation pairs behind
	// a run — the feedback-collection trend.
	AttrTuneTotalRows = "tune.total_feedback_rows"
	// AttrTuneSweeps and AttrTuneEvals describe how much work the search did.
	AttrTuneSweeps = "tune.sweeps"
	AttrTuneEvals  = "tune.evals"
	// AttrTuneBaseObjective and AttrTuneBestObjective are TRAINING-set
	// objectives: where the search started and where it ended.
	AttrTuneBaseObjective = "tune.base_objective"
	AttrTuneBestObjective = "tune.best_objective"
	// AttrTuneGatePassed is the promotion decision.
	AttrTuneGatePassed = "tune.gate_passed"
	// AttrTuneGateReason is a fixed enum code from internal/tune (gate.go),
	// never free text and never the human sentence stored in Postgres — free
	// text is how user input eventually leaks into a trace.
	AttrTuneGateReason = "tune.gate_reason"
	// AttrTuneNDCGGain and AttrTuneFPDelta are the holdout deltas the gate
	// judged. Plotted over runs, these are the holdout score trend.
	AttrTuneNDCGGain = "tune.ndcg_gain"
	AttrTuneFPDelta  = "tune.fp_delta"

	// AttrWeightsHistoryID is the search_weights_history primary key.
	AttrWeightsHistoryID = "weights.history_id"
	// AttrWeightsAction is one of the WeightsAction* values below.
	AttrWeightsAction = "weights.action"
	// AttrWeightsPreviousHistoryID is the row a rollback restored, when there
	// was one.
	AttrWeightsPreviousHistoryID = "weights.previous_history_id"
)

// Closed set of values for AttrWeightsAction. These are payload rather than
// keys, which is why the allowlist test ignores them — but they are constants
// so that the batch command and any future dashboard query agree on spelling.
const (
	WeightsActionPropose  = "propose"
	WeightsActionPromote  = "promote"
	WeightsActionRollback = "rollback"
)
