package briefing

import (
	"testing"
	"time"
)

func fixedTime() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) }

// twoActionInput is the shared fixture. Every value is a synthetic dummy: no
// real name, number, or message text appears anywhere in this package's tests.
func twoActionInput() Input {
	return Input{
		Model:            "dummy-model",
		LatestDocumentAt: fixedTime(),
		Actions: []InputAction{
			{
				IdentityKey:     "action:aaaaaaaaaaaaaaaa",
				DocumentID:      "11111111-1111-1111-1111-111111111111",
				Kind:            "my_commitment",
				Summary:         "dummy summary one",
				CounterpartName: "zz-dummy-counterpart",
				DetectedBy:      "structural",
				Confidence:      1.0,
			},
			{
				IdentityKey:     "action:bbbbbbbbbbbbbbbb",
				DocumentID:      "22222222-2222-2222-2222-222222222222",
				Kind:            "scheduled",
				Summary:         "dummy summary two",
				CounterpartName: "zz-dummy-counterpart",
				DetectedBy:      "llm",
				Confidence:      0.6,
			},
		},
	}
}

// TestCacheKeyIgnoresRowOrder pins that the key is computed over a SORTED set of
// identity keys. Row order out of PostgreSQL is not guaranteed to be stable
// across plans; if it leaked into the key, the cache would miss on nearly every
// request and each miss costs an LLM call.
func TestCacheKeyIgnoresRowOrder(t *testing.T) {
	a := twoActionInput()
	b := twoActionInput()
	b.Actions[0], b.Actions[1] = b.Actions[1], b.Actions[0]

	if a.CacheKey() != b.CacheKey() {
		t.Fatal("cache key depends on row order; sort identity_keys before hashing")
	}
}

// TestCacheKeyIgnoresSummaryText pins that summary text is NOT part of the key.
// The same identity_key is by definition the same real-world action, so a
// re-extraction that reworded the summary is not a reason to pay for a new
// briefing. The side effect matters too: with summaries excluded, no personal
// data feeds the hash, which is what makes the key safe to log.
func TestCacheKeyIgnoresSummaryText(t *testing.T) {
	a := twoActionInput()
	b := twoActionInput()
	b.Actions[0].Summary = "an entirely different dummy wording"
	b.Actions[1].CounterpartName = "zz-other-dummy"

	if a.CacheKey() != b.CacheKey() {
		t.Fatal("cache key changed when only summary/counterpart text changed; it must depend on identity keys alone")
	}
}

// TestCacheKeyChangesOnRealInputChange pins every documented invalidation
// trigger. Each of these events must produce a different key, otherwise a stale
// briefing is served.
func TestCacheKeyChangesOnRealInputChange(t *testing.T) {
	base := twoActionInput()

	t.Run("new open action", func(t *testing.T) {
		got := twoActionInput()
		got.Actions = append(got.Actions, InputAction{
			IdentityKey: "action:cccccccccccccccc",
			DocumentID:  "33333333-3333-3333-3333-333333333333",
			Kind:        "their_commitment",
		})
		if got.CacheKey() == base.CacheKey() {
			t.Fatal("key unchanged after a new action entered the open set")
		}
	})

	t.Run("action resolved by the user", func(t *testing.T) {
		got := twoActionInput()
		got.Actions = got.Actions[:1]
		if got.CacheKey() == base.CacheKey() {
			t.Fatal("key unchanged after an action left the open set")
		}
	})

	t.Run("latest document time advanced", func(t *testing.T) {
		got := twoActionInput()
		got.LatestDocumentAt = fixedTime().Add(time.Hour)
		if got.CacheKey() == base.CacheKey() {
			t.Fatal("key unchanged after LatestDocumentAt advanced")
		}
	})

	t.Run("model swapped", func(t *testing.T) {
		got := twoActionInput()
		got.Model = "other-dummy-model"
		if got.CacheKey() == base.CacheKey() {
			t.Fatal("key unchanged after the model changed; a redeploy on a new model would serve the old model's sentences")
		}
	})
}

// TestCacheKeyIncludesPromptVersion pins that the prompt version participates in
// the key. Without it, editing the prompt and redeploying keeps serving
// sentences produced by the previous prompt until the open set happens to
// change.
func TestCacheKeyIncludesPromptVersion(t *testing.T) {
	in := twoActionInput()
	got := in.CacheKey()

	// Recompute with a different prompt version by going through the same
	// helper the exported method uses.
	other := cacheKeyWithVersion(in, PromptVersion+"-modified")
	if got == other {
		t.Fatal("cache key does not depend on PromptVersion; a prompt change would not invalidate cached briefings")
	}
}

// TestCacheKeyIsTimezoneStable pins that the timestamp is normalised to UTC
// before hashing, so the same instant read back in a different location does not
// produce a different key.
func TestCacheKeyIsTimezoneStable(t *testing.T) {
	a := twoActionInput()
	b := twoActionInput()
	b.LatestDocumentAt = a.LatestDocumentAt.In(time.FixedZone("KST", 9*60*60))

	if a.CacheKey() != b.CacheKey() {
		t.Fatal("cache key depends on the timestamp's location; normalise to UTC before hashing")
	}
}
