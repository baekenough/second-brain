package model

import (
	"math"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// SourceType identifies the origin of a document.
type SourceType string

const (
	SourceSlack      SourceType = "slack"
	SourceGitHub     SourceType = "github"
	SourceGDrive     SourceType = "gdrive"
	SourceNotion     SourceType = "notion"
	SourceFilesystem SourceType = "filesystem"
	SourceDiscord    SourceType = "discord"
	SourceTelegram   SourceType = "telegram"
	SourceSecretary  SourceType = "secretary"
	// SourceLLMMemory is DEPRECATED as of 2026-08-25 — DO NOT write new
	// documents with this source_type.
	//
	// 폐기 사유(2026-08-25): memory-collector 데몬이 Claude Code/Codex 세션
	// 트랜스크립트 전체를 이 source_type 으로 적재하고 있었다. 19,933건
	// (평균 35,930자, 최대 490,324자, 청크 353,843개 — 코퍼스 청크의 76.5%)이
	// 쌓여 검색 랭킹을 세션 덤프가 지배하면서 RAG 품질을 잠식했다. 전량
	// 폐기(삭제)했고, 수집 데몬은 4대 머신(macmini/ubuntu1/ubuntu2 등)에서
	// 전부 정지·비활성화했다.
	//
	// MCP add_note 도구도 과거 이 값을 썼으나(에이전트가 의도적으로 남기는
	// 짧은 노트 — 세션 덤프와는 성격이 다른 데이터), 같은 이름표를 공유해
	// 구분이 불가능했던 것이 문제였다. add_note 는 이제 SourceAgentNote 를
	// 쓴다(cmd/mcp/main.go registerAddNoteTool). 이 상수 자체를 지우지 않는
	// 이유는 기존에 이미 적재된 레거시 문서(예: secretary 경유 llm-memory
	// 1건, migrations/027 참고)를 여전히 이 값으로 식별해야 하기 때문이다.
	//
	// internal/store의 upsert 가드(checkSourceTypeGuard)가 이 타입으로의
	// 신규 저장 시도를 감지해 경고 로그를 남긴다 — 이 주석이 없어지면
	// "왜 막혀 있지?" 하고 나중에 누군가 되살릴 수 있으므로 반드시 유지할 것.
	// 이 값을 model.KnownSourceTypes()/DeprecatedSourceTypes() 에서 지우거나
	// 옮기려면 위 배경을 먼저 이해할 것 (internal/model/source_validation.go).
	SourceLLMMemory      SourceType = "llm-memory"
	SourceGmail          SourceType = "gmail"
	SourceCalendar       SourceType = "calendar"
	SourceSMS            SourceType = "sms"
	SourceCallLog        SourceType = "call-log"
	SourceCallTranscript SourceType = "call-transcript"
	SourceUpload         SourceType = "upload"
	// SourceNote is a user-authored note captured via POST /api/v1/notes
	// (Capture). Distinct from SourceAgentNote (MCP add_note tool,
	// AI-agent-authored) — see spec §3.3. Content is write-once; only
	// Title/Metadata are ever updated after creation, by NoteEnrichmentWorker.
	// NoteEnrichmentWorker (and the insight-extraction pipeline it drives) is
	// scoped to source_type='note' specifically — SourceAgentNote documents
	// are NOT picked up by it. Keep the two source types separate rather than
	// merging them; see SourceAgentNote's doc comment for why.
	SourceNote SourceType = "note"
	// SourceInsight is an LLM-derived inference extracted from a SourceNote
	// document by NoteEnrichmentWorker. Never user-authored; always carries
	// Metadata.provenance.source_note_id back to its origin note (spec §3,
	// §6.5). Excluded from default /api/v1/search results (see
	// internal/api/search.go applyInsightExclusionDefault).
	SourceInsight SourceType = "insight"
	// SourceAgentNote is a note deliberately persisted by an AI agent via the
	// MCP add_note tool (cmd/mcp/main.go registerAddNoteTool). Added
	// 2026-08-25 as the replacement for SourceLLMMemory, which add_note used
	// to write to — see SourceLLMMemory's doc comment for the full
	// background (session-transcript contamination that source_type shared
	// with these deliberately-authored notes).
	//
	// Deliberately NOT merged into SourceNote: SourceNote is scoped by spec
	// §3.3 to user-authored Capture content and is the sole input to
	// NoteEnrichmentWorker's insight-extraction pipeline. Writing
	// agent-authored notes there would silently enroll every MCP add_note
	// call in that (LLM-cost-bearing) pipeline — an unrequested behavior
	// change, not just a rename.
	SourceAgentNote SourceType = "agent-note"
)

// Document represents a piece of content collected from an external source.
type Document struct {
	ID         uuid.UUID      `json:"id"`
	SourceType SourceType     `json:"source_type"`
	SourceID   string         `json:"source_id"`
	Title      string         `json:"title"`
	Content    string         `json:"content"`
	Metadata   map[string]any `json:"metadata"`
	Embedding  []float32      `json:"-"`                    // omit from REST: large vector
	Status     string         `json:"status"`               // "active", "deleted", "moved"
	DeletedAt  *time.Time     `json:"deleted_at,omitempty"` // nil for active documents
	// OccurredAt is the timestamp of the original event: email sent date,
	// calendar event start time, SMS/call time, etc.  It is distinct from
	// CollectedAt (when second-brain ingested the document).  Nil when the
	// collector has no event-time concept or the value could not be parsed.
	// "Latest" queries sort by COALESCE(occurred_at, collected_at) DESC so
	// documents without OccurredAt degrade gracefully to ingest order.
	OccurredAt  *time.Time `json:"occurred_at,omitempty"`
	CollectedAt time.Time  `json:"collected_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`

	// LLM-generated summary fields (populated asynchronously by SummarizerWorker).
	// NULL in DB until the worker processes the document.
	TitleSummary     string    `json:"title_summary,omitempty"`
	BulletSummary    string    `json:"bullet_summary,omitempty"`
	SummaryEmbedding []float32 `json:"-"` // omit from REST: large vector
}

// SearchResult wraps a Document with relevance scoring metadata.
type SearchResult struct {
	Document
	Score     float64  `json:"score"`
	MatchType string   `json:"match_type"`         // "fulltext", "vector", or "hybrid"
	Entities  []Entity `json:"entities,omitempty"` // named entities extracted from the document; nil when not populated
}

// SearchWeights controls RRF fusion behaviour.
//
// Zero-value contract:
//   - A zero SearchWeights{} means "use all defaults" — Defaults() replaces
//     zero/NaN/Inf fields with their canonical values.
//   - To disable the SummaryVec signal explicitly, set DisableSummaryVec=true.
//     Defaults() then forces SummaryVec to 0.0 regardless of the SummaryVec field,
//     bypassing the coverage gate entirely (#63).
//   - To let the coverage gate decide, leave both SummaryVec and DisableSummaryVec
//     at their zero values.
//   - To force a specific weight (bypassing the gate), set SummaryVec to the
//     desired value with DisableSummaryVec=false.
type SearchWeights struct {
	FTSWeight  float64 `json:"fts_weight"`
	VecWeight  float64 `json:"vec_weight"`
	BigmWeight float64 `json:"bigm_weight"`
	// SummaryVec is the RRF weight for the summary_embedding CTE in hybrid search.
	// When zero (the default / unset) and DisableSummaryVec is false, the coverage
	// gate in hybridSearch decides the effective weight based on SummaryCoverageRatio (see #63).
	// When explicitly set to a positive value by the caller (and DisableSummaryVec is false),
	// that value is used directly and the coverage gate is bypassed.
	SummaryVec float64 `json:"summary_vec_weight"`
	// DisableSummaryVec, when true, forces SummaryVec to 0.0 after Defaults(),
	// bypassing the coverage gate. Use this when the caller explicitly wants to
	// exclude the summary-embedding lane from hybrid search (#63).
	// This disambiguates "I haven't set SummaryVec" (zero, gate applies) from
	// "I explicitly want SummaryVec disabled" (DisableSummaryVec=true).
	DisableSummaryVec bool `json:"disable_summary_vec,omitempty"`
	// EntityWeight is the RRF weight for the entity-match lane (#139).
	// When zero (the default) the entity lane is enabled at DefaultEntityWeight
	// when ENTITY_EXTRACTION_ENABLED env var is set; set to a negative value
	// to explicitly disable. The lane is a no-op when no entity rows exist for
	// the query's matched entities.
	EntityWeight float64 `json:"entity_weight,omitempty"`
	RRFK         float64 `json:"rrf_k"`
}

// DefaultSummaryVecWeight is the weight used for the summary_embedding RRF
// signal once summary coverage exceeds SummaryVecCoverageThreshold.
const DefaultSummaryVecWeight = 0.8

// DefaultEntityWeight is the RRF weight used for the entity-match lane (#139)
// when ENTITY_EXTRACTION_ENABLED is set and EntityWeight is unset (zero).
const DefaultEntityWeight = 0.5

// SummaryVecCoverageThreshold returns the minimum fraction (0–1) of active
// documents that must have a non-NULL summary_embedding before the summvec
// RRF lane is enabled in hybrid search.
//
// The threshold is read from SUMMARY_VEC_COVERAGE_THRESHOLD env var (float in
// [0, 1]).  Defaults to 0.80 when unset or invalid.
func SummaryVecCoverageThreshold() float64 {
	if v := os.Getenv("SUMMARY_VEC_COVERAGE_THRESHOLD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 && f <= 1 {
			return f
		}
	}
	return 0.80
}

// Defaults returns a copy with zero, NaN, Inf, or negative fields replaced by defaults.
//
// SummaryVec semantics (#63):
//   - DisableSummaryVec=true  → SummaryVec is forced to 0.0 (explicit disable, bypasses gate).
//   - SummaryVec==0 (unset)   → Zero is preserved so hybridSearch can apply the coverage gate.
//   - SummaryVec>0 (explicit) → Value is preserved; coverage gate is bypassed by caller.
//   - SummaryVec<0 or NaN/Inf → Invalid; replaced with DefaultSummaryVecWeight.
func (w SearchWeights) Defaults() SearchWeights {
	if w.RRFK == 0 || math.IsNaN(w.RRFK) || math.IsInf(w.RRFK, 0) {
		w.RRFK = 60.0
	}
	if w.FTSWeight == 0 || math.IsNaN(w.FTSWeight) || math.IsInf(w.FTSWeight, 0) {
		w.FTSWeight = 1.0
	}
	if w.VecWeight == 0 || math.IsNaN(w.VecWeight) || math.IsInf(w.VecWeight, 0) {
		w.VecWeight = 1.0
	}
	if w.BigmWeight == 0 || math.IsNaN(w.BigmWeight) || math.IsInf(w.BigmWeight, 0) {
		w.BigmWeight = 1.0
	}
	// DisableSummaryVec=true: caller explicitly wants the summary-embedding lane
	// disabled. Force SummaryVec to 0.0 regardless of its current value.
	// This resolves the ambiguity between "unset (gate applies)" and "explicitly
	// disabled" that a plain zero field cannot express (#63).
	if w.DisableSummaryVec {
		w.SummaryVec = 0.0
		return w
	}
	// SummaryVec: NaN/Inf are invalid; replace with the default weight.
	// Negative values are replaced with the default (negative weight is nonsensical).
	// Zero is preserved: it signals "unset by caller → apply coverage gate"
	// in hybridSearch.  The coverage gate then sets the effective weight to
	// DefaultSummaryVecWeight (0.8) when coverage is sufficient, or 0.0 when not.
	if math.IsNaN(w.SummaryVec) || math.IsInf(w.SummaryVec, 0) || w.SummaryVec < 0 {
		w.SummaryVec = DefaultSummaryVecWeight
	}
	// EntityWeight (#139): negative values explicitly disable the lane (weight=0).
	// Zero (unset) is resolved in hybridSearch based on ENTITY_EXTRACTION_ENABLED.
	// NaN/Inf are invalid and are treated as "unset" (zero).
	if math.IsNaN(w.EntityWeight) || math.IsInf(w.EntityWeight, 0) {
		w.EntityWeight = 0.0 // treat invalid as unset; gate resolves in hybridSearch
	}
	return w
}

// SearchQuery describes a search request.
type SearchQuery struct {
	Query string

	// SourceType is the legacy single-source include filter (nil = all
	// sources). It is NOT deprecated: several callers set exactly one source
	// and reading a pointer is clearer there than a one-element slice.
	//
	// SourceTypes is the multi-source form (empty = no filter). A query planner
	// emits a list, so both shapes exist.
	//
	// The two are ONE filter, and IncludeSourceTypes is its only definition —
	// see that method for how they combine and why. Nothing downstream may read
	// either field directly to decide what is included.
	SourceType         *SourceType
	SourceTypes        []SourceType
	ExcludeSourceTypes []SourceType // source types to exclude from results
	Limit              int
	Embedding          []float32 // populated by search service when available
	IncludeDeleted     bool      // when true, search includes deleted/moved docs
	// Sort selects the RANKING of the candidate set; it never changes which
	// documents enter it. Two values are recognised, and only the exact
	// literal "recent" is special — everything else, including "" and
	// "relevance", ranks by score DESC.
	//
	// "recent" means NEAREST TO NOW FIRST, which is not the same clause in
	// both directions of time and is resolved by the store from the event-time
	// window below:
	//
	//   - window entirely in the future (OccurredFrom >= now):
	//     occurred_at ASC — soonest first, so "다음 주 일정" starts with the
	//     most imminent entry.
	//   - otherwise (past window, straddling window, or no window):
	//     COALESCE(occurred_at, collected_at) DESC — most recent first.
	//
	// The direction is deliberately NOT a field here. It is a pure function of
	// OccurredFrom and the current time, both of which the store already has,
	// and a second writer of a derived value is how this field's documented
	// behaviour drifted from its actual behaviour in the first place. The rule
	// itself lives in RecencyAscending below — the store renders it as SQL and
	// the search service replays it over merged results; neither restates it.
	Sort      string
	UseHyDE   bool          // when true, expand query via HyDE before retrieval
	Weights   SearchWeights // zero value uses defaults (k=60, equal weights)
	UseRerank bool          `json:"use_rerank,omitempty"` // when true, apply cross-encoder reranking post-retrieval

	// OccurredFrom / OccurredTo constrain results to the half-open event-time
	// window [OccurredFrom, OccurredTo) on documents.occurred_at. Either bound
	// may be nil, meaning "unbounded on that side"; both nil disables the
	// filter entirely.
	//
	// This is a CANDIDATE-POOL constraint, not a sort hint: the store applies
	// it as a WHERE predicate inside every retrieval lane, exactly like
	// SourceType/ExcludeSourceTypes. Sort="recent" cannot substitute for it —
	// sorting can only reorder candidates that a lane already retrieved.
	//
	// Rows with occurred_at IS NULL are EXCLUDED whenever either bound is set.
	// The comparison is deliberately made against occurred_at alone rather than
	// COALESCE(occurred_at, collected_at): a large part of the corpus has a NULL
	// occurred_at, and coalescing would silently reinterpret "ingested during
	// the window" as "happened during the window".
	OccurredFrom *time.Time
	OccurredTo   *time.Time
}

// SortRecent is the ONLY value of SearchQuery.Sort that ranks by time. Every
// other value — including "" and "relevance" — ranks by relevance score. It is
// a constant because Sort is caller-controlled and reaches an SQL ORDER BY by
// string concatenation: the comparison against this exact literal is the
// whitelist that keeps it out of the statement.
const SortRecent = "recent"

// SortsByRecency reports whether this query asks for the time ranking.
//
// Every component that has to honour Sort asks here rather than comparing the
// string itself, so the whitelist has one definition and cannot be widened by
// accident in one place only (a case-insensitive or prefix comparison
// somewhere downstream would re-open the injection surface the literal
// comparison closes).
func (q SearchQuery) SortsByRecency() bool { return q.Sort == SortRecent }

// RecencyAscending is THE definition of which way SortRecent points. Both
// consumers derive their order from this one function:
//
//   - internal/store.sortOrder turns it into the SQL ORDER BY clause
//     (occurred_at ASC vs COALESCE(occurred_at, collected_at) DESC),
//   - internal/search re-establishes the same order in Go after it has merged
//     chunk-lane candidates into the store's result set, which the store's
//     ORDER BY cannot cover because those rows never went through it.
//
// "recent" means NEAREST TO NOW FIRST. That is a DESC over event time only
// while every candidate lies in the past, which stopped being true when the
// query planner began emitting forward-looking windows: for "다음 주 일정" a
// plain DESC returns furthest-future-first, the reverse of what was asked.
//
// The direction is decided on the narrowest test that is provably safe — the
// LOWER bound alone:
//
//   - OccurredFrom is set and is not before now -> the window lies ENTIRELY in
//     the future, because every surviving row satisfies occurred_at >= From.
//     Ascending: soonest first.
//   - anything else (past window, window straddling now, upper bound only, no
//     window at all) -> descending, the historical behaviour.
//
// A window that merely ENDS in the future may still be mostly historical
// ("최근 한 달"), and one that straddles now has no unambiguous "nearest"
// direction, so both stay on the branch they have always been on: the fix must
// be invisible to every query that already worked.
//
// It lives on the model rather than in either consumer because a duplicated
// copy of this rule is unobservable in the response — a merged result set and
// a store-only result set are the same shape, so the two orders would differ
// only for the queries that happen to reach the chunk lanes.
func (q SearchQuery) RecencyAscending(now time.Time) bool {
	return q.OccurredFrom != nil && !q.OccurredFrom.Before(now)
}

// IncludeSourceTypes returns the effective source-type include set: the UNION
// of the singular SourceType and the plural SourceTypes, deduplicated, with the
// singular value first. An empty result means "no include filter" — i.e. all
// sources — never "the empty set".
//
// Union, rather than intersection or precedence, because:
//
//   - Both fields are include filters, and an include filter is set membership
//     (source_type = ANY(...)). The union is the natural lift of the one-element
//     case into the list case; no other rule leaves the singular field meaning
//     what it has always meant.
//   - It is monotone. Callers that still set only SourceType
//     (internal/api/search.go, internal/api/graphql.go, cmd/mcp) cannot lose a
//     document because some other layer populated SourceTypes. Intersection can
//     silently produce an empty result when the two disagree; precedence
//     silently discards whichever field lost. Both failure modes are invisible
//     at the API boundary, which is the exact shape of the defect (#196) this
//     field was added to fix.
//
// Overlap between the include set and ExcludeSourceTypes is resolved where the
// filters are applied, not here: exclude wins (see internal/search.Service).
//
// The returned slice never aliases the caller's SourceTypes backing array, so
// filter steps may sort or truncate it freely.
func (q SearchQuery) IncludeSourceTypes() []SourceType {
	if q.SourceType == nil && len(q.SourceTypes) == 0 {
		return nil
	}
	out := make([]SourceType, 0, len(q.SourceTypes)+1)
	seen := make(map[SourceType]struct{}, len(q.SourceTypes)+1)
	add := func(st SourceType) {
		if _, dup := seen[st]; dup {
			return
		}
		seen[st] = struct{}{}
		out = append(out, st)
	}
	if q.SourceType != nil {
		add(*q.SourceType)
	}
	for _, st := range q.SourceTypes {
		add(st)
	}
	return out
}
