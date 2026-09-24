package api

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
)

// UTF-8 bytes conservatively bound byte-based tokenizer input without coupling
// this API to a provider tokenizer. Within a 32K envelope leave 8K for output
// and 4K for system instructions/message framing. This is an application cap,
// not a claim about the configured provider's actual context window.
const (
	askMessageBytes  = 20 * 1024
	askQuestionBytes = 4 * 1024
	askHistoryBytes  = 4 * 1024
	askExcerptBytes  = 4 * 1024
)
const excerptMarker = " [발췌: 나머지 생략]"

// evidencePrefixMarker and evidenceSuffixMarker mark an evidence passage's
// omitted head/tail (askPassage's lexical fallback and windowAround's
// located-span window both use them). Their byte lengths must be reserved
// BEFORE the surrounding window is sized — see windowAround's doc comment
// for why: a prior version sized the window against the raw budget first
// and relied on clipAskText's end-trim safety net to absorb any marker
// overflow afterward, which silently deleted the evidence itself whenever
// the located span sat at the very end of the document (#267 follow-up).
const (
	evidencePrefixMarker = "[앞부분 생략] "
	evidenceSuffixMarker = " [뒷부분 생략]"
)

// unlocatedEvidenceMarker (#267) is appended when a matched chunk's text
// exists (model.MatchedEvidence.Text is non-empty) but could not be located
// verbatim inside its parent document's Content — see
// model.MatchedEvidence's doc comment (chunker cleanup/heading
// prefix/paragraph merge means a chunk is not always a byte-for-byte
// substring of the document it came from). The chunk's own text is used
// as-is in that case; this marker makes the fallback visible rather than
// presenting it as a located, in-context passage.
const unlocatedEvidenceMarker = " [발췌: 매칭된 문단, 원문 내 위치 미확인]"

func clipAskText(text string, budget int) string {
	if len(text) <= budget {
		return text
	}
	if budget < len(excerptMarker) {
		return ""
	}
	end := budget - len(excerptMarker)
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + excerptMarker
}

// Keep complete recent turns where possible. Mark clipped text rather than
// presenting a cut-off sentence as the full previous answer.
func budgetAskHistory(history []askHistoryTurn) []askHistoryTurn {
	remaining := askHistoryBytes
	kept := make([]askHistoryTurn, 0, len(history))
	for i := len(history) - 1; i >= 0 && len(kept) < askMaxHistoryTurns; i-- {
		h := history[i]
		h.Question = clipAskText(h.Question, min(1024, remaining/2))
		h.Answer = clipAskText(h.Answer, min(2048, remaining-len(h.Question)))
		if h.Question == "" || h.Answer == "" {
			break
		}
		remaining -= len(h.Question) + len(h.Answer)
		kept = append(kept, h)
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	return kept
}

// For long documents select the window with most distinct query-term matches.
// This deterministic lexical heuristic is bounded; it is not an entailment or
// semantic relevance verifier. Missing matches retain the beginning.
//
// The returned askEvidenceMode records which of the three outcomes happened
// (askPromptEvidence's doc comment) — it is manifest bookkeeping for
// citation validation (ask_citation.go), not something that changes the
// excerpt text itself.
func askPassage(content, question string, budget int) (string, askEvidenceMode) {
	if len(content) <= budget {
		return content, askEvidenceModeFull
	}
	terms := strings.FieldsFunc(strings.ToLower(question), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	runes := []rune(content)
	bestStart, bestScore := 0, 0
	windowRunes := max(1, (budget-2*len(excerptMarker))/utf8.UTFMax)
	for start := 0; start < len(runes); start += max(1, windowRunes/2) {
		window := strings.ToLower(string(runes[start:min(start+windowRunes, len(runes))]))
		score := 0
		seen := map[string]bool{}
		for _, term := range terms {
			if len([]rune(term)) >= 2 && !seen[term] && strings.Contains(window, term) {
				score++
				seen[term] = true
			}
		}
		if score > bestScore {
			bestScore = score
			bestStart = start
		}
	}
	// Prefix marker makes an omitted beginning explicit as well.
	prefix := ""
	mode := askEvidenceModeHead
	if bestStart > 0 {
		prefix = evidencePrefixMarker
		mode = askEvidenceModeLexicalWindow
	}
	return prefix + clipAskText(string(runes[bestStart:]), budget-len(prefix)), mode
}

// isChunkOnlyResult reports whether r's Content already IS a matched chunk's
// own text rather than the parent document's full body — see
// model.MatchTypeChunkVector's doc comment. Such a result needs no
// evidence-location step: Content is already the passage.
func isChunkOnlyResult(r *model.SearchResult) bool {
	return r.MatchType == model.MatchTypeChunkVector || r.MatchType == model.MatchTypeChunkFTS
}

// evidenceSpan is a byte range located inside a document-lane primary's
// Content that a model.MatchedEvidence's Text was found at.
type evidenceSpan struct {
	start, end int
	score      float64
}

// locateEvidenceSpans finds every evidence entry whose Text is a literal
// substring of content, merges overlapping/adjacent hits into single spans,
// and returns them sorted by position. Entries whose Text is empty or not
// found are silently skipped — #267 (deep-plan finding F3) forbids
// fabricating a location for text that is not verifiably there.
func locateEvidenceSpans(content string, evidence []model.MatchedEvidence) []evidenceSpan {
	var spans []evidenceSpan
	for _, ev := range evidence {
		if ev.Text == "" {
			continue
		}
		idx := strings.Index(content, ev.Text)
		if idx < 0 {
			continue
		}
		spans = append(spans, evidenceSpan{start: idx, end: idx + len(ev.Text), score: ev.Score})
	}
	if len(spans) == 0 {
		return nil
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	merged := spans[:1]
	for _, s := range spans[1:] {
		last := &merged[len(merged)-1]
		if s.start > last.end {
			merged = append(merged, s)
			continue
		}
		// Overlapping or touching: dedupe into one span (#267 "dedupe
		// overlaps across multiple evidences").
		if s.end > last.end {
			last.end = s.end
		}
		if s.score > last.score {
			last.score = s.score
		}
	}
	return merged
}

// bestEvidenceByScore returns a pointer to the highest-scoring entry in
// evidence, or nil for an empty slice.
func bestEvidenceByScore(evidence []model.MatchedEvidence) *model.MatchedEvidence {
	if len(evidence) == 0 {
		return nil
	}
	best := &evidence[0]
	for i := 1; i < len(evidence); i++ {
		if evidence[i].Score > best.Score {
			best = &evidence[i]
		}
	}
	return best
}

// windowAround clips content to a budget-sized window straddling [start,
// end), preferring to centre the window on the match. Prefix/suffix markers
// make an omitted surrounding region explicit, matching askPassage's
// "[앞부분 생략]" convention.
//
// Marker bytes are reserved out of budget BEFORE the window itself is sized
// (evidencePrefixMarker/evidenceSuffixMarker's doc comment) — the window
// plus whichever markers end up attached is therefore guaranteed to fit
// budget without ever needing to trim afterward. This matters because
// [start, end) is always included whole once it is inside the window (see
// the invariant argued below), so trimming after the fact — as a prior
// version did via clipAskText's end-trim safety net — would delete part of
// the evidence itself whenever it happened to sit at the window's tail
// (#267 follow-up: a call transcript's answer-bearing sentence was often
// its very last sentence).
func windowAround(content string, start, end, budget int) string {
	if budget <= 0 {
		return ""
	}
	if len(content) <= budget {
		// Whole document already fits: no window, no markers needed.
		return content
	}
	matchLen := end - start
	if matchLen >= budget {
		// The evidence alone doesn't fit; there is no room left for markers
		// either. Defined behaviour: return as much of the evidence as
		// budget allows, starting at its own beginning.
		return clipAskText(content[start:], budget)
	}

	reserveLeft, reserveRight := len(evidencePrefixMarker), len(evidenceSuffixMarker)
	contentBudget := budget - reserveLeft - reserveRight
	if contentBudget < matchLen {
		// Not enough room for both markers AND the full span: drop the
		// markers rather than trim the evidence they would otherwise
		// annotate.
		contentBudget, reserveLeft, reserveRight = matchLen, 0, 0
	}

	// left <= start <= end <= right by construction: extra is the total
	// slack beyond the match, split (roughly) evenly on each side, then
	// clamped to content's bounds — clamping one side can only push the
	// other further toward the match, never past it, since contentBudget >=
	// matchLen throughout.
	extra := contentBudget - matchLen
	left := start - extra/2
	if left < 0 {
		left = 0
	}
	right := left + contentBudget
	if right > len(content) {
		right = len(content)
		left = right - contentBudget
		if left < 0 {
			left = 0
		}
	}
	// [start, end) is always a valid rune-boundary pair — it came from
	// strings.Index over valid UTF-8 text — so shrinking (never growing)
	// toward the nearest valid boundary can only narrow the non-evidence
	// context on either side; it can never eat into the evidence span
	// itself, and it can never push the window's byte length back over
	// contentBudget the way growing outward would have.
	for left < start && !utf8.RuneStart(content[left]) {
		left++
	}
	for right > end && right < len(content) && !utf8.RuneStart(content[right]) {
		right--
	}

	prefix := ""
	if reserveLeft > 0 && left > 0 {
		prefix = evidencePrefixMarker
	}
	suffix := ""
	if reserveRight > 0 && right < len(content) {
		suffix = evidenceSuffixMarker
	}
	return prefix + content[left:right] + suffix
}

// evidencePassage builds a document-lane primary's excerpt from its fused
// chunk Evidence (#267): the answer-bearing passage a chunk lane actually
// matched, placed first, instead of always falling back to content's
// beginning. Returns ("", "") when evidence exists but carries no usable
// text at all, signalling the caller to fall back to askPassage's lexical
// heuristic.
//
// content is the FULL document body (never Content from a chunk-only
// result — see isChunkOnlyResult, handled separately by the caller).
func evidencePassage(content string, evidence []model.MatchedEvidence, budget int) (string, askEvidenceMode) {
	if spans := locateEvidenceSpans(content, evidence); len(spans) > 0 {
		best := spans[0]
		for _, s := range spans[1:] {
			if s.score > best.score {
				best = s
			}
		}
		return windowAround(content, best.start, best.end, budget), askEvidenceModeMatchedChunk
	}
	// Not locatable in content: use the chunk's own text directly rather
	// than guessing an offset (deep-plan #267 finding F3 / risk table).
	if best := bestEvidenceByScore(evidence); best != nil && best.Text != "" {
		return clipAskText(best.Text, budget-len(unlocatedEvidenceMarker)) + unlocatedEvidenceMarker, askEvidenceModeMatchedChunk
	}
	return "", ""
}

// evidenceChunkIDs collects the ChunkID of every entry in evidence, in
// order, for askPromptEvidence.ChunkIDs manifest bookkeeping.
func evidenceChunkIDs(evidence []model.MatchedEvidence) []int64 {
	if len(evidence) == 0 {
		return nil
	}
	ids := make([]int64, len(evidence))
	for i, ev := range evidence {
		ids[i] = ev.ChunkID
	}
	return ids
}

// buildBudgetedAskMessages assembles Stage 3's prompt AND the
// askPromptManifest recording exactly which documents (and, for the ones
// that fit, which excerpt mode) actually made it into that prompt. The
// manifest is citation validation's allow-list (ask_citation.go) — every
// askPromptEvidence entry below is appended at the SAME point the excerpt is
// written into the message builder, and the budget-exceeded break below
// records everything it did NOT reach as Omitted, so the two never drift
// apart.
//
// question is the user-facing wording: it is clipped and placed as the
// final turn Stage 3 answers, and it is what gets persisted (saveAskTurn).
// excerptQuery is used ONLY to drive askPassage's lexical fallback window
// (#267 deep-plan finding F12/§5): a Korean 지시어 follow-up like "그건 언제로
// 정했지?" carries almost no searchable vocabulary of its own, so excerpt
// selection needs the SAME standalone (rewritten) question Stage 2 retrieval
// already searched with — see askHandler's rewriteStandaloneQuestion call.
// When a document instead has model.SearchResult.Evidence populated,
// neither question nor excerptQuery drives excerpt selection at all: the
// passage is the chunk lane's own matched location (evidencePassage below).
func buildBudgetedAskMessages(question, excerptQuery string, result RetrievalResult, history []askHistoryTurn) ([]llm.Message, askPromptManifest) {
	result = selectAskEvidence(result)
	question = clipAskText(question, askQuestionBytes)
	if excerptQuery == "" {
		excerptQuery = question
	}
	messages := make([]llm.Message, 0, len(history)*2+2)
	remaining := askMessageBytes - len(question)
	for _, h := range budgetAskHistory(history) {
		messages = append(messages, llm.Message{Role: "user", Content: h.Question}, llm.Message{Role: "assistant", Content: h.Answer})
		remaining -= len(h.Question) + len(h.Answer)
	}
	var manifest askPromptManifest
	var b strings.Builder
	b.WriteString("[관측된 사실]\n[입력 예산 내 선택한 근거이며 전체 자료가 아닙니다]\n")
	if len(result.Observed) == 0 {
		b.WriteString("(없음)\n")
	}
	count := len(result.Observed) + len(result.Inferred)
	// Reserve section/omission overhead; share evidence space across sources.
	perDoc := min(askExcerptBytes, max(0, (remaining-512)/max(1, count)))
	appendDocs := func(docs []*model.SearchResult, layer askEvidenceLayer) {
		for i, r := range docs {
			header := fmt.Sprintf("- [근거 ID: %s](/documents/%s) (%s) %s [발생: %s]: ", r.Document.ID, r.Document.ID, r.Document.SourceType, clipAskText(r.Document.Title, 256), formatOccurredAt(r.Document.OccurredAt))
			if perDoc <= len(header)+len(excerptMarker) || b.Len()+perDoc > remaining-128 {
				b.WriteString("[입력 예산으로 추가 문서 생략]\n")
				for _, omitted := range docs[i:] {
					manifest.Omitted = append(manifest.Omitted, omitted.Document.ID)
				}
				break
			}
			b.WriteString(header)
			passageBudget := perDoc - len(header) - 1
			// #267: prefer the passage a chunk lane actually matched over
			// always falling back to the document's beginning.
			var passage string
			var mode askEvidenceMode
			var chunkIDs []int64
			switch {
			case isChunkOnlyResult(r):
				passage, mode = clipAskText(r.Document.Content, passageBudget), askEvidenceModeChunkOnly
				chunkIDs = evidenceChunkIDs(r.Evidence)
			case len(r.Evidence) > 0:
				passage, mode = evidencePassage(r.Document.Content, r.Evidence, passageBudget)
				if passage != "" {
					chunkIDs = evidenceChunkIDs(r.Evidence)
				}
			}
			if passage == "" {
				passage, mode = askPassage(r.Document.Content, excerptQuery, passageBudget)
			}
			b.WriteString(passage)
			b.WriteByte('\n')
			manifest.Evidence = append(manifest.Evidence, askPromptEvidence{
				ID:       r.Document.ID,
				Layer:    layer,
				Mode:     mode,
				Bytes:    len(passage),
				ChunkIDs: chunkIDs,
			})
		}
	}
	appendDocs(result.Observed, askEvidenceObserved)
	if len(result.Inferred) > 0 {
		b.WriteString("\n[추론 — 가설이며 사실로 인용 불가]\n")
		appendDocs(result.Inferred, askEvidenceInferred)
	}
	messages = append(messages, llm.Message{Role: "user", Content: b.String()}, llm.Message{Role: "user", Content: question})
	return messages, manifest
}

// Bound source count as well as bytes so every advertised source receives a
// meaningful excerpt. The default eight observations plus three insights fit.
func selectAskEvidence(result RetrievalResult) RetrievalResult {
	const maxSources = 12
	result.Observed = result.Observed[:min(len(result.Observed), maxSources)]
	result.Inferred = result.Inferred[:min(len(result.Inferred), maxSources-len(result.Observed))]
	return result
}
