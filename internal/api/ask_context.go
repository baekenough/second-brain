package api

import (
	"fmt"
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
func askPassage(content, question string, budget int) string {
	if len(content) <= budget {
		return content
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
	if bestStart > 0 {
		prefix = "[앞부분 생략] "
	}
	return prefix + clipAskText(string(runes[bestStart:]), budget-len(prefix))
}

func buildBudgetedAskMessages(question string, result RetrievalResult, history []askHistoryTurn) []llm.Message {
	result = selectAskEvidence(result)
	question = clipAskText(question, askQuestionBytes)
	messages := make([]llm.Message, 0, len(history)*2+2)
	remaining := askMessageBytes - len(question)
	for _, h := range budgetAskHistory(history) {
		messages = append(messages, llm.Message{Role: "user", Content: h.Question}, llm.Message{Role: "assistant", Content: h.Answer})
		remaining -= len(h.Question) + len(h.Answer)
	}
	var b strings.Builder
	b.WriteString("[관측된 사실]\n[입력 예산 내 선택한 근거이며 전체 자료가 아닙니다]\n")
	if len(result.Observed) == 0 {
		b.WriteString("(없음)\n")
	}
	count := len(result.Observed) + len(result.Inferred)
	// Reserve section/omission overhead; share evidence space across sources.
	perDoc := min(askExcerptBytes, max(0, (remaining-512)/max(1, count)))
	appendDocs := func(docs []*model.SearchResult) {
		for _, r := range docs {
			header := fmt.Sprintf("- [근거 ID: %s](/documents/%s) (%s) %s [발생: %s]: ", r.Document.ID, r.Document.ID, r.Document.SourceType, clipAskText(r.Document.Title, 256), formatOccurredAt(r.Document.OccurredAt))
			if perDoc <= len(header)+len(excerptMarker) || b.Len()+perDoc > remaining-128 {
				b.WriteString("[입력 예산으로 추가 문서 생략]\n")
				break
			}
			b.WriteString(header)
			b.WriteString(askPassage(r.Document.Content, question, perDoc-len(header)-1))
			b.WriteByte('\n')
		}
	}
	appendDocs(result.Observed)
	if len(result.Inferred) > 0 {
		b.WriteString("\n[추론 — 가설이며 사실로 인용 불가]\n")
		appendDocs(result.Inferred)
	}
	messages = append(messages, llm.Message{Role: "user", Content: b.String()}, llm.Message{Role: "user", Content: question})
	return messages
}

// Bound source count as well as bytes so every advertised source receives a
// meaningful excerpt. The default eight observations plus three insights fit.
func selectAskEvidence(result RetrievalResult) RetrievalResult {
	const maxSources = 12
	result.Observed = result.Observed[:min(len(result.Observed), maxSources)]
	result.Inferred = result.Inferred[:min(len(result.Inferred), maxSources-len(result.Observed))]
	return result
}
