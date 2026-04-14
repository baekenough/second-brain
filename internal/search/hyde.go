package search

import (
	"context"
	"log/slog"

	"github.com/baekenough/second-brain/internal/llm"
)

const hydeSystemPrompt = `당신은 사용자 질문에 대한 가상 답변을 작성합니다. 실제 근거 없이도 가능한 한 구체적인 팩트와 키워드를 포함한 답변을 3-4문장으로 작성하세요. 이 가상 답변은 검색 시스템의 recall 향상에만 사용되며, 사용자에게 직접 보여지지 않습니다.

원칙:
- 질문과 관련된 고유명사·기술 용어·숫자·인명을 최대한 포함
- 확신하는 톤으로 작성 (단, 실제 답변이 아님을 알고 있음)
- 3-4문장
- 메타 발화 금지 ("저는 AI입니다" 같은 문구 금지)`

// Expand returns a hypothetical document embedding query by asking the LLM to
// generate a plausible answer for the query. The generated answer is appended
// to the original query so that both the original keywords and the hypothetical
// document's vocabulary are used for retrieval.
//
// It is safe to call with a nil client — the original query is returned unchanged.
// When the LLM call fails or returns an empty string, the original query is
// returned unchanged and no error is propagated.
func Expand(ctx context.Context, client llm.Completer, query string) string {
	if client == nil || !client.Enabled() {
		return query
	}

	messages := []llm.Message{
		{Role: "user", Content: query},
	}

	expanded, err := client.CompleteWithMessages(ctx, hydeSystemPrompt, messages)
	if err != nil {
		slog.Warn("hyde: expansion failed, using original query", "err", err)
		return query
	}
	if expanded == "" {
		return query
	}

	slog.Debug("hyde: query expanded",
		"original_len", len(query),
		"expanded_len", len(expanded),
	)

	// Combine the original query with the hypothetical document so that FTS
	// matches both the user's own keywords and the LLM-generated vocabulary.
	return query + "\n\n" + expanded
}
