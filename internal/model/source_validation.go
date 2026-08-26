package model

// KnownSourceTypes lists the source types that this corpus actually contains
// as of 2026-08-25 (see migration 027 background): gmail, sms, call-log,
// call-transcript, calendar, insight, note, and upload.
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
		SourceCallLog,
		SourceCallTranscript,
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
func DeprecatedSourceTypes() []SourceType {
	return []SourceType{SourceLLMMemory}
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
