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
	SourceSlack          SourceType = "slack"
	SourceGitHub         SourceType = "github"
	SourceGDrive         SourceType = "gdrive"
	SourceNotion         SourceType = "notion"
	SourceFilesystem     SourceType = "filesystem"
	SourceDiscord        SourceType = "discord"
	SourceTelegram       SourceType = "telegram"
	SourceSecretary      SourceType = "secretary"
	SourceLLMMemory      SourceType = "llm-memory"
	SourceGmail          SourceType = "gmail"
	SourceCalendar       SourceType = "calendar"
	SourceSMS            SourceType = "sms"
	SourceCallLog        SourceType = "call-log"
	SourceCallTranscript SourceType = "call-transcript"
	SourceUpload         SourceType = "upload"
)

// Document represents a piece of content collected from an external source.
type Document struct {
	ID          uuid.UUID      `json:"id"`
	SourceType  SourceType     `json:"source_type"`
	SourceID    string         `json:"source_id"`
	Title       string         `json:"title"`
	Content     string         `json:"content"`
	Metadata    map[string]any `json:"metadata"`
	Embedding   []float32      `json:"-"`                    // omit from REST: large vector
	Status      string         `json:"status"`               // "active", "deleted", "moved"
	DeletedAt   *time.Time     `json:"deleted_at,omitempty"` // nil for active documents
	// OccurredAt is the timestamp of the original event: email sent date,
	// calendar event start time, SMS/call time, etc.  It is distinct from
	// CollectedAt (when second-brain ingested the document).  Nil when the
	// collector has no event-time concept or the value could not be parsed.
	// "Latest" queries sort by COALESCE(occurred_at, collected_at) DESC so
	// documents without OccurredAt degrade gracefully to ingest order.
	OccurredAt  *time.Time     `json:"occurred_at,omitempty"`
	CollectedAt time.Time      `json:"collected_at"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`

	// LLM-generated summary fields (populated asynchronously by SummarizerWorker).
	// NULL in DB until the worker processes the document.
	TitleSummary    string    `json:"title_summary,omitempty"`
	BulletSummary   string    `json:"bullet_summary,omitempty"`
	SummaryEmbedding []float32 `json:"-"` // omit from REST: large vector
}

// SearchResult wraps a Document with relevance scoring metadata.
type SearchResult struct {
	Document
	Score     float64  `json:"score"`
	MatchType string   `json:"match_type"`          // "fulltext", "vector", or "hybrid"
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
	RRFK              float64 `json:"rrf_k"`
}

// DefaultSummaryVecWeight is the weight used for the summary_embedding RRF
// signal once summary coverage exceeds SummaryVecCoverageThreshold.
const DefaultSummaryVecWeight = 0.8

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
	return w
}

// SearchQuery describes a search request.
type SearchQuery struct {
	Query              string
	SourceType         *SourceType  // nil means all sources
	ExcludeSourceTypes []SourceType // source types to exclude from results
	Limit              int
	Embedding          []float32    // populated by search service when available
	IncludeDeleted     bool         // when true, search includes deleted/moved docs
	Sort               string       // "relevance" (default, score DESC) | "recent" (collected_at DESC)
	UseHyDE            bool         // when true, expand query via HyDE before retrieval
	Weights            SearchWeights // zero value uses defaults (k=60, equal weights)
	UseRerank          bool         `json:"use_rerank,omitempty"` // when true, apply cross-encoder reranking post-retrieval
}
