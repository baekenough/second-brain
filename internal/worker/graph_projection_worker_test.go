package worker

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
)

// --- fakes ---

type fakeGraphSource struct {
	mu        sync.Mutex
	relations []model.ProjectionRelation
	mentions  []model.ProjectionMention
	relErr    error
	mentErr   error

	relCalls  []int64
	mentCalls []string
}

func (f *fakeGraphSource) ListRelationsAfter(_ context.Context, afterID int64, limit int) ([]model.ProjectionRelation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.relCalls = append(f.relCalls, afterID)
	if f.relErr != nil {
		return nil, f.relErr
	}
	out := make([]model.ProjectionRelation, 0, limit)
	for _, r := range f.relations {
		if r.ID > afterID && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeGraphSource) ListMentionsAfter(_ context.Context, afterDocumentID string, afterEntityID int64, limit int) ([]model.ProjectionMention, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mentCalls = append(f.mentCalls, afterDocumentID)
	if f.mentErr != nil {
		return nil, f.mentErr
	}
	out := make([]model.ProjectionMention, 0, limit)
	for _, m := range f.mentions {
		after := m.DocumentID > afterDocumentID ||
			(m.DocumentID == afterDocumentID && m.EntityID > afterEntityID)
		if afterDocumentID == "" {
			after = true
		}
		if after && len(out) < limit {
			out = append(out, m)
		}
	}
	return out, nil
}

type fakeProjector struct {
	state          model.GraphProjectionState
	lastRelationID int64
	resetToken     string
	wiped          bool
	schemaCalls    int

	upsertErr   error
	upsertCalls int
	mentionRows int
}

func (f *fakeProjector) EnsureSchema(context.Context) error { f.schemaCalls++; return nil }
func (f *fakeProjector) State(context.Context) (model.GraphProjectionState, error) {
	return f.state, nil
}

func (f *fakeProjector) UpsertRelations(_ context.Context, rows []model.ProjectionRelation) error {
	f.upsertCalls++
	if f.upsertErr != nil {
		return f.upsertErr
	}
	_ = rows
	return nil
}

func (f *fakeProjector) UpsertMentions(_ context.Context, rows []model.ProjectionMention) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.mentionRows += len(rows)
	return nil
}

func (f *fakeProjector) SetLastRelationID(_ context.Context, id int64) error {
	f.lastRelationID = id
	f.state.LastRelationID = id
	return nil
}

func (f *fakeProjector) SetResetToken(_ context.Context, token string) error {
	f.resetToken = token
	f.state.ResetToken = token
	return nil
}

func (f *fakeProjector) Wipe(context.Context) error {
	f.wiped = true
	f.state = model.GraphProjectionState{}
	return nil
}

// --- tests ---

func TestGraphProjectionWorker_TickAdvancesWatermark(t *testing.T) {
	src := &fakeGraphSource{relations: []model.ProjectionRelation{{ID: 7}, {ID: 9}}}
	proj := &fakeProjector{}
	NewGraphProjectionWorker(GraphProjectionWorkerConfig{Source: src, Projector: proj, BatchSize: 10, Overlap: 1000}).
		tick(context.Background())
	if proj.lastRelationID != 9 {
		t.Fatalf("lastRelationID = %d, want 9", proj.lastRelationID)
	}
}

func TestGraphProjectionWorker_ResetTokenTriggersWipe(t *testing.T) {
	proj := &fakeProjector{state: model.GraphProjectionState{LastRelationID: 100, ResetToken: "old"}}
	NewGraphProjectionWorker(GraphProjectionWorkerConfig{Source: &fakeGraphSource{}, Projector: proj, ResetToken: "new"}).
		tick(context.Background())
	if !proj.wiped {
		t.Fatal("reset token changed: want Wipe() called")
	}
	if proj.resetToken != "new" {
		t.Fatalf("resetToken = %q, want %q", proj.resetToken, "new")
	}
}

// TestGraphProjectionWorker_SameResetTokenDoesNotWipe is the other half of the
// rebuild switch: the default (empty token on both sides) must never destroy
// the graph on a routine tick.
func TestGraphProjectionWorker_SameResetTokenDoesNotWipe(t *testing.T) {
	proj := &fakeProjector{state: model.GraphProjectionState{LastRelationID: 100, ResetToken: ""}}
	NewGraphProjectionWorker(GraphProjectionWorkerConfig{Source: &fakeGraphSource{}, Projector: proj, ResetToken: ""}).
		tick(context.Background())
	if proj.wiped {
		t.Fatal("unchanged reset token: Wipe() must not be called")
	}
}

// TestGraphProjectionWorker_SurvivesProjectorError is the core safety
// property: a batch that failed to project must NOT move the watermark.
// Advancing past a failed batch loses those relations permanently, because
// nothing ever revisits ids below the watermark.
func TestGraphProjectionWorker_SurvivesProjectorError(t *testing.T) {
	proj := &fakeProjector{upsertErr: errors.New("neo4j down")}
	NewGraphProjectionWorker(GraphProjectionWorkerConfig{
		Source: &fakeGraphSource{relations: []model.ProjectionRelation{{ID: 1}}}, Projector: proj,
	}).tick(context.Background()) // must not panic
	if proj.lastRelationID != 0 {
		t.Fatalf("watermark advanced past a failed batch: %d, want 0", proj.lastRelationID)
	}
}

// TestGraphProjectionWorker_SurvivesSourceError covers the same invariant for
// a PostgreSQL-side failure.
func TestGraphProjectionWorker_SurvivesSourceError(t *testing.T) {
	proj := &fakeProjector{state: model.GraphProjectionState{LastRelationID: 5}}
	NewGraphProjectionWorker(GraphProjectionWorkerConfig{
		Source: &fakeGraphSource{relErr: errors.New("postgres down")}, Projector: proj,
	}).tick(context.Background())
	if proj.lastRelationID != 0 {
		t.Fatalf("SetLastRelationID called after a source error (=%d); watermark must not move", proj.lastRelationID)
	}
}

// TestGraphProjectionWorker_RereadsOverlapWindow pins the BIGSERIAL
// commit-order defence: each cycle re-reads Overlap ids below the watermark,
// which is only safe because the projection is idempotent.
func TestGraphProjectionWorker_RereadsOverlapWindow(t *testing.T) {
	src := &fakeGraphSource{}
	proj := &fakeProjector{state: model.GraphProjectionState{LastRelationID: 5000}}
	NewGraphProjectionWorker(GraphProjectionWorkerConfig{
		Source: src, Projector: proj, BatchSize: 10, Overlap: 1000,
	}).tick(context.Background())
	if len(src.relCalls) == 0 || src.relCalls[0] != 4000 {
		t.Fatalf("first ListRelationsAfter cursor = %v, want 4000", src.relCalls)
	}
}

// TestGraphProjectionWorker_OverlapNeverGoesNegative guards the early-life
// case where the watermark is smaller than the overlap window.
func TestGraphProjectionWorker_OverlapNeverGoesNegative(t *testing.T) {
	src := &fakeGraphSource{}
	proj := &fakeProjector{state: model.GraphProjectionState{LastRelationID: 3}}
	NewGraphProjectionWorker(GraphProjectionWorkerConfig{
		Source: src, Projector: proj, BatchSize: 10, Overlap: 1000,
	}).tick(context.Background())
	if len(src.relCalls) == 0 || src.relCalls[0] != 0 {
		t.Fatalf("first ListRelationsAfter cursor = %v, want 0", src.relCalls)
	}
}

// TestGraphProjectionWorker_MentionSweepStartsFromScratch pins the full-sweep
// decision: document_entities has no monotonic key, so a saved cursor would
// miss rows inserted "below" it (document ids are random UUIDs).
func TestGraphProjectionWorker_MentionSweepStartsFromScratch(t *testing.T) {
	src := &fakeGraphSource{mentions: []model.ProjectionMention{
		{DocumentID: "00000000-0000-0000-0000-00000000000a", EntityID: 1},
		{DocumentID: "00000000-0000-0000-0000-00000000000b", EntityID: 2},
	}}
	proj := &fakeProjector{}
	w := NewGraphProjectionWorker(GraphProjectionWorkerConfig{
		Source: src, Projector: proj, BatchSize: 10, Overlap: 1000,
	})
	w.tick(context.Background())
	w.tick(context.Background())

	if proj.mentionRows != 4 {
		t.Fatalf("mention rows projected over 2 ticks = %d, want 4 (full sweep each tick)", proj.mentionRows)
	}
	for _, c := range src.mentCalls {
		if c == "" {
			return // at least one sweep started from an empty cursor
		}
	}
	t.Fatalf("no mention sweep started from an empty cursor: %v", src.mentCalls)
}
