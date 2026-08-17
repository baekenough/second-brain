package api

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/baekenough/second-brain/internal/llm"
)

// askRewriteSystemPrompt instructs the model to compress a capped
// conversation history plus the user's latest (possibly context-dependent)
// question into ONE standalone question a document search can run against
// without seeing any prior turn.
//
// This exists because retrieval (assembleRetrieval) and intent
// classification (intent.Classify) only ever see a single question string
// — a follow-up like "그거 더 자세히" or "그럼 그 사람은?" carries no
// searchable content on its own, so it must be rewritten BEFORE it reaches
// Stage 1/2. The rewritten text is used ONLY for search/classification;
// Stage 3 synthesis still shows the model the user's original wording
// (buildAskMessages' question param, sourced from req.Question in
// askHandler) so the visible answer reads naturally, not as a restatement
// of an artificially-expanded question.
const askRewriteSystemPrompt = `당신은 대화형 검색 시스템의 질의 재작성기입니다. 아래 [이전 대화]와 사용자의 [현재 질문]을 보고, 이전 대화 없이도 독립적으로 이해되는 검색용 질문 하나를 작성하세요.

규칙:
1. 현재 질문이 가리키는 대상(대명사, 생략된 주어/목적어, "그거"/"그 사람"/"거기" 등)을 이전 대화에서 찾아 명시적으로 채워 넣으세요.
2. 이전 대화에 없는 새로운 정보를 추가하거나 질문의 의도를 바꾸지 마세요. 압축·명확화만 하세요.
3. 결과는 재작성된 질문 한 문장만 출력하세요. 설명, 따옴표, 접두사를 붙이지 마세요.`

// buildAskRewritePrompt renders the user-turn content for the rewrite call:
// the capped history (question/answer pairs, oldest first, matching
// askMaxHistoryTurns) followed by the current question.
func buildAskRewritePrompt(history []askHistoryTurn, question string) string {
	var b strings.Builder
	b.WriteString("[이전 대화]\n")
	for i, h := range history {
		fmt.Fprintf(&b, "Q%d: %s\nA%d: %s\n", i+1, h.Question, i+1, h.Answer)
	}
	fmt.Fprintf(&b, "\n[현재 질문]\n%s", question)
	return b.String()
}

// rewriteStandaloneQuestion asks the LLM to compress history+question into
// one standalone search query.
//
// Returns (question, false) unchanged — i.e. skips rewriting entirely —
// whenever a rewrite cannot be trusted to be an improvement:
//   - no history (nothing to resolve — this is the conversation's first
//     turn; requirement (a) of the multi-turn ask feature)
//   - the LLM is not configured (Enabled() == false)
//   - the rewrite call errors
//   - the model returns an empty/whitespace-only response
//
// This "fail open" contract means a rewrite failure can never make search
// WORSE than not rewriting at all — the caller (askHandler) always has a
// usable search query, and a rewrite error is logged but never surfaces to
// the client (spec: "재작성 실패 시 원문으로 폴백. 에러로 전체를 죽이지 마세요").
func (s *Server) rewriteStandaloneQuestion(ctx context.Context, history []askHistoryTurn, question string) (rewritten string, ok bool) {
	if len(history) == 0 || !s.llmClient.Enabled() {
		return question, false
	}

	prompt := buildAskRewritePrompt(history, question)
	result, err := s.llmClient.CompleteWithMessages(ctx, askRewriteSystemPrompt, []llm.Message{
		{Role: "user", Content: prompt},
	})
	if err != nil {
		slog.Warn("ask: query rewrite failed, falling back to raw question", "error", err)
		return question, false
	}

	result = strings.TrimSpace(result)
	if result == "" {
		slog.Warn("ask: query rewrite returned empty response, falling back to raw question")
		return question, false
	}
	return result, true
}
