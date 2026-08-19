package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/baekenough/second-brain/internal/dataset"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/baekenough/second-brain/internal/telemetry"
	"github.com/baekenough/second-brain/internal/tune"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// Modes. Exactly one is selected per invocation.
const (
	// modePropose measures a candidate and records it. It never changes the
	// live configuration.
	modePropose = "propose"
	// modePromote does everything propose does and, if the gate allows it,
	// makes the candidate live.
	modePromote = "promote"
	// modeRollback undoes the current promotion.
	modeRollback = "rollback"
)

// errDisabled is returned when TUNE_ENABLED is not set to a true value.
var errDisabled = errors.New("tune: TUNE_ENABLED is not set; refusing to run")

const tracerName = "github.com/baekenough/second-brain/cmd/tune"

func tracer() oteltrace.Tracer { return otel.Tracer(tracerName) }

// tuneEnabled is the feature flag, default OFF.
//
// It gates every mode including rollback. Gating rollback is a deliberate
// inconvenience rather than an oversight: an operator undoing a promotion runs
// `TUNE_ENABLED=true tune -rollback`, which is one extra word, whereas an
// ungated mode would mean this binary can mutate the live configuration on a
// host where nobody has opted into tuning at all. The cheap rollback path does
// not go through this binary anyway — unsetting the flag and leaving the
// history alone already returns the system to its compiled-in defaults.
//
// Read straight from the environment rather than through internal/config: that
// file is under concurrent change and this flag has no other consumer.
func tuneEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TUNE_ENABLED"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// loaderFunc reads the labelled set. It is a function rather than a
// dataset.Source so that tests can hand in sets in states a Source cannot
// produce — in particular a holdout set whose evaluation budget is already
// spent.
type loaderFunc func(context.Context) (*dataset.TrainSet, *dataset.HoldoutSet, error)

// historyStore is the subset of store.WeightsHistoryStore this command uses.
type historyStore interface {
	Insert(ctx context.Context, rec store.WeightsRecord) (int64, error)
	Promote(ctx context.Context, id int64) error
	Rollback(ctx context.Context) (*store.WeightsRecord, error)
	Active(ctx context.Context) (*store.WeightsRecord, error)
}

// Compile-time proof that the production store still fits.
var _ historyStore = (*store.WeightsHistoryStore)(nil)

type deps struct {
	Load    loaderFunc
	Search  tune.Searcher
	History historyStore
}

type options struct {
	Mode   string
	DryRun bool
	// Space defaults to tune.DefaultSpace() when left zero.
	Space tune.SearchSpace
	// ToolVersion is recorded in the history row's metadata so that a decision
	// can be attributed to a build.
	ToolVersion string
}

// outcome is what one invocation did. It is printed as JSON and returned to the
// tests; it carries no query text and no document IDs.
type outcome struct {
	Mode string `json:"mode"`
	// HistoryID is the row written (propose/promote) or restored (rollback);
	// 0 when nothing was written.
	HistoryID      int64               `json:"history_id"`
	Promoted       bool                `json:"promoted"`
	GatePassed     bool                `json:"gate_passed"`
	GateReason     string              `json:"gate_reason"`
	GateDetail     string              `json:"gate_detail"`
	Weights        model.SearchWeights `json:"weights"`
	TrainQueries   int                 `json:"train_queries"`
	HoldoutQueries int                 `json:"holdout_queries"`
	Sweeps         int                 `json:"sweeps"`
	Evals          int                 `json:"evals"`
	TrainObjective float64             `json:"train_objective"`
	HoldoutNDCG10  float64             `json:"holdout_ndcg10"`
	HoldoutFP10    float64             `json:"holdout_fp10"`
	BaselineNDCG10 float64             `json:"baseline_ndcg10"`
	BaselineFP10   float64             `json:"baseline_fp10"`
	DryRun         bool                `json:"dry_run"`
	// Trace is the accepted-move log from coordinate search. Weights and
	// objective values only.
	Trace []string `json:"trace,omitempty"`
}

func run(ctx context.Context, d deps, o options) (outcome, error) {
	if !tuneEnabled() {
		return outcome{}, errDisabled
	}
	if d.History == nil {
		return outcome{}, errors.New("tune: no weights history store configured")
	}

	switch o.Mode {
	case "", modePropose:
		return runTuning(ctx, d, o, false)
	case modePromote:
		return runTuning(ctx, d, o, true)
	case modeRollback:
		return runRollback(ctx, d, o)
	default:
		return outcome{}, fmt.Errorf("tune: unknown mode %q (want %s, %s or %s)",
			o.Mode, modePropose, modePromote, modeRollback)
	}
}

// runTuning is the whole loop: load labels, search the training set, judge on
// the holdout set exactly twice, record, and — only in promote mode and only
// when the gate agrees — make the candidate live.
func runTuning(ctx context.Context, d deps, o options, allowPromote bool) (outcome, error) {
	if d.Load == nil || d.Search == nil {
		return outcome{}, errors.New("tune: loader and searcher are required")
	}
	space := o.Space
	if len(space.FTS) == 0 {
		space = tune.DefaultSpace()
	}

	train, holdout, err := d.Load(ctx)
	if err != nil {
		return outcome{}, fmt.Errorf("tune: load labelled set: %w", err)
	}
	out := outcome{
		Mode:           modeName(allowPromote),
		DryRun:         o.DryRun,
		TrainQueries:   train.Queries(),
		HoldoutQueries: holdout.Queries(),
	}
	emit(ctx, telemetry.SpanTuneDatasetLoaded,
		attribute.Int(telemetry.AttrTuneTrainQueries, out.TrainQueries),
		attribute.Int(telemetry.AttrTuneHoldoutQueries, out.HoldoutQueries),
		attribute.Int(telemetry.AttrTuneTotalRows, out.TrainQueries+out.HoldoutQueries),
	)

	// The incumbent is whatever is live, or the compiled-in defaults when
	// nothing has been promoted. Comparing against the defaults rather than
	// against the previous candidate is what makes a promotion mean "better
	// than what users are getting today".
	base := model.SearchWeights{}.Defaults()
	active, err := d.History.Active(ctx)
	if err != nil {
		return outcome{}, fmt.Errorf("tune: read active weights: %w", err)
	}
	if active != nil {
		base = active.Weights.Defaults()
	}

	search, err := tune.Coordinate(ctx, train, base, space,
		func(ctx context.Context, w model.SearchWeights) (dataset.Metrics, error) {
			return train.EvaluateTrain(ctx, tune.RunnerFor(d.Search, w))
		})
	if err != nil {
		return outcome{}, err
	}
	out.Weights = search.Best
	out.Sweeps = search.Sweeps
	out.Evals = search.Evals
	out.TrainObjective = search.BestObjective
	out.Trace = search.Trace
	emit(ctx, telemetry.SpanTuneCoordinate,
		attribute.Int(telemetry.AttrTuneSweeps, search.Sweeps),
		attribute.Int(telemetry.AttrTuneEvals, search.Evals),
		attribute.Float64(telemetry.AttrTuneBaseObjective, search.StartObjective),
		attribute.Float64(telemetry.AttrTuneBestObjective, search.BestObjective),
	)

	// The only two looks at the holdout set in the entire run.
	verdict, err := tune.Judge(ctx, holdout,
		tune.RunnerFor(d.Search, base), tune.RunnerFor(d.Search, search.Best))
	if err != nil {
		return outcome{}, err
	}
	gateIn := verdict.GateInput()
	passed, reason := tune.PassesPromotion(gateIn)

	out.GatePassed = passed
	out.GateReason = reason
	out.GateDetail = tune.Describe(reason, gateIn)
	out.HoldoutNDCG10 = verdict.Candidate.NDCG10
	out.HoldoutFP10 = verdict.Candidate.FPPenalty10
	out.BaselineNDCG10 = verdict.Baseline.NDCG10
	out.BaselineFP10 = verdict.Baseline.FPPenalty10

	emit(ctx, telemetry.SpanTunePromotion,
		attribute.Bool(telemetry.AttrTuneGatePassed, passed),
		// The reason CODE, not the detail sentence: the sentence carries
		// numbers that belong in Postgres, and free text on a span is how user
		// input eventually reaches a hosted service.
		attribute.String(telemetry.AttrTuneGateReason, reason),
		attribute.Float64(telemetry.AttrTuneNDCGGain, gateIn.NDCGGain()),
		attribute.Float64(telemetry.AttrTuneFPDelta, gateIn.FPDelta()),
		attribute.Int(telemetry.AttrTuneHoldoutQueries, gateIn.HoldoutQueries),
	)

	if o.DryRun {
		return out, nil
	}

	// Refused candidates are recorded too. Without the row there is no way to
	// answer "why did the weights not change last month" later.
	trainObj := search.BestObjective
	detail := out.GateDetail
	rec := store.WeightsRecord{
		Weights:        search.Best,
		Status:         store.WeightsStatusProposed,
		TrainObjective: &trainObj,
		HoldoutNDCG10:  &out.HoldoutNDCG10,
		HoldoutFP10:    &out.HoldoutFP10,
		BaselineNDCG10: &out.BaselineNDCG10,
		BaselineFP10:   &out.BaselineFP10,
		HoldoutQueries: out.HoldoutQueries,
		GateReason:     &detail,
		Metadata: map[string]any{
			// Recorded so that these numbers are never mistaken for what the
			// live, reranked pipeline produces.
			"rerank": false,
			// The overfitting tell-tale: a training objective far above the
			// holdout objective means the search fitted the labels it could see.
			"train_holdout_gap": search.BestObjective - verdict.Candidate.Objective,
			"sweeps":            search.Sweeps,
			"evals":             search.Evals,
			"gate_reason_code":  reason,
			"tool_version":      o.ToolVersion,
		},
	}
	id, err := d.History.Insert(ctx, rec)
	if err != nil {
		return outcome{}, fmt.Errorf("tune: record candidate: %w", err)
	}
	out.HistoryID = id
	emit(ctx, telemetry.SpanWeightsAction,
		attribute.String(telemetry.AttrWeightsAction, telemetry.WeightsActionPropose),
		attribute.Int64(telemetry.AttrWeightsHistoryID, id),
	)

	if !allowPromote || !passed {
		return out, nil
	}

	if err := d.History.Promote(ctx, id); err != nil {
		return outcome{}, fmt.Errorf("tune: promote candidate: %w", err)
	}
	out.Promoted = true
	emit(ctx, telemetry.SpanWeightsAction,
		attribute.String(telemetry.AttrWeightsAction, telemetry.WeightsActionPromote),
		attribute.Int64(telemetry.AttrWeightsHistoryID, id),
	)
	return out, nil
}

func runRollback(ctx context.Context, d deps, _ options) (outcome, error) {
	restored, err := d.History.Rollback(ctx)
	if err != nil {
		return outcome{}, fmt.Errorf("tune: rollback: %w", err)
	}
	out := outcome{Mode: modeRollback}
	attrs := []attribute.KeyValue{
		attribute.String(telemetry.AttrWeightsAction, telemetry.WeightsActionRollback),
	}
	if restored != nil {
		out.HistoryID = restored.ID
		out.Weights = restored.Weights
		attrs = append(attrs,
			attribute.Int64(telemetry.AttrWeightsHistoryID, restored.ID),
			attribute.Int64(telemetry.AttrWeightsPreviousHistoryID, restored.ID))
	}
	// A rollback with nothing to restore is a success: the system is back on
	// its compiled-in defaults, which is the state rollback exists to reach.
	emit(ctx, telemetry.SpanWeightsAction, attrs...)
	return out, nil
}

func modeName(allowPromote bool) string {
	if allowPromote {
		return modePromote
	}
	return modePropose
}

// emit records a zero-duration event span. The tracer is looked up per call:
// the global provider is installed in main() after package initialisation, so a
// package-level tracer would be a permanent no-op.
func emit(ctx context.Context, name string, attrs ...attribute.KeyValue) {
	_, span := tracer().Start(ctx, name, oteltrace.WithAttributes(attrs...))
	span.End()
}
