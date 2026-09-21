package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// fakeStructuralLister stands in for the SQL query. It is still exercised by
// TestStructuralSignalWorker_TickNeverCallsStore below, which asserts the
// opposite of what earlier versions of this fake existed to prove: Tick must
// NOT call ListLatestPerThread at all now that awaiting_my_reply generation
// is retired (see structural_signals.go's package-level doc comment).
type fakeStructuralLister struct {
	messages []ThreadLatestMessage
	listErr  error
	calls    int
}

func (f *fakeStructuralLister) ListLatestPerThread(_ context.Context, _ int) ([]ThreadLatestMessage, error) {
	f.calls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.messages, nil
}

// fakeStructuralActionWriter records what the worker asked the store to
// write. It exists so a regression that resurrects the old write path is
// caught by a straightforward count assertion, not by inspecting SQL.
type fakeStructuralActionWriter struct {
	upserted    []model.Action
	statusCalls int
}

func (f *fakeStructuralActionWriter) UpsertAction(_ context.Context, a model.Action) error {
	f.upserted = append(f.upserted, a)
	return nil
}

func (f *fakeStructuralActionWriter) EnsureOpenStatus(_ context.Context, _ string) error {
	f.statusCalls++
	return nil
}

// TestStructuralSignalWorker_TickCreatesNoActions is the regression test for
// the 2026-09-21 retirement decision: 메일·SMS "미회신"으로 자동 생성되던
// awaiting_my_reply는 할일이 아니라 잡음이므로 더 이상 만들지 않는다. Even a
// thread shape that would have been the clearest positive case under the old
// logic (inbound SMS, no prior reply) must produce nothing.
func TestStructuralSignalWorker_TickCreatesNoActions(t *testing.T) {
	t.Parallel()
	lister := &fakeStructuralLister{messages: []ThreadLatestMessage{
		{DocumentID: uuid.New(), SourceType: string(model.SourceSMS), Metadata: map[string]any{
			"contact_name": "철수", "direction": "received",
		}},
		{DocumentID: uuid.New(), SourceType: string(model.SourceGmail), Metadata: map[string]any{
			"thread_id": "t1", "from": "alice@example.com",
		}},
	}}
	actions := &fakeStructuralActionWriter{}
	w := NewStructuralSignalWorker(StructuralSignalWorkerConfig{Store: lister, Actions: actions})

	stats := w.Tick(context.Background())

	if stats.Listed != 0 || stats.Written != 0 {
		t.Errorf("stats = %+v, want all zero", stats)
	}
	if len(actions.upserted) != 0 {
		t.Fatalf("got %d actions, want 0 (awaiting_my_reply generation is retired)", len(actions.upserted))
	}
	if actions.statusCalls != 0 {
		t.Errorf("EnsureOpenStatus called %d times, want 0", actions.statusCalls)
	}
}

// TestStructuralSignalWorker_TickNeverCallsStore pins the efficiency half of
// the retirement: Tick must not spend a database round trip listing threads
// for a candidate it will never write.
func TestStructuralSignalWorker_TickNeverCallsStore(t *testing.T) {
	t.Parallel()
	lister := &fakeStructuralLister{listErr: errors.New("must not be called")}
	actions := &fakeStructuralActionWriter{}
	w := NewStructuralSignalWorker(StructuralSignalWorkerConfig{Store: lister, Actions: actions})

	if _, err := lister.ListLatestPerThread(context.Background(), 1); err == nil {
		t.Fatalf("test setup broken: fake must return an error when called")
	}
	lister.calls = 0 // the setup probe above should not count against the assertion

	w.Tick(context.Background())

	if lister.calls != 0 {
		t.Errorf("ListLatestPerThread called %d times, want 0", lister.calls)
	}
}
