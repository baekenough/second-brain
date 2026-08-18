package briefing

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// maxSentenceRunes bounds one sentence. A model that returns a whole paragraph
// in a single "sentence" has ignored the schema, and a paragraph cannot be
// honestly attributed to one set of documents.
const maxSentenceRunes = 400

// llmSentence is the wire shape of one sentence in the model's reply.
type llmSentence struct {
	Text         string   `json:"text"`
	DocumentIDs  []string `json:"document_ids"`
	IdentityKeys []string `json:"identity_keys"`
}

type llmPayload struct {
	Sentences []llmSentence `json:"sentences"`
}

// Validate turns a raw model reply into a Result, discarding every sentence
// that is not backed by a document the server actually put in the prompt.
//
// This is the whole point of the feature. A briefing that says "call the
// plumber back" without being able to show which message says so is worse than
// no briefing: it is a confident claim with nothing behind it. So the check is
// mechanical rather than a request in the prompt — the server owns the set of
// ids it showed the model, and anything outside that set is a fabrication by
// definition.
//
// Discarding is per sentence, not per response: one bad sentence must not take
// the valid ones with it, or the briefing would be empty in practice.
//
// A non-nil error means the reply could not be decoded at all. The returned
// Result is still usable (the deterministic fallback), so callers can report the
// failure without having to invent a response.
func Validate(raw string, in Input) (Result, error) {
	payload, err := decodePayload(raw)
	if err != nil {
		r := fallbackResult(in)
		return r, err
	}

	allowedDocs := make(map[string]bool, len(in.Actions))
	allowedKeys := make(map[string]bool, len(in.Actions))
	for _, a := range in.Actions {
		if a.DocumentID != "" {
			allowedDocs[a.DocumentID] = true
		}
		if a.IdentityKey != "" {
			allowedKeys[a.IdentityKey] = true
		}
	}

	kept := make([]Sentence, 0, len(payload.Sentences))
	dropped := 0
	for _, s := range payload.Sentences {
		text := strings.TrimSpace(s.Text)
		if text == "" || utf8.RuneCountInString(text) > maxSentenceRunes {
			dropped++
			continue
		}
		if len(s.DocumentIDs) == 0 {
			dropped++
			continue
		}
		grounded := true
		for _, id := range s.DocumentIDs {
			if !allowedDocs[id] {
				grounded = false
				break
			}
		}
		if !grounded {
			dropped++
			continue
		}

		// An invented identity_key is dropped on its own: the sentence's
		// evidence is its document ids, and those are still valid.
		keys := make([]string, 0, len(s.IdentityKeys))
		for _, k := range s.IdentityKeys {
			if allowedKeys[k] {
				keys = append(keys, k)
			}
		}

		kept = append(kept, Sentence{
			Text:         text,
			DocumentIDs:  s.DocumentIDs,
			IdentityKeys: keys,
		})
		if len(kept) == maxSentences {
			break
		}
	}

	if len(kept) == 0 {
		r := fallbackResult(in)
		r.DroppedCount = dropped
		return r, nil
	}

	return Result{
		Sentences:    kept,
		DroppedCount: dropped,
		GeneratedAt:  time.Now().UTC(),
	}, nil
}

// decodePayload extracts the JSON object from a reply that may be wrapped in a
// code fence or surrounded by prose. Both habits have been observed from this
// project's model in production.
//
// The failure path deliberately carries no fragment of raw: the reply is a
// restatement of the user's own conversations, so quoting it in an error (which
// ends up in logs) would leak personal data. Only content-free measurements are
// reported.
func decodePayload(raw string) (llmPayload, error) {
	trimmed := strings.TrimSpace(raw)

	if strings.HasPrefix(trimmed, "```") {
		if idx := strings.Index(trimmed, "\n"); idx != -1 {
			trimmed = trimmed[idx+1:]
		}
		trimmed = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(trimmed), "```"))
	}

	start, end := strings.Index(trimmed, "{"), strings.LastIndex(trimmed, "}")
	if start < 0 || end <= start {
		return llmPayload{}, fmt.Errorf("briefing: reply carries no JSON object (len=%d, first_brace=%d, last_brace=%d)",
			len(raw), start, end)
	}
	trimmed = trimmed[start : end+1]

	var payload llmPayload
	if err := json.NewDecoder(strings.NewReader(trimmed)).Decode(&payload); err != nil {
		// err.Error() is NOT included: encoding/json quotes the offending input
		// in its message. Only the offset is content-free enough to keep.
		var syntaxErr *json.SyntaxError
		offset := int64(-1)
		if errors.As(err, &syntaxErr) {
			offset = syntaxErr.Offset
		}
		return llmPayload{}, fmt.Errorf("briefing: reply is not valid JSON (len=%d, offset=%d)", len(raw), offset)
	}
	return payload, nil
}

// kindLabels renders the closed action vocabulary in Korean for the fallback
// sentence. The order is the order they are counted in.
var kindLabels = []struct {
	kind  string
	label string
}{
	{"awaiting_my_reply", "응답 대기"},
	{"my_commitment", "내 약속"},
	{"their_commitment", "상대 약속"},
	{"scheduled", "예정"},
}

// fallbackResult builds the deterministic, LLM-free briefing used whenever the
// model's output cannot be trusted.
//
// It is a count, not prose: "응답 대기 3건, 내 약속 2건." states only what the
// server itself computed, so it cannot be unfounded. It still carries the
// document ids of the counted actions, because the evidence rule has no
// exceptions — including for the fallback that exists to enforce it.
//
// With no actions at all there is nothing to count and nothing to warn about,
// so the result is empty and NOT marked degraded.
func fallbackResult(in Input) Result {
	if len(in.Actions) == 0 {
		return Result{Sentences: []Sentence{}, GeneratedAt: time.Now().UTC()}
	}

	counts := make(map[string]int, len(kindLabels))
	other := 0
	docSeen := make(map[string]bool, len(in.Actions))
	docIDs := make([]string, 0, len(in.Actions))
	keys := make([]string, 0, len(in.Actions))

	known := make(map[string]bool, len(kindLabels))
	for _, kl := range kindLabels {
		known[kl.kind] = true
	}

	for _, a := range in.Actions {
		if known[a.Kind] {
			counts[a.Kind]++
		} else {
			other++
		}
		if a.DocumentID != "" && !docSeen[a.DocumentID] {
			docSeen[a.DocumentID] = true
			docIDs = append(docIDs, a.DocumentID)
		}
		if a.IdentityKey != "" {
			keys = append(keys, a.IdentityKey)
		}
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(kindLabels)+1)
	for _, kl := range kindLabels {
		if n := counts[kl.kind]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d건", kl.label, n))
		}
	}
	if other > 0 {
		parts = append(parts, fmt.Sprintf("기타 %d건", other))
	}

	return Result{
		Sentences: []Sentence{{
			Text:         strings.Join(parts, ", ") + ".",
			DocumentIDs:  docIDs,
			IdentityKeys: keys,
		}},
		Degraded:    true,
		GeneratedAt: time.Now().UTC(),
	}
}
