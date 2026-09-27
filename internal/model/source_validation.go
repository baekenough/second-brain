package model

// KnownSourceTypes lists the source types that this corpus actually contains
// as of 2026-09-19 (see migration 033 background): gmail, sms, call,
// calendar, insight, note, and upload.
//
// This list is DELIBERATELY narrower than the full SourceType const block
// above. Several of those consts (SourceSlack, SourceGitHub, SourceGDrive,
// SourceNotion, SourceDiscord, SourceTelegram, SourceFilesystem) name
// integrations whose collector code still exists in internal/collector but
// which are not currently populating documents in production. They are not
// included here so that ValidateSourceType's "unknown" signal stays tied to
// what the corpus actually looks like today, not to every integration this
// codebase has ever shipped.
//
// SourceLLMMemory is also excluded: it was decommissioned as an active
// source (see agent memory project_macmini_prod_compose — secretary infra
// torn down) and its one remaining secretary-routed row is intentionally
// left alone by migration 027 rather than resurrected into a live bucket.
//
// SourceCallLog/SourceCallTranscript are also excluded as of migration 033:
// they were unified into SourceCall (see SourceCallLog's doc comment in
// document.go) and are listed in DeprecatedSourceTypes below instead.
//
// Extending this list (e.g. re-enabling a dormant collector) is a one-line
// change here; nothing else needs updating for the guard in internal/store
// to recognise the new value.
// SourceAgentNote is included because it is the 2026-08-25 replacement for
// SourceLLMMemory (see that const's doc comment) — the MCP add_note tool's
// real, ongoing write path, not a legacy or container type.
func KnownSourceTypes() []SourceType {
	return []SourceType{
		SourceGmail,
		SourceSMS,
		SourceCall,
		SourceCalendar,
		SourceInsight,
		SourceNote,
		SourceUpload,
		SourceAgentNote,
	}
}

// IsKnownSourceType reports whether st is one of KnownSourceTypes.
func IsKnownSourceType(st SourceType) bool {
	for _, k := range KnownSourceTypes() {
		if st == k {
			return true
		}
	}
	return false
}

// ContainerSourceTypes lists source_type values that are known to aggregate
// multiple, unrelated real sources under one bucket rather than naming a
// single origin. SourceSecretary is the motivating (and, as of migration 027,
// historical) example: it mixed gmail/sms/call-log/call-transcript/calendar
// content, which made source-type filtering and query-planner routing unable
// to distinguish them (see migrations/027_secretary_source_normalization.sql).
//
// No new document should ever be written with a container source_type — the
// value only exists in the corpus as a target for one-time normalization
// migrations. See internal/store's upsert guard for where this is enforced.
func ContainerSourceTypes() []SourceType {
	return []SourceType{SourceSecretary}
}

// IsContainerSourceType reports whether st is a known container/aggregate
// source_type (see ContainerSourceTypes).
func IsContainerSourceType(st SourceType) bool {
	for _, c := range ContainerSourceTypes() {
		if st == c {
			return true
		}
	}
	return false
}

// DeprecatedSourceTypes lists source_type values that must never be used for
// a new document, but that already-collected documents may still carry (so
// the value cannot simply be deleted from the model).
//
// SourceLLMMemory is deprecated as of 2026-08-25: see its doc comment in
// document.go for the full background (session-transcript contamination —
// 19,933 documents / 353,843 chunks / 76.5% of the corpus, purged; the
// memory-collector daemon that produced them stopped on every machine that
// ran it). internal/store's upsert guard (checkSourceTypeGuard) logs a
// warning — with the incoming document's source_id and any identifying
// metadata fields — whenever a write targets a deprecated source_type, so a
// reintroduced write path is discoverable instead of silently resurrecting
// the same contamination.
//
// SourceCallLog/SourceCallTranscript are deprecated as of 2026-09-19
// (migration 033): see SourceCallLog's doc comment in document.go for the
// full background (unified into SourceCall — one document per phone call).
// The same upsert guard logs a warning on any new write to either value, so
// a collector or handler that was missed during the unification is
// discoverable rather than silently reintroducing the split.
func DeprecatedSourceTypes() []SourceType {
	return []SourceType{SourceLLMMemory, SourceCallLog, SourceCallTranscript}
}

// IsDeprecatedSourceType reports whether st is a known deprecated source_type
// (see DeprecatedSourceTypes).
func IsDeprecatedSourceType(st SourceType) bool {
	for _, d := range DeprecatedSourceTypes() {
		if st == d {
			return true
		}
	}
	return false
}

// legacySourceTypeAliases maps a pre-migration-033 SourceType (see
// SourceCallLog's doc comment in document.go) to the value migration 033
// unified it into. NormalizeSourceType is the only reader — nowhere else
// should compare a caller-supplied SourceType against this map directly.
//
// SourceLLMMemory is deliberately NOT an entry here: it was decommissioned
// (documents purged, collector stopped), not merged into a replacement value,
// so there is nothing to alias it to.
var legacySourceTypeAliases = map[SourceType]SourceType{
	SourceCallLog:        SourceCall,
	SourceCallTranscript: SourceCall,
}

// NormalizeSourceType maps a caller-supplied SourceType to the value the
// corpus actually stores documents under today, so a filter written against a
// since-merged alias matches real rows instead of silently matching none.
//
// Motivating bug: migration 033 (2026-09-19) rewrote every "call-log"/
// "call-transcript" document in Postgres to source_type='call' (see
// SourceCallLog's doc comment). SourceCallLog/SourceCallTranscript stayed in
// this package only so ValidateSourceType-style guards keep recognising the
// value on old rows — but a caller (REST /api/v1/search, the MCP search
// tool's "source" parameter, a query-planner LLM output that has not been
// told about the rename) that still filters BY one of the old values passed
// that guard and then matched zero documents. No error, no warning — a
// caller reading only the HTTP status or the MCP tool result cannot tell
// "this source has no matches" apart from "you spelled it correctly but it
// doesn't exist anymore".
//
// Every place a SearchQuery's include/exclude filter turns into a SQL or
// OpenSearch predicate must resolve a raw SourceType through here first (see
// SearchQuery.IncludeSourceTypes, the single point that currently does).
// Values with no alias — including every other DeprecatedSourceTypes() entry
// and every ordinary, current SourceType — are returned unchanged, so calling
// this on an already-normalized value is a no-op.
func NormalizeSourceType(st SourceType) SourceType {
	if alias, ok := legacySourceTypeAliases[st]; ok {
		return alias
	}
	return st
}
