// Package chunker — adaptive strategy selection.
//
// SelectOptions picks chunk Options tuned for a document's source type and
// content shape. The selection is fully deterministic (no LLM calls). Future
// work can replace or augment the heuristics here with the LLM-scored
// intrinsic-metrics approach described in the Adaptive Chunking paper
// (Chen et al. 2024, "Evaluating Chunking Strategies for Retrieval").
//
// # Intrinsic metrics (paper §3) — future LLM-scored extension points
//
//   - RC  (Retrieval Coherence): each chunk answers a coherent sub-question.
//   - BI  (Block Integrity): chunks preserve logical block boundaries
//     (paragraphs, sections).
//   - ICC (Inter-Chunk Coherence): adjacent chunks remain topically linked.
//   - DCC (Document-Chunk Coherence): every chunk stays faithful to the
//     source document's overall topic.
//   - SC  (Size Compliance): chunk byte-size falls within [TargetSize,MaxSize].
//
// The deterministic rules below approximate these metrics without LLM scoring:
//   - BI  → HeadingAware=true for structured docs; HeadingAware=false for chat.
//   - SC  → smaller TargetSize/MaxSize for short-turn chat sources.
//   - RC  → smaller chunks for high-density memory/agent sources.
package chunker

import (
	"strings"

	"github.com/baekenough/second-brain/internal/model"
)

// Per-source-type size constants — all values are in RUNES, not bytes (#145).
// Using runes ensures Korean text (3 bytes/rune) receives the same information
// density per chunk as ASCII text. Long-form/structured sources keep the global
// scheduler defaults (2000/4000/100 runes).
const (
	// Long-form structured (filesystem, notion, github, gdrive).
	// Same as the scheduler's defaultChunk* constants so there is no regression.
	// Units: runes (previously bytes — value unchanged; rune=byte for ASCII).
	longFormTargetSize = 2000
	longFormMaxSize    = 4000
	longFormOverlap    = 100

	// Short chat (slack, discord, telegram).
	// Chat turns are short and contain no heading structure.  Smaller chunks
	// improve retrieval granularity (better SC and RC scores).
	// Units: runes. chatMaxSize=1500 runes ≈ 4500 bytes for Korean, giving
	// each chunk ~3x more Korean content than the previous byte-based limit.
	chatTargetSize = 900
	chatMaxSize    = 1500
	chatOverlap    = 80

	// Memory / agent sources (secretary, llm-memory).
	// Mid-sized chunks: these documents are dense but still prose-like.
	// Units: runes.
	memTargetSize = 1200
	memMaxSize    = 2500
	memOverlap    = 100
)

// contentIsStructured is a lightweight heuristic for BI metric: returns true
// when the content contains heading markers or blank-line-separated paragraphs,
// suggesting HeadingAware splitting will find meaningful section boundaries.
//
// Future extension point (LLM Regex Splitter / Split-then-Merge):
// replace this function with an LLM-scored BI probe that queries the model for
// the dominant structural pattern in a 512-token content sample.
func contentIsStructured(content string) bool {
	// Has blank-line paragraph separators?
	if strings.Contains(content, "\n\n") {
		return true
	}
	// Has ATX Markdown headings?
	lines := strings.SplitN(content, "\n", 50) // sample first 50 lines only
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "#") && !strings.HasPrefix(t, `\#`) {
			return true
		}
	}
	return false
}

// SelectOptions returns chunking Options tuned for doc.SourceType and the
// shape of doc.Content.
//
// Strategy mapping (implemented):
//
//	filesystem, notion, github, gdrive → HeadingAware=true, 2000/4000/100
//	slack, discord, telegram           → HeadingAware=false, 900/1500/80
//	secretary, llm-memory              → HeadingAware=false, 1200/2500/100
//	unknown / default                  → HeadingAware=true, 2000/4000/100
//
// Content-shape override (BI metric heuristic):
// If a "long-form" source's content has no heading markers and no paragraph
// breaks, HeadingAware is downgraded to false to avoid creating single-chunk
// sections that span the full document.
func SelectOptions(doc model.Document) Options {
	switch doc.SourceType {
	// --- Long-form / structured sources ---
	// These consistently have Markdown headings, numbered sections, or at
	// minimum blank-line-delimited paragraphs (high BI, high ICC expected).
	case model.SourceFilesystem, model.SourceNotion, model.SourceGitHub, model.SourceGDrive:
		headingAware := contentIsStructured(doc.Content)
		return Options{
			TargetSize:   longFormTargetSize,
			MaxSize:      longFormMaxSize,
			Overlap:      longFormOverlap,
			HeadingAware: headingAware,
		}

	// --- Short chat / messaging sources ---
	// Chat turns are short, rarely contain headings, and benefit from fine-
	// grained chunks (better RC, lower risk of SC violation).
	case model.SourceSlack, model.SourceDiscord, model.SourceTelegram:
		return Options{
			TargetSize:   chatTargetSize,
			MaxSize:      chatMaxSize,
			Overlap:      chatOverlap,
			HeadingAware: false, // BI: chat has no heading structure
		}

	// --- Memory / agent sources ---
	// Secretary logs and LLM memory entries are dense prose without headings.
	// Mid-sized chunks balance RC (enough context per chunk) and SC.
	case model.SourceSecretary, model.SourceLLMMemory:
		return Options{
			TargetSize:   memTargetSize,
			MaxSize:      memMaxSize,
			Overlap:      memOverlap,
			HeadingAware: false, // BI: structured headings not expected
		}

	// --- Unknown / future source types ---
	// Default to the conservative long-form strategy (same as pre-issue-#60
	// behaviour) to avoid regressing on newly added sources.
	default:
		return Options{
			TargetSize:   longFormTargetSize,
			MaxSize:      longFormMaxSize,
			Overlap:      longFormOverlap,
			HeadingAware: true,
		}
	}
}
