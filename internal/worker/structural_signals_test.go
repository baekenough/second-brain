package worker

import (
	"context"
	"testing"

	"github.com/baekenough/second-brain/internal/action"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

type fakeStructuralLister struct {
	messages []ThreadLatestMessage
}

func (f *fakeStructuralLister) ListLatestPerThread(_ context.Context, _ int) ([]ThreadLatestMessage, error) {
	return f.messages, nil
}

type fakeStructuralActionWriter struct {
	upserted []model.Action
}

func (f *fakeStructuralActionWriter) UpsertAction(_ context.Context, a model.Action) error {
	f.upserted = append(f.upserted, a)
	return nil
}
func (f *fakeStructuralActionWriter) EnsureOpenStatus(_ context.Context, _ string) error { return nil }

// TestStructuralSignalWorker_InboundSMS_CreatesAwaitingMyReply verifies the
// simplest positive case: an sms document with direction=received is the
// latest message in its thread, so it becomes an awaiting_my_reply
// candidate with detected_by=structural.
func TestStructuralSignalWorker_InboundSMS_CreatesAwaitingMyReply(t *testing.T) {
	t.Parallel()
	lister := &fakeStructuralLister{messages: []ThreadLatestMessage{
		{DocumentID: uuid.New(), SourceType: string(model.SourceSMS), Metadata: map[string]any{
			"contact_name": "철수", "direction": "received",
		}},
	}}
	actions := &fakeStructuralActionWriter{}
	w := NewStructuralSignalWorker(StructuralSignalWorkerConfig{Store: lister, Actions: actions})
	w.Tick(context.Background())

	if len(actions.upserted) != 1 {
		t.Fatalf("got %d actions, want 1", len(actions.upserted))
	}
	if actions.upserted[0].Kind != model.KindAwaitingMyReply || actions.upserted[0].DetectedBy != model.DetectedStructural {
		t.Errorf("got %+v", actions.upserted[0])
	}
	wantThreadKey := "sms:철수"
	if actions.upserted[0].ThreadKey != wantThreadKey {
		t.Errorf("ThreadKey = %q, want %q", actions.upserted[0].ThreadKey, wantThreadKey)
	}
}

// TestStructuralSignalWorker_OutboundSMS_NoAction verifies the latest
// message being OUTBOUND (direction=sent) produces no candidate — the user
// already replied.
func TestStructuralSignalWorker_OutboundSMS_NoAction(t *testing.T) {
	t.Parallel()
	lister := &fakeStructuralLister{messages: []ThreadLatestMessage{
		{DocumentID: uuid.New(), SourceType: string(model.SourceSMS), Metadata: map[string]any{
			"contact_name": "철수", "direction": "sent",
		}},
	}}
	actions := &fakeStructuralActionWriter{}
	w := NewStructuralSignalWorker(StructuralSignalWorkerConfig{Store: lister, Actions: actions})
	w.Tick(context.Background())

	if len(actions.upserted) != 0 {
		t.Fatalf("got %d actions, want 0", len(actions.upserted))
	}
}

// TestStructuralSignalWorker_GmailFromUserAddress_NoAction verifies the
// gmail direction-inference path: "from" matching a configured user address
// means the user sent the latest message, so no reply is pending.
func TestStructuralSignalWorker_GmailFromUserAddress_NoAction(t *testing.T) {
	t.Parallel()
	lister := &fakeStructuralLister{messages: []ThreadLatestMessage{
		{DocumentID: uuid.New(), SourceType: string(model.SourceGmail), Metadata: map[string]any{
			"thread_id": "t1", "from": "me@example.com",
		}},
	}}
	actions := &fakeStructuralActionWriter{}
	w := NewStructuralSignalWorker(StructuralSignalWorkerConfig{
		Store: lister, Actions: actions, UserAddresses: []string{"me@example.com"},
	})
	w.Tick(context.Background())

	if len(actions.upserted) != 0 {
		t.Fatalf("got %d actions, want 0 (from == user's own address)", len(actions.upserted))
	}
}

// TestStructuralSignalWorker_GmailFromOther_CreatesAwaitingMyReply is the
// positive-path counterpart to the above: "from" NOT matching any
// configured user address means the message is inbound, so a candidate is
// produced. Also pins that identity_key convergence depends on
// CounterpartIdentity being fed the same "from" string used here.
func TestStructuralSignalWorker_GmailFromOther_CreatesAwaitingMyReply(t *testing.T) {
	t.Parallel()
	lister := &fakeStructuralLister{messages: []ThreadLatestMessage{
		{DocumentID: uuid.New(), SourceType: string(model.SourceGmail), Metadata: map[string]any{
			"thread_id": "t1", "from": "alice@example.com",
		}},
	}}
	actions := &fakeStructuralActionWriter{}
	w := NewStructuralSignalWorker(StructuralSignalWorkerConfig{
		Store: lister, Actions: actions, UserAddresses: []string{"me@example.com"},
	})
	w.Tick(context.Background())

	if len(actions.upserted) != 1 {
		t.Fatalf("got %d actions, want 1", len(actions.upserted))
	}
	wantKey := action.BuildIdentityKey("gmail:t1", string(model.KindAwaitingMyReply), "alice@example.com", "")
	if actions.upserted[0].IdentityKey != wantKey {
		t.Errorf("IdentityKey = %q, want %q (must converge with extraction worker's LLM candidate)", actions.upserted[0].IdentityKey, wantKey)
	}
}

// TestStructuralSignalWorker_AuthLikeSMS_NoAction verifies the sms
// is_auth_like flag suppresses a candidate even when direction=received.
// The SQL query (ListLatestPerThread) also filters these out at the
// source (spec §7.4), but the worker checks defensively here too — the
// two are independently testable and this Go-level check protects against
// any future caller of StructuralSignalLister that does not pre-filter.
func TestStructuralSignalWorker_AuthLikeSMS_NoAction(t *testing.T) {
	t.Parallel()
	lister := &fakeStructuralLister{messages: []ThreadLatestMessage{
		{DocumentID: uuid.New(), SourceType: string(model.SourceSMS), Metadata: map[string]any{
			"contact_name": "광고", "direction": "received", "is_auth_like": true,
		}},
	}}
	actions := &fakeStructuralActionWriter{}
	w := NewStructuralSignalWorker(StructuralSignalWorkerConfig{Store: lister, Actions: actions})
	w.Tick(context.Background())

	if len(actions.upserted) != 0 {
		t.Fatalf("got %d actions, want 0 (is_auth_like=true must be excluded)", len(actions.upserted))
	}
}

// TestStructuralSignalWorker_SMSNoContactName_FallsBackToNumberHash
// verifies the spec §7.1 fallback: when contact_name is null, the thread
// key and counterpart identity are both derived from a hash of the phone
// number instead (never the raw number, and never "unknown" silently
// merging distinct callers).
func TestStructuralSignalWorker_SMSNoContactName_FallsBackToNumberHash(t *testing.T) {
	t.Parallel()
	const number = "010-1234-5678"
	lister := &fakeStructuralLister{messages: []ThreadLatestMessage{
		{DocumentID: uuid.New(), SourceType: string(model.SourceSMS), Metadata: map[string]any{
			"number": number, "direction": "received",
		}},
	}}
	actions := &fakeStructuralActionWriter{}
	w := NewStructuralSignalWorker(StructuralSignalWorkerConfig{Store: lister, Actions: actions})
	w.Tick(context.Background())

	if len(actions.upserted) != 1 {
		t.Fatalf("got %d actions, want 1", len(actions.upserted))
	}
	wantThreadKey := "sms:" + action.HashPhoneNumber(number)
	if actions.upserted[0].ThreadKey != wantThreadKey {
		t.Errorf("ThreadKey = %q, want %q (hashed number fallback)", actions.upserted[0].ThreadKey, wantThreadKey)
	}
	if actions.upserted[0].ThreadKey == "sms:"+number {
		t.Error("ThreadKey used the raw phone number instead of its hash")
	}
}
