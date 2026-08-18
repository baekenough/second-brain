package briefing

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"
)

// PromptVersion identifies the prompt this package sends to the model. It is
// part of the cache key, so bumping it after editing prompt.go invalidates every
// cached briefing. Without that, a prompt fix would keep serving sentences
// produced by the previous prompt until the open action set happened to change.
const PromptVersion = "briefing-v1"

// InputAction is one open action as handed to the briefing generator. It is a
// flat, store-free struct on purpose: this package does no database or HTTP
// work, so it can be exercised entirely with table-driven tests.
type InputAction struct {
	IdentityKey     string
	Kind            string
	Summary         string
	CounterpartName string
	DocumentID      string
	DetectedBy      string
	DueAt           *time.Time
	Confidence      float64
}

// Input is everything a briefing is derived from.
type Input struct {
	Actions []InputAction
	// LatestDocumentAt is the newest observed_at among Actions. It advances
	// whenever an action is re-observed, which is the signal that the briefing's
	// underlying facts moved.
	LatestDocumentAt time.Time
	// Model is the LLM model name. Swapping models must not serve the previous
	// model's sentences from cache.
	Model string
}

// Sentence is one briefing sentence together with the evidence that justifies
// it. DocumentIDs is never empty for a sentence that survived validation — a
// sentence with no evidence is discarded, not shown.
type Sentence struct {
	Text         string   `json:"text"`
	DocumentIDs  []string `json:"document_ids"`
	IdentityKeys []string `json:"identity_keys"`
}

// Result is a generated briefing.
type Result struct {
	Sentences []Sentence
	// Degraded is true when the sentences are the deterministic count-only
	// fallback rather than model output. The UI surfaces this so a user never
	// mistakes an aggregate for a summary.
	Degraded bool
	// DroppedCount is how many model sentences failed evidence validation. It is
	// shown to the user as a trust signal, not hidden as debug output.
	DroppedCount int
	GeneratedAt  time.Time
}

// CacheKey is the content hash of everything that can change a briefing:
// the prompt version, the model, the newest observation time, and the SET of
// open identity keys.
//
// Invalidation is entirely implicit — there is no TTL and no explicit purge.
// A new action, a resolved action, a re-observation, a prompt edit, or a model
// swap each change the key on its own.
//
// Summary text is deliberately absent. The same identity_key denotes the same
// real-world action (spec §5.5), so a reworded re-extraction is not a reason to
// regenerate. The useful side effect is that no personal data enters the hash
// input, which is why the resulting key is safe to write to logs.
func (in Input) CacheKey() string { return cacheKeyWithVersion(in, PromptVersion) }

func cacheKeyWithVersion(in Input, version string) string {
	keys := make([]string, 0, len(in.Actions))
	for _, a := range in.Actions {
		keys = append(keys, a.IdentityKey)
	}
	// Sorting is required, not cosmetic: PostgreSQL makes no ordering promise
	// beyond the ORDER BY, and an order-sensitive key would miss constantly.
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(version)
	b.WriteString("|")
	b.WriteString(in.Model)
	b.WriteString("|")
	b.WriteString(in.LatestDocumentAt.UTC().Format(time.RFC3339))
	b.WriteString("|")
	b.WriteString(strings.Join(keys, "\n"))

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
