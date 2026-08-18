// Package briefing turns a set of open actions into a short, evidence-backed
// summary.
//
// # The contract
//
// Every sentence a briefing shows carries the document ids it is derived from,
// and the server discards any sentence whose ids it did not itself put in the
// prompt (see Validate). This is the reason the package exists: a system that
// says "reply to this" without being able to show which message says so is
// asking for trust it has not earned. The check is mechanical, not a request in
// the prompt — the prompt states the rule only because stating it improves
// compliance.
//
// When nothing survives validation, the result is a deterministic count of the
// open actions by kind, flagged Degraded so the UI can say "aggregate only".
// The fallback carries evidence too; the rule has no exceptions.
//
// # Cost
//
// Briefings are cached on a hash of (prompt version, model, newest observation
// time, sorted set of open identity keys). Invalidation is implicit: any change
// to those inputs changes the key. Summary text is deliberately not part of the
// key — the same identity_key is the same real-world action even if a
// re-extraction reworded it.
//
// # Privacy
//
// Action summaries, counterpart names, notes, prompts, and raw model output are
// never logged (issue #194): all of them are restatements of the user's private
// conversations. Log lines are built from SafeLogFields, which can only emit
// counts, flags, and a truncated key prefix. Parse failures are reported with
// lengths and offsets — never with err.Error(), because encoding/json quotes the
// offending input in its message.
//
// The package has no database or HTTP dependency; the API layer converts store
// rows into Input and Result into a response.
package briefing
