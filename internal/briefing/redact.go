package briefing

// keyPrefixRunes is how much of an identity_key is safe to log: enough to
// correlate two log lines about the same action, not enough to be a stable
// handle for anything else. "action:" + 4 hex characters.
const keyPrefixRunes = len("action:") + 4

// KeyPrefix truncates an identity_key for logging.
//
// The full key is a deterministic hash and is not itself personal data, but it
// is the join handle to a row whose summary is. Logging only a prefix keeps log
// lines correlatable without making the log a usable index into the actions
// table.
//
// Truncation is rune-based so a non-hex value cannot be cut mid-rune and turn
// into invalid UTF-8 in the log stream.
func KeyPrefix(identityKey string) string {
	runes := []rune(identityKey)
	if len(runes) <= keyPrefixRunes {
		return identityKey
	}
	return string(runes[:keyPrefixRunes])
}

// SafeLogFields returns slog key/value pairs describing a briefing generation
// with no personal data in them.
//
// The rule this enforces (issue #194): action summaries, counterpart names,
// resolution notes, prompts, and raw model output must never reach the logs.
// A briefing's text is a restatement of the user's own conversations, so a
// single well-meant "log the response we failed to parse" turns the container
// log — and every log collector behind it — into a copy of the user's private
// messages.
//
// What is left is enough to operate the feature: how many actions went in, how
// many sentences came out, how many were discarded for lacking evidence,
// whether the result was the degraded aggregate, and a truncated key prefix for
// correlation. Nothing here is free text.
func SafeLogFields(in Input, r Result) []any {
	fields := []any{
		"action_count", len(in.Actions),
		"sentence_count", len(r.Sentences),
		"dropped_count", r.DroppedCount,
		"degraded", r.Degraded,
		"model", in.Model,
	}
	if len(in.Actions) > 0 {
		fields = append(fields, "first_action", KeyPrefix(in.Actions[0].IdentityKey))
	}
	return fields
}
