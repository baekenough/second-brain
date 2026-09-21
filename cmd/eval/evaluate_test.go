package main

import (
	"context"
	"errors"
	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
	"testing"
)

type controlledSearch struct{ failAll bool }

func (s controlledSearch) Search(_ context.Context, q model.SearchQuery) ([]*model.SearchResult, error) {
	if s.failAll || q.Query == "fail" {
		return nil, errors.New("synthetic failure")
	}
	return []*model.SearchResult{{Document: model.Document{ID: uuid.MustParse("00000000-0000-0000-0000-000000000001")}}}, nil
}
func TestEvaluationCountsFailuresAndNegativeOnlyQuestions(t *testing.T) {
	id := "00000000-0000-0000-0000-000000000001"
	pairs := []store.EvalPair{{Query: "hit", RelevantDocIDs: []string{id}}, {Query: "fail", RelevantDocIDs: []string{id}}, {Query: "negative", IrrelevantDocIDs: []string{id}}}
	got := evaluatePairs(context.Background(), controlledSearch{}, pairs, evalRunOptions{rerank: true})
	if got.Attempted != 3 || got.Failed != 1 || got.PositiveQueries != 2 || got.NegativeQueries != 1 || got.Metrics.NDCG10 != 0.5 || got.FPPenalty10 != 0.1 {
		t.Fatalf("unexpected evaluation: %+v", got)
	}
	if got.completionError() == nil {
		t.Fatal("partial failure must fail the run")
	}
	failed := evaluatePairs(context.Background(), controlledSearch{failAll: true}, pairs, evalRunOptions{rerank: true})
	if failed.completionError() == nil {
		t.Fatal("all failed run reported success")
	}
	if failed.Failed != 3 || failed.Attempted != 3 || failed.Metrics.NDCG10 != 0 {
		t.Fatalf("all failures disappeared: %+v", failed)
	}
}
func TestFingerprintsMatchOnlyEquivalentEvaluationInputs(t *testing.T) {
	a := []store.EvalPair{{Query: "q2", RelevantDocIDs: []string{"b", "a"}}, {Query: "q1", IrrelevantDocIDs: []string{"c"}}}
	b := []store.EvalPair{{Query: "q1", IrrelevantDocIDs: []string{"c"}}, {Query: "q2", RelevantDocIDs: []string{"a", "b"}}}
	if labelFingerprint(a) != labelFingerprint(b) {
		t.Fatal("ordering changed label identity")
	}
	b[0].IrrelevantDocIDs = []string{"different"}
	if labelFingerprint(a) == labelFingerprint(b) {
		t.Fatal("different labels matched")
	}
	cfg := &config.Config{RerankURL: "https://example.invalid", RerankAPIKey: "secret", RerankModel: "model", RerankTopN: 10}
	profile := runConfiguration(cfg, true, false, (model.SearchWeights{}).Defaults(), nil)
	other := runConfiguration(cfg, false, false, (model.SearchWeights{}).Defaults(), nil)
	if digest(profile) == digest(other) {
		t.Fatal("rerank modes matched")
	}
	cfg.RerankAPIKey = "rotated-secret"
	if digest(profile) != digest(runConfiguration(cfg, true, false, (model.SearchWeights{}).Defaults(), nil)) {
		t.Fatal("credential rotation changed model semantics")
	}
}
