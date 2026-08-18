package worker

import (
	"context"
	"errors"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/baekenough/second-brain/internal/action"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// fakeStructuralLister stands in for the SQL query. listErr exists because a
// double that can only succeed cannot tell a tick that found no work apart
// from a tick that never received any — the two failure modes this worker
// reports identically unless the read error is counted.
type fakeStructuralLister struct {
	messages []ThreadLatestMessage
	listErr  error
}

func (f *fakeStructuralLister) ListLatestPerThread(_ context.Context, _ int) ([]ThreadLatestMessage, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.messages, nil
}

// fakeStructuralActionWriter records what the worker asked the store to write.
// upsertErr/statusErr exist so the failure paths are exercisable: a double
// that can only succeed proves nothing about a worker whose whole job is to
// keep writing on the next tick after a failed one. ctxErr captures whether
// the write was attempted with an already-cancelled context.
type fakeStructuralActionWriter struct {
	upserted    []model.Action
	upsertErr   error
	statusErr   error
	statusCalls int
	ctxErr      error
}

func (f *fakeStructuralActionWriter) UpsertAction(ctx context.Context, a model.Action) error {
	if err := ctx.Err(); err != nil && f.ctxErr == nil {
		f.ctxErr = err
	}
	f.upserted = append(f.upserted, a)
	return f.upsertErr
}

func (f *fakeStructuralActionWriter) EnsureOpenStatus(ctx context.Context, _ string) error {
	if err := ctx.Err(); err != nil && f.ctxErr == nil {
		f.ctxErr = err
	}
	f.statusCalls++
	return f.statusErr
}

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

// ---------------------------------------------------------------------------
// Card content: the summary and the counterpart label.
//
// Before this change every awaiting_my_reply action carried the fixed string
// below, so the /actions screen rendered 50 identical cards and the
// counterpart column was empty for every one of them (the worker never set
// CounterpartName, so counterpart_entity_id was NULL by construction).
// ---------------------------------------------------------------------------

// legacyStructuralSummary is the constant the worker used to write.
const legacyStructuralSummary = "마지막 메시지 이후 응답 없음"

// fixedNow pins the worker's clock so summaries are deterministic. 12:00 KST.
var fixedNow = time.Date(2026, 8, 18, 3, 0, 0, 0, time.UTC)

// TestStructuralSignalWorker_SummariesAreThreadSpecific is the headline
// regression test: three different conversations must yield three different
// cards, none of them the old constant.
func TestStructuralSignalWorker_SummariesAreThreadSpecific(t *testing.T) {
	t.Parallel()
	lister := &fakeStructuralLister{messages: []ThreadLatestMessage{
		{
			DocumentID: uuid.New(), SourceType: string(model.SourceSMS),
			Metadata: map[string]any{"contact_name": "테스트연락처A", "direction": "received"},
			Title:    "SMS received A", EventAt: fixedNow.AddDate(0, 0, -2),
		},
		{
			DocumentID: uuid.New(), SourceType: string(model.SourceGmail),
			Metadata: map[string]any{"thread_id": "t1", "from": `"테스트발신자B" <b@example.test>`},
			Title:    "8월 정산 자료 요청", EventAt: fixedNow.AddDate(0, 0, -5),
		},
		{
			DocumentID: uuid.New(), SourceType: string(model.SourceCallLog),
			Metadata: map[string]any{"contact_name": "테스트연락처C", "direction": "missed"},
			Title:    "missed 통화 C", EventAt: fixedNow,
		},
	}}
	actions := &fakeStructuralActionWriter{}
	w := NewStructuralSignalWorker(StructuralSignalWorkerConfig{
		Store: lister, Actions: actions, Now: func() time.Time { return fixedNow },
	})
	w.Tick(context.Background())

	if len(actions.upserted) != 3 {
		t.Fatalf("got %d actions, want 3", len(actions.upserted))
	}
	seen := make(map[string]int, 3)
	for _, a := range actions.upserted {
		if a.Summary == legacyStructuralSummary {
			t.Errorf("action %s still carries the fixed constant summary", a.ThreadKey)
		}
		if n := utf8.RuneCountInString(a.Summary); n > 80 || n == 0 {
			t.Errorf("summary %q is %d runes, want 1..80", a.Summary, n)
		}
		seen[a.Summary]++
	}
	for text, n := range seen {
		if n > 1 {
			t.Errorf("summary %q was written for %d different threads", text, n)
		}
	}
}

// TestStructuralSignalWorker_PopulatesCounterpartNameForResolution pins the
// counterpart_entity_id fix at its origin. The worker itself does not touch
// the entities table — it hands the store a human-readable label plus the
// entity type to file it under, and ActionStore.UpsertAction resolves it.
// Before this change the field did not exist and every structural action was
// written with a NULL counterpart.
func TestStructuralSignalWorker_PopulatesCounterpartNameForResolution(t *testing.T) {
	t.Parallel()
	lister := &fakeStructuralLister{messages: []ThreadLatestMessage{
		{
			DocumentID: uuid.New(), SourceType: string(model.SourceSMS),
			Metadata: map[string]any{"contact_name": "테스트연락처A", "direction": "received"},
			EventAt:  fixedNow,
		},
		{
			DocumentID: uuid.New(), SourceType: string(model.SourceGmail),
			Metadata: map[string]any{"thread_id": "t1", "from": `"테스트발신자B" <b@example.test>`},
			EventAt:  fixedNow,
		},
	}}
	actions := &fakeStructuralActionWriter{}
	w := NewStructuralSignalWorker(StructuralSignalWorkerConfig{
		Store: lister, Actions: actions, Now: func() time.Time { return fixedNow },
	})
	w.Tick(context.Background())

	if len(actions.upserted) != 2 {
		t.Fatalf("got %d actions, want 2", len(actions.upserted))
	}
	if got := actions.upserted[0]; got.CounterpartName != "테스트연락처A" || got.CounterpartType != model.EntityTypePerson {
		t.Errorf("sms action counterpart = (%q, %q), want (%q, %q)", got.CounterpartName, got.CounterpartType, "테스트연락처A", model.EntityTypePerson)
	}
	if got := actions.upserted[1]; got.CounterpartName != "테스트발신자B" || got.CounterpartType != model.EntityTypePerson {
		t.Errorf("gmail action counterpart = (%q, %q), want (%q, %q)", got.CounterpartName, got.CounterpartType, "테스트발신자B", model.EntityTypePerson)
	}
	// The identity string (what identity_key hashes) is unchanged: still the
	// raw From header, not the display name.
	wantKey := action.BuildIdentityKey("gmail:t1", string(model.KindAwaitingMyReply), `"테스트발신자B" <b@example.test>`, "")
	if actions.upserted[1].IdentityKey != wantKey {
		t.Errorf("gmail IdentityKey = %q, want %q (counterpart display name must not enter the key)", actions.upserted[1].IdentityKey, wantKey)
	}
}

// TestStructuralSignalWorker_UnnamedCounterpart_LeavesNameEmpty keeps junk out
// of the entities table: a thread whose only handle is a hashed number has no
// label worth registering, and filing every such thread under one shared row
// would merge unrelated callers.
func TestStructuralSignalWorker_UnnamedCounterpart_LeavesNameEmpty(t *testing.T) {
	t.Parallel()
	lister := &fakeStructuralLister{messages: []ThreadLatestMessage{
		{
			DocumentID: uuid.New(), SourceType: string(model.SourceSMS),
			Metadata: map[string]any{"direction": "received"}, EventAt: fixedNow,
		},
	}}
	actions := &fakeStructuralActionWriter{}
	w := NewStructuralSignalWorker(StructuralSignalWorkerConfig{
		Store: lister, Actions: actions, Now: func() time.Time { return fixedNow },
	})
	w.Tick(context.Background())

	if len(actions.upserted) != 1 {
		t.Fatalf("got %d actions, want 1", len(actions.upserted))
	}
	if name := actions.upserted[0].CounterpartName; name != "" {
		t.Errorf("CounterpartName = %q, want empty (no label exists for this thread)", name)
	}
	if actions.upserted[0].Summary == "" {
		t.Error("summary is empty; an unnamed thread still needs a readable card")
	}
}

// TestStructuralSignalWorker_IdentityKeyStableAsThreadAges runs two ticks a
// day apart. The summary must change (it reports the age) while identity_key
// must not — that is what lets a user's done/ignored decision survive.
func TestStructuralSignalWorker_IdentityKeyStableAsThreadAges(t *testing.T) {
	t.Parallel()
	docID := uuid.New()
	msg := ThreadLatestMessage{
		DocumentID: docID, SourceType: string(model.SourceSMS),
		Metadata: map[string]any{"contact_name": "테스트연락처A", "direction": "received"},
		EventAt:  fixedNow.AddDate(0, 0, -1),
	}
	lister := &fakeStructuralLister{messages: []ThreadLatestMessage{msg}}
	actions := &fakeStructuralActionWriter{}

	day1 := NewStructuralSignalWorker(StructuralSignalWorkerConfig{
		Store: lister, Actions: actions, Now: func() time.Time { return fixedNow },
	})
	day1.Tick(context.Background())

	day2 := NewStructuralSignalWorker(StructuralSignalWorkerConfig{
		Store: lister, Actions: actions, Now: func() time.Time { return fixedNow.AddDate(0, 0, 1) },
	})
	day2.Tick(context.Background())

	if len(actions.upserted) != 2 {
		t.Fatalf("got %d actions, want 2", len(actions.upserted))
	}
	first, second := actions.upserted[0], actions.upserted[1]
	if first.Summary == second.Summary {
		t.Errorf("summary %q did not change as the thread aged", first.Summary)
	}
	if first.IdentityKey != second.IdentityKey {
		t.Errorf("identity_key changed with the summary: %q -> %q", first.IdentityKey, second.IdentityKey)
	}
	if want := action.BuildIdentityKey("sms:테스트연락처A", string(model.KindAwaitingMyReply), "테스트연락처A", ""); first.IdentityKey != want {
		t.Errorf("identity_key = %q, want %q (empty-summary form)", first.IdentityKey, want)
	}
}

// TestStructuralSignalWorker_UpsertFailure_SkipsStatusWrite exercises the
// failure path the happy-path fake hides: action_status has an FK to
// actions.identity_key, so writing a status after a failed action insert can
// only raise 23503. The fake records ctx.Err() as well, so a cancelled tick
// cannot silently look like a successful one.
func TestStructuralSignalWorker_UpsertFailure_SkipsStatusWrite(t *testing.T) {
	t.Parallel()
	lister := &fakeStructuralLister{messages: []ThreadLatestMessage{
		{
			DocumentID: uuid.New(), SourceType: string(model.SourceSMS),
			Metadata: map[string]any{"contact_name": "테스트연락처A", "direction": "received"},
			EventAt:  fixedNow,
		},
	}}
	actions := &fakeStructuralActionWriter{upsertErr: errors.New("injected write failure")}
	w := NewStructuralSignalWorker(StructuralSignalWorkerConfig{
		Store: lister, Actions: actions, Now: func() time.Time { return fixedNow },
	})
	w.Tick(context.Background())

	if actions.statusCalls != 0 {
		t.Errorf("EnsureOpenStatus called %d times after a failed UpsertAction, want 0", actions.statusCalls)
	}
}

// prodGmailMetadata mirrors the EXACT key set a production gmail document
// carries, as observed on 2026-08-18 (jsonb_object_keys over every active
// gmail document inside the worker's 30-day window: from, to, thread_id,
// label_ids — 453/453 for each). It is spelled out here rather than reduced
// to the two keys the worker reads, because the incident this test guards
// against was a REPORTED regression in which every gmail thread was believed
// to be silently skipped; the accusation could not be settled from the code
// alone, only by comparing the shape the worker expects against the shape
// production actually stores. Pinning the real shape makes that comparison a
// test rather than a database query.
//
// Two absences carry as much weight as the four keys present:
//   - no "direction" key. isOutbound must therefore fall through its default
//     branch for gmail; a future case that reads doc.Metadata["direction"]
//     for gmail would skip every real thread while every SMS test still
//     passed.
//   - the caller passes NO UserAddresses. USER_EMAIL_ADDRESSES is unset on
//     the production host, so IsUserAddress is always false and the
//     "user sent this themself" gate must not fire.
func prodGmailMetadata(threadID string) map[string]any {
	return map[string]any{
		"thread_id": threadID,
		"from":      "테스트발신자 <sender@example.test>",
		"to":        "owner@example.test",
		"label_ids": []any{"INBOX", "CATEGORY_PERSONAL"},
	}
}

// TestStructuralSignalWorker_ProductionGmailShape_ConvertsEveryThread locks
// the invariant the 2026-08-18 investigation ultimately verified in
// production: for gmail, listed threads and written actions are the SAME
// number. Every one of the four early returns in processMessage is silent, so
// a regression in any of them turns this worker into a no-op that logs exactly
// what a healthy worker logs. This test is the only thing standing between
// that regression and a deploy.
func TestStructuralSignalWorker_ProductionGmailShape_ConvertsEveryThread(t *testing.T) {
	t.Parallel()
	lister := &fakeStructuralLister{messages: []ThreadLatestMessage{
		{DocumentID: uuid.New(), SourceType: string(model.SourceGmail), Metadata: prodGmailMetadata("t1"), Title: "제목 하나", EventAt: fixedNow},
		{DocumentID: uuid.New(), SourceType: string(model.SourceGmail), Metadata: prodGmailMetadata("t2"), Title: "제목 둘", EventAt: fixedNow},
		{DocumentID: uuid.New(), SourceType: string(model.SourceGmail), Metadata: prodGmailMetadata("t3"), Title: "제목 셋", EventAt: fixedNow},
	}}
	actions := &fakeStructuralActionWriter{}
	w := NewStructuralSignalWorker(StructuralSignalWorkerConfig{
		Store: lister, Actions: actions, Now: func() time.Time { return fixedNow },
	})

	stats := w.Tick(context.Background())

	if stats.Listed != 3 || stats.Written != 3 {
		t.Fatalf("listed=%d written=%d, want 3/3 — a production-shaped gmail thread must always yield an action", stats.Listed, stats.Written)
	}
	if len(actions.upserted) != 3 {
		t.Fatalf("got %d upserts, want 3", len(actions.upserted))
	}
	if actions.statusCalls != 3 {
		t.Errorf("EnsureOpenStatus called %d times, want 3 — an action with no open status is invisible on /actions", actions.statusCalls)
	}
	if skipped := stats.SkippedAuthLike + stats.SkippedOutbound + stats.SkippedNoCounterpart + stats.SkippedNoThreadKey; skipped != 0 {
		t.Errorf("%d gmail threads were skipped, want 0 (auth_like=%d outbound=%d no_counterpart=%d no_thread_key=%d)",
			skipped, stats.SkippedAuthLike, stats.SkippedOutbound, stats.SkippedNoCounterpart, stats.SkippedNoThreadKey)
	}
}

// TestStructuralSignalWorker_TickAccountsForEveryMessage is the observability
// guard. A tick that writes 419 actions and a tick that silently skips 419
// threads used to produce byte-identical output: nothing. That is what made a
// non-existent regression cost hours to disprove and what made a 447-row
// production DELETE look like a reasonable diagnostic step.
//
// The contract is arithmetic, not textual: every listed message lands in
// exactly one outcome bucket. A future early return added without a bucket
// breaks this test rather than quietly re-opening the blind spot.
func TestStructuralSignalWorker_TickAccountsForEveryMessage(t *testing.T) {
	t.Parallel()
	lister := &fakeStructuralLister{messages: []ThreadLatestMessage{
		// writes
		{DocumentID: uuid.New(), SourceType: string(model.SourceGmail), Metadata: prodGmailMetadata("t1"), Title: "제목", EventAt: fixedNow},
		// skipped: is_auth_like
		{DocumentID: uuid.New(), SourceType: string(model.SourceSMS), EventAt: fixedNow, Metadata: map[string]any{
			"contact_name": "테스트연락처A", "direction": "received", "is_auth_like": true,
		}},
		// skipped: outbound
		{DocumentID: uuid.New(), SourceType: string(model.SourceSMS), EventAt: fixedNow, Metadata: map[string]any{
			"contact_name": "테스트연락처A", "direction": "sent",
		}},
		// skipped: counterpart is the user themself
		{DocumentID: uuid.New(), SourceType: string(model.SourceGmail), EventAt: fixedNow, Metadata: map[string]any{
			"thread_id": "t2", "from": "owner@example.test",
		}},
		// skipped: no thread key (gmail without thread_id)
		{DocumentID: uuid.New(), SourceType: string(model.SourceGmail), EventAt: fixedNow, Metadata: map[string]any{
			"from": "테스트발신자 <sender@example.test>",
		}},
	}}
	actions := &fakeStructuralActionWriter{}
	w := NewStructuralSignalWorker(StructuralSignalWorkerConfig{
		Store: lister, Actions: actions, UserAddresses: []string{"owner@example.test"},
		Now: func() time.Time { return fixedNow },
	})

	stats := w.Tick(context.Background())

	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"Listed", stats.Listed, 5},
		{"Written", stats.Written, 1},
		{"SkippedAuthLike", stats.SkippedAuthLike, 1},
		{"SkippedOutbound", stats.SkippedOutbound, 1},
		{"SkippedNoCounterpart", stats.SkippedNoCounterpart, 1},
		{"SkippedNoThreadKey", stats.SkippedNoThreadKey, 1},
		{"Failed", stats.Failed, 0},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	if sum := stats.accountedFor(); sum != stats.Listed {
		t.Errorf("outcomes account for %d of %d listed messages — an early return has no bucket", sum, stats.Listed)
	}
}

// TestStructuralSignalWorker_TickCountsWriteFailures pins the two failure
// buckets apart. They are different operational problems: a failed
// UpsertAction leaves nothing behind, whereas a failed EnsureOpenStatus
// leaves a row in actions that /actions will never render because it joins
// action_status — a table full of rows and an empty screen, which is exactly
// the shape of report this worker's silence made impossible to triage.
func TestStructuralSignalWorker_TickCountsWriteFailures(t *testing.T) {
	t.Parallel()
	msgs := []ThreadLatestMessage{
		{DocumentID: uuid.New(), SourceType: string(model.SourceGmail), Metadata: prodGmailMetadata("t1"), Title: "제목", EventAt: fixedNow},
	}

	t.Run("upsert failure", func(t *testing.T) {
		t.Parallel()
		actions := &fakeStructuralActionWriter{upsertErr: errors.New("injected write failure")}
		w := NewStructuralSignalWorker(StructuralSignalWorkerConfig{
			Store: &fakeStructuralLister{messages: msgs}, Actions: actions, Now: func() time.Time { return fixedNow },
		})
		stats := w.Tick(context.Background())
		if stats.Failed != 1 || stats.Written != 0 {
			t.Errorf("failed=%d written=%d, want 1/0", stats.Failed, stats.Written)
		}
		if stats.accountedFor() != stats.Listed {
			t.Errorf("outcomes account for %d of %d listed messages", stats.accountedFor(), stats.Listed)
		}
	})

	t.Run("status failure", func(t *testing.T) {
		t.Parallel()
		actions := &fakeStructuralActionWriter{statusErr: errors.New("injected status failure")}
		w := NewStructuralSignalWorker(StructuralSignalWorkerConfig{
			Store: &fakeStructuralLister{messages: msgs}, Actions: actions, Now: func() time.Time { return fixedNow },
		})
		stats := w.Tick(context.Background())
		if stats.Written != 1 {
			t.Errorf("written=%d, want 1 — the action row itself was persisted", stats.Written)
		}
		if stats.StatusFailed != 1 {
			t.Errorf("statusFailed=%d, want 1 — an action with no open status is invisible on /actions", stats.StatusFailed)
		}
	})
}

// TestStructuralSignalWorker_ListFailureIsCounted keeps the all-or-nothing
// failure mode distinguishable from a tick that ran and found nothing to do.
// ListLatestPerThread returns an error for the WHOLE batch on a single bad
// row, so this bucket answers "did the worker do no work, or did it never get
// the work?" — two reports with identical symptoms.
func TestStructuralSignalWorker_ListFailureIsCounted(t *testing.T) {
	t.Parallel()
	actions := &fakeStructuralActionWriter{}
	w := NewStructuralSignalWorker(StructuralSignalWorkerConfig{
		Store:   &fakeStructuralLister{listErr: errors.New("injected list failure")},
		Actions: actions, Now: func() time.Time { return fixedNow },
	})

	stats := w.Tick(context.Background())

	if !stats.ListFailed {
		t.Error("ListFailed = false, want true")
	}
	if stats.Listed != 0 || len(actions.upserted) != 0 {
		t.Errorf("listed=%d upserts=%d, want 0/0", stats.Listed, len(actions.upserted))
	}
}
