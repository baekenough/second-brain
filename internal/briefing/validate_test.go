package briefing

import (
	"strings"
	"testing"
)

// testInput is the whitelist fixture: two synthetic actions whose document ids
// are the ONLY ids a sentence may cite.
func testInput() Input { return twoActionInput() }

const (
	docA = "11111111-1111-1111-1111-111111111111"
	docB = "22222222-2222-2222-2222-222222222222"
	keyA = "action:aaaaaaaaaaaaaaaa"
	keyB = "action:bbbbbbbbbbbbbbbb"
)

// TestValidateDropsSentenceWithUnknownDocumentID is the central test of this
// feature. A document id that never appeared in the prompt cannot have been
// read by the model, so a sentence citing one is unfounded by construction and
// must be discarded rather than shown with a broken link.
func TestValidateDropsSentenceWithUnknownDocumentID(t *testing.T) {
	raw := `{"sentences":[
		{"text":"근거 있는 문장.","document_ids":["` + docA + `"],"identity_keys":["` + keyA + `"]},
		{"text":"환각 문장.","document_ids":["99999999-9999-9999-9999-999999999999"],"identity_keys":[]}
	]}`

	got, err := Validate(raw, testInput())
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(got.Sentences) != 1 {
		t.Fatalf("sentences = %+v, want only the grounded one", got.Sentences)
	}
	if got.Sentences[0].Text != "근거 있는 문장." {
		t.Fatalf("kept the wrong sentence: %q", got.Sentences[0].Text)
	}
	if got.DroppedCount != 1 {
		t.Fatalf("DroppedCount = %d, want 1", got.DroppedCount)
	}
	if got.Degraded {
		t.Error("Degraded = true although one valid sentence survived")
	}
}

// TestValidateDropsUngroundedSentences pins the remaining discard rules. Each
// case is a way a model can produce a sentence that cannot be traced back to a
// document the server actually showed it.
func TestValidateDropsUngroundedSentences(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"no document_ids field", `{"sentences":[{"text":"근거 없는 주장."}]}`},
		{"empty document_ids", `{"sentences":[{"text":"근거 없는 주장.","document_ids":[]}]}`},
		{"blank text", `{"sentences":[{"text":"   ","document_ids":["` + docA + `"]}]}`},
		{"paragraph instead of sentence", `{"sentences":[{"text":"` + strings.Repeat("가", 401) + `","document_ids":["` + docA + `"]}]}`},
		{"one bad id among good ones", `{"sentences":[{"text":"일부만 근거 있음.","document_ids":["` + docA + `","99999999-9999-9999-9999-999999999999"]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Validate(tc.raw, testInput())
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			for _, s := range got.Sentences {
				if !got.Degraded {
					t.Fatalf("ungrounded sentence survived: %+v", s)
				}
			}
			if !got.Degraded {
				t.Fatal("every sentence was discarded but Degraded = false; the UI would present an empty briefing as a real one")
			}
			if got.DroppedCount != 1 {
				t.Fatalf("DroppedCount = %d, want 1", got.DroppedCount)
			}
		})
	}
}

// TestValidateFallbackIsItselfGrounded pins that the degraded fallback obeys the
// same rule it protects: even a pure count sentence must carry evidence.
func TestValidateFallbackIsItselfGrounded(t *testing.T) {
	got, err := Validate(`{"sentences":[]}`, testInput())
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !got.Degraded {
		t.Fatal("Degraded = false for an empty sentence list")
	}
	if len(got.Sentences) != 1 {
		t.Fatalf("fallback produced %d sentences, want 1", len(got.Sentences))
	}
	if len(got.Sentences[0].DocumentIDs) == 0 {
		t.Fatal("fallback sentence has no DocumentIDs; the evidence rule has no exceptions")
	}
	for _, id := range got.Sentences[0].DocumentIDs {
		if id != docA && id != docB {
			t.Fatalf("fallback cited an id outside the input: %q", id)
		}
	}
	// The fallback is a count, not prose: it must not invent a claim.
	if !strings.Contains(got.Sentences[0].Text, "건") {
		t.Fatalf("fallback text = %q, want a count-only sentence", got.Sentences[0].Text)
	}
}

// TestValidateStripsUnknownIdentityKeys pins the one non-fatal violation: an
// invented identity_key is removed, but the sentence survives because its
// DOCUMENT evidence is what grounds it. Dropping the whole sentence here would
// throw away a valid statement over a broken cross-reference.
func TestValidateStripsUnknownIdentityKeys(t *testing.T) {
	raw := `{"sentences":[{"text":"근거 있는 문장.","document_ids":["` + docA + `"],"identity_keys":["` + keyA + `","action:ffffffffffffffff"]}]}`

	got, err := Validate(raw, testInput())
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(got.Sentences) != 1 {
		t.Fatalf("sentences = %+v, want the sentence kept", got.Sentences)
	}
	if len(got.Sentences[0].IdentityKeys) != 1 || got.Sentences[0].IdentityKeys[0] != keyA {
		t.Fatalf("IdentityKeys = %v, want only the known key", got.Sentences[0].IdentityKeys)
	}
	if got.DroppedCount != 0 {
		t.Fatalf("DroppedCount = %d, want 0 (no sentence was discarded)", got.DroppedCount)
	}
}

// TestValidateDecodesDecoratedJSON pins tolerance for the two output habits this
// project has already observed in production: fenced code blocks and a
// conversational preamble around the JSON.
func TestValidateDecodesDecoratedJSON(t *testing.T) {
	body := `{"sentences":[{"text":"근거 있는 문장.","document_ids":["` + docA + `"]}]}`
	cases := map[string]string{
		"fenced":            "```json\n" + body + "\n```",
		"preamble":          "다음은 요청하신 브리핑입니다:\n" + body,
		"preamble and tail": "설명:\n" + body + "\n이상입니다.",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Validate(raw, testInput())
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if got.Degraded || len(got.Sentences) != 1 {
				t.Fatalf("decorated JSON was not decoded: %+v", got)
			}
		})
	}
}

// TestValidateUndecodableFallsBack pins that garbage produces the deterministic
// fallback plus a reportable error, never a partially-parsed briefing.
func TestValidateUndecodableFallsBack(t *testing.T) {
	got, err := Validate("this is not json at all", testInput())
	if err == nil {
		t.Fatal("Validate returned nil error for undecodable output; the caller cannot record the failure")
	}
	if !got.Degraded || len(got.Sentences) != 1 {
		t.Fatalf("result = %+v, want the degraded fallback", got)
	}
}

// TestValidateNoActionsProducesNothing pins that an empty open set yields an
// empty, non-degraded result: there is nothing to summarise and nothing to warn
// about.
func TestValidateNoActionsProducesNothing(t *testing.T) {
	got, err := Validate(`{"sentences":[]}`, Input{Model: "dummy-model"})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(got.Sentences) != 0 {
		t.Fatalf("sentences = %+v, want none", got.Sentences)
	}
	if got.Degraded {
		t.Error("Degraded = true with no actions at all; there is no failure to report")
	}
}

// TestValidateCapsSentenceCount pins the response-size bound on the model's
// side of the contract.
func TestValidateCapsSentenceCount(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"sentences":[`)
	for i := 0; i < 20; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"text":"근거 있는 문장.","document_ids":["` + docA + `"]}`)
	}
	b.WriteString(`]}`)

	got, err := Validate(b.String(), testInput())
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(got.Sentences) > maxSentences {
		t.Fatalf("kept %d sentences, want at most %d", len(got.Sentences), maxSentences)
	}
}
