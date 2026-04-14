// export_test.go exposes internal package-level functions for black-box tests
// in the collector_test package. This file is compiled only during testing.
package collector

import (
	"context"

	"github.com/bwmarrin/discordgo"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
)

// ExportBuildContextBlock exposes buildContextBlock for testing.
func ExportBuildContextBlock(results []*model.SearchResult) string {
	return buildContextBlock(results)
}

// ExportStripMention exposes stripMention for testing.
func ExportStripMention(content, botID string) string {
	return stripMention(content, botID)
}

// ExportSplitForDiscord exposes splitForDiscord for testing.
func ExportSplitForDiscord(text string, maxLen int) []string {
	return splitForDiscord(text, maxLen)
}

// TestInputMessage is the input descriptor used by ExportBuildConversationHistory.
// It avoids importing discordgo in the test file.
type TestInputMessage struct {
	AuthorID string
	Content  string
}

// ExportBuildConversationHistory wraps buildConversationHistory for testing.
// msgs is a slice of TestInputMessage so callers do not depend on discordgo.
func ExportBuildConversationHistory(msgs []TestInputMessage, botID string) []llm.Message {
	dmsgs := make([]*discordgo.Message, len(msgs))
	for i, m := range msgs {
		dmsgs[i] = &discordgo.Message{
			Author:  &discordgo.User{ID: m.AuthorID, Username: "user-" + m.AuthorID},
			Content: m.Content,
		}
	}
	return buildConversationHistory(dmsgs, botID)
}

// ExportBuildReply exercises the core RAG pipeline of buildReply without
// requiring a live Discord session. It calls the internal logic directly after
// stripping the discordgo.Session dependency (session is only used for typing
// indicator and history fetch — both optional / non-fatal).
//
// searcher and completer are injected interfaces.
// query is the already-cleaned user question (mention already stripped).
//
// The function creates a minimal DiscordGateway and calls the testable parts
// of buildReply's logic: search → buildContextBlock → buildConversationHistory
// → CompleteWithMessages.
func ExportBuildReply(
	ctx context.Context,
	searcher Searcher,
	completer llm.Completer,
	botID string,
	query string,
) string {
	// Search (non-fatal on error).
	results, err := searcher.Search(ctx, model.SearchQuery{
		Query:          query,
		Limit:          10,
		IncludeDeleted: false,
	})
	if err != nil {
		results = nil
	}

	// Build RAG context block.
	contextBlock := buildContextBlock(results)

	// Build conversation turn (no prior history in unit test scope).
	var currentTurn string
	if contextBlock == "" {
		currentTurn = "질문: " + query
	} else {
		currentTurn = "참고 가능한 내부 문서:\n" + contextBlock + "\n\n질문: " + query
	}

	msgs := []llm.Message{{Role: "user", Content: currentTurn}}
	answer, err := completer.CompleteWithMessages(ctx, discordSystemPrompt, msgs)
	if err != nil {
		return "답변 생성 중 오류가 발생했어요. 잠시 후 다시 시도해주세요."
	}
	return answer
}
