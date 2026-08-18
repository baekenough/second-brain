package action_test

import (
	"testing"

	"github.com/baekenough/second-brain/internal/action"
	"github.com/baekenough/second-brain/internal/model"
)

// The counterpart tests below use obviously fictional tokens only
// (example.test is the RFC 2606 reserved TLD, and 010-0000-0000 is not an
// assignable Korean number) — no real contact of the user's ever appears in
// this repository.

// TestCounterpartDisplay_GmailEncodedDisplayName pins the gmail path: the
// From header is an RFC 5322 mailbox, and the part a human recognises is the
// display name, not the address. Without this, every gmail action card would
// be labelled with a raw address.
func TestCounterpartDisplay_GmailEncodedDisplayName(t *testing.T) {
	t.Parallel()
	doc := &model.Document{
		SourceType: model.SourceGmail,
		Metadata:   map[string]any{"from": `"테스트발신자" <dummy@example.test>`},
	}
	name, typ, ok := action.CounterpartDisplay(doc)
	if !ok {
		t.Fatal("ok = false, want true (a From header with a display name is resolvable)")
	}
	if name != "테스트발신자" {
		t.Errorf("name = %q, want %q (display name preferred over the address)", name, "테스트발신자")
	}
	if typ != model.EntityTypePerson {
		t.Errorf("type = %q, want %q", typ, model.EntityTypePerson)
	}
}

// TestCounterpartDisplay_GmailBareAddress covers the common case of a From
// header carrying no display name: the address itself is the only label
// available, and it is still far better than nothing on the card.
func TestCounterpartDisplay_GmailBareAddress(t *testing.T) {
	t.Parallel()
	doc := &model.Document{
		SourceType: model.SourceGmail,
		Metadata:   map[string]any{"from": "dummy@example.test"},
	}
	name, typ, ok := action.CounterpartDisplay(doc)
	if !ok || name != "dummy@example.test" || typ != model.EntityTypePerson {
		t.Errorf("got (%q, %q, %v), want (%q, %q, true)", name, typ, ok, "dummy@example.test", model.EntityTypePerson)
	}
}

// TestCounterpartDisplay_GmailEmptyFrom_NotResolvable guards the degenerate
// header: an empty From must not create an entity named "".
func TestCounterpartDisplay_GmailEmptyFrom_NotResolvable(t *testing.T) {
	t.Parallel()
	doc := &model.Document{SourceType: model.SourceGmail, Metadata: map[string]any{"from": "  "}}
	if name, _, ok := action.CounterpartDisplay(doc); ok {
		t.Errorf("ok = true (name %q), want false for a blank From header", name)
	}
}

// TestCounterpartDisplay_SMSContactName is the highest-volume path: the phone
// already resolved the number to a contact, so that name is what the user
// recognises and what must dedup against LLM-extracted PERSON entities.
func TestCounterpartDisplay_SMSContactName(t *testing.T) {
	t.Parallel()
	for _, src := range []model.SourceType{model.SourceSMS, model.SourceCallLog, model.SourceCallTranscript} {
		doc := &model.Document{
			SourceType: src,
			Metadata:   map[string]any{"contact_name": "테스트연락처", "number": "010-0000-0000"},
		}
		name, typ, ok := action.CounterpartDisplay(doc)
		if !ok || name != "테스트연락처" || typ != model.EntityTypePerson {
			t.Errorf("%s: got (%q, %q, %v), want (%q, %q, true)", src, name, typ, ok, "테스트연락처", model.EntityTypePerson)
		}
	}
}

// TestCounterpartDisplay_SMSNumberOnly_UsesNumberAsOther covers the unsaved
// contact: the number is the only handle the user has, so it becomes the
// entity label — but typed OTHER, not PERSON, so a bare number can never be
// silently merged with a real person's name in the entity graph.
//
// Note the policy coupling: metadata carries "number" ONLY when phone-number
// hashing is disabled (issue #164). With hashing on, this branch never fires,
// so no raw number reaches the entities table.
func TestCounterpartDisplay_SMSNumberOnly_UsesNumberAsOther(t *testing.T) {
	t.Parallel()
	const number = "010-0000-0000"
	doc := &model.Document{
		SourceType: model.SourceSMS,
		Metadata:   map[string]any{"contact_name": "", "number": number},
	}
	name, typ, ok := action.CounterpartDisplay(doc)
	if !ok {
		t.Fatal("ok = false, want true (an unsaved number is still a usable label)")
	}
	if name != number {
		t.Errorf("name = %q, want %q", name, number)
	}
	if name == action.HashPhoneNumber(number) {
		t.Error("name is the thread-key hash; a hash is not a label a human can act on")
	}
	if typ != model.EntityTypeOther {
		t.Errorf("type = %q, want %q (a bare number is not a PERSON entity)", typ, model.EntityTypeOther)
	}
}

// TestCounterpartDisplay_NoIdentifiers_NotResolvable pins the "unknown"
// counterpart: CounterpartIdentity returns the literal "unknown" so the
// identity_key stays computable, but there is nothing to name an entity
// after — resolving one would create a shared junk row every unnamed thread
// points at.
func TestCounterpartDisplay_NoIdentifiers_NotResolvable(t *testing.T) {
	t.Parallel()
	doc := &model.Document{SourceType: model.SourceSMS, Metadata: map[string]any{"direction": "received"}}
	if name, _, ok := action.CounterpartDisplay(doc); ok {
		t.Errorf("ok = true (name %q), want false when neither contact_name nor number exists", name)
	}
}

// TestCounterpartDisplay_UnsupportedSource_NotResolvable mirrors
// CounterpartIdentity's contract for source types outside the four
// conversation sources.
func TestCounterpartDisplay_UnsupportedSource_NotResolvable(t *testing.T) {
	t.Parallel()
	doc := &model.Document{SourceType: model.SourceFilesystem, Metadata: map[string]any{"contact_name": "테스트연락처"}}
	if _, _, ok := action.CounterpartDisplay(doc); ok {
		t.Error("ok = true, want false for a non-conversation source type")
	}
}

// TestCounterpartDisplay_NilMetadata_DoesNotPanic — the worker builds the
// document from a JSONB column that can legitimately be NULL.
func TestCounterpartDisplay_NilMetadata_DoesNotPanic(t *testing.T) {
	t.Parallel()
	if _, _, ok := action.CounterpartDisplay(&model.Document{SourceType: model.SourceSMS}); ok {
		t.Error("ok = true, want false for a document with no metadata")
	}
}
