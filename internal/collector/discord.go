// Package collector provides source-specific document collectors.
// discord.go implements the Discord collector using the discordgo library.
//
// Security note: DM and GroupDM channels are NEVER collected. Only the
// following channel types are allowed (positive-list approach):
//   - GuildText   (ChannelTypeGuildText)
//   - GuildPublicThread (ChannelTypeGuildPublicThread)
//   - GuildPrivateThread (ChannelTypeGuildPrivateThread)
//
// This file covers:
//  1. Periodic REST-based collection (full backfill on first run, incremental after)
//  2. Mention-response via WebSocket gateway (always-on, separate from cron)
package collector

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
)

// DiscordCollector collects messages from Discord guild channels using the
// discordgo library. It respects discordgo's built-in rate-limit handler.
type DiscordCollector struct {
	botToken              string
	applicationID         string
	guildIDs              []string
	mentionResponseEnabled bool

	// session is the discordgo session used for REST API calls.
	// It is created lazily on the first Collect call and reused.
	session *discordgo.Session
}

// NewDiscordCollector returns a DiscordCollector configured from the given
// parameters. When botToken or guildIDs is empty the collector is disabled and
// Collect will not be called by the scheduler.
func NewDiscordCollector(
	botToken string,
	applicationID string,
	guildIDs []string,
	mentionResponseEnabled bool,
) *DiscordCollector {
	return &DiscordCollector{
		botToken:              botToken,
		applicationID:         applicationID,
		guildIDs:              guildIDs,
		mentionResponseEnabled: mentionResponseEnabled,
	}
}

func (c *DiscordCollector) Name() string             { return "discord" }
func (c *DiscordCollector) Source() model.SourceType { return model.SourceDiscord }

// Enabled reports whether the collector has the minimum required configuration.
func (c *DiscordCollector) Enabled() bool {
	return c.botToken != "" && len(c.guildIDs) > 0
}

// Collect fetches messages from all allowed text channels in the configured
// guilds that were created or updated since the given time.
// On the first run (since.IsZero()) it performs a full backfill.
// Thread messages (public + archived public threads) are also collected.
func (c *DiscordCollector) Collect(ctx context.Context, since time.Time) ([]model.Document, error) {
	sess, err := c.getSession()
	if err != nil {
		return nil, fmt.Errorf("discord: create session: %w", err)
	}

	var allDocs []model.Document
	for _, guildID := range c.guildIDs {
		guildDocs, err := c.collectGuild(ctx, sess, guildID, since)
		if err != nil {
			slog.Warn("discord: failed to collect guild",
				"guild_id", guildID, "error", err)
			continue
		}
		allDocs = append(allDocs, guildDocs...)
	}

	slog.Info("discord: collected documents", "count", len(allDocs))
	return allDocs, nil
}

// collectGuild collects all allowed text channels and their threads within
// a single guild.
func (c *DiscordCollector) collectGuild(
	ctx context.Context,
	sess *discordgo.Session,
	guildID string,
	since time.Time,
) ([]model.Document, error) {
	channels, err := sess.GuildChannels(guildID)
	if err != nil {
		return nil, fmt.Errorf("list channels for guild %s: %w", guildID, err)
	}

	var docs []model.Document
	for _, ch := range channels {
		// Positive-list guard: only collect guild text channels.
		// DM and GroupDM channels are never present in GuildChannels, but we
		// explicitly allow only the three safe types to guard against future
		// Discord API additions.
		if !isAllowedChannelType(ch.Type) {
			continue
		}

		chDocs, err := c.collectChannel(ctx, sess, ch, "", "", since)
		if err != nil {
			slog.Warn("discord: failed to collect channel",
				"guild_id", guildID,
				"channel_id", ch.ID,
				"channel_name", ch.Name,
				"error", err)
			continue
		}
		docs = append(docs, chDocs...)

		// Collect active public threads within this text channel.
		threadDocs, err := c.collectThreads(ctx, sess, ch, since)
		if err != nil {
			slog.Warn("discord: failed to collect threads",
				"guild_id", guildID,
				"channel_id", ch.ID,
				"error", err)
			continue
		}
		docs = append(docs, threadDocs...)
	}

	return docs, nil
}

// collectThreads fetches active and archived public threads for a parent channel,
// then collects messages from each.
func (c *DiscordCollector) collectThreads(
	ctx context.Context,
	sess *discordgo.Session,
	parentCh *discordgo.Channel,
	since time.Time,
) ([]model.Document, error) {
	// GuildChannels already includes sub-channels (threads) when the bot has
	// the GUILD_MESSAGES intent. We can also use GuildThreadsActive for live
	// threads. Use GuildThreadsActive for active threads and
	// ThreadsArchived for archived public threads.

	var docs []model.Document

	// --- Active threads ---
	activeThreads, err := sess.GuildThreadsActive(parentCh.GuildID)
	if err != nil {
		slog.Warn("discord: failed to fetch active threads",
			"parent_channel_id", parentCh.ID, "error", err)
		// Non-fatal: continue to archived threads.
	} else {
		for _, th := range activeThreads.Threads {
			if th.ParentID != parentCh.ID {
				continue
			}
			if !isAllowedChannelType(th.Type) {
				continue
			}
			thDocs, err := c.collectChannel(ctx, sess, th, parentCh.ID, parentCh.Name, since)
			if err != nil {
				slog.Warn("discord: failed to collect active thread",
					"thread_id", th.ID, "thread_name", th.Name, "error", err)
				continue
			}
			docs = append(docs, thDocs...)
		}
	}

	// --- Archived public threads ---
	var archivedBefore string
	for {
		archived, err := sess.ThreadsArchived(parentCh.ID, nil, 100)
		if err != nil {
			slog.Warn("discord: failed to fetch archived threads",
				"parent_channel_id", parentCh.ID, "error", err)
			break
		}
		_ = archivedBefore // used for pagination cursor if needed in future
		for _, th := range archived.Threads {
			if !isAllowedChannelType(th.Type) {
				continue
			}
			thDocs, err := c.collectChannel(ctx, sess, th, parentCh.ID, parentCh.Name, since)
			if err != nil {
				slog.Warn("discord: failed to collect archived thread",
					"thread_id", th.ID, "thread_name", th.Name, "error", err)
				continue
			}
			docs = append(docs, thDocs...)
		}
		if !archived.HasMore {
			break
		}
		// Advance cursor to last thread's archive timestamp.
		if len(archived.Threads) > 0 {
			last := archived.Threads[len(archived.Threads)-1]
			if last.ThreadMetadata != nil {
				archivedBefore = last.ThreadMetadata.ArchiveTimestamp.Format(time.RFC3339)
			}
		}
		break // discordgo ThreadsArchived does not yet expose the before cursor param — stop after first page
	}

	return docs, nil
}

// collectChannel fetches messages from a single channel (or thread) and converts
// them to model.Document values. parentChannelID and parentChannelName are only
// meaningful for threads; pass empty strings for top-level channels.
func (c *DiscordCollector) collectChannel(
	ctx context.Context,
	sess *discordgo.Session,
	ch *discordgo.Channel,
	parentChannelID string,
	parentChannelName string,
	since time.Time,
) ([]model.Document, error) {
	msgs, err := c.fetchMessages(ctx, sess, ch.ID, since)
	if err != nil {
		return nil, fmt.Errorf("fetch messages for channel %s: %w", ch.ID, err)
	}

	isThread := ch.Type == discordgo.ChannelTypeGuildPublicThread ||
		ch.Type == discordgo.ChannelTypeGuildPrivateThread

	guildID := ch.GuildID

	var docs []model.Document
	for _, m := range msgs {
		if m.Content == "" {
			continue
		}

		threadID := ""
		threadName := ""
		channelID := ch.ID
		channelName := ch.Name

		if isThread {
			threadID = ch.ID
			threadName = ch.Name
			if parentChannelID != "" {
				channelID = parentChannelID
				channelName = parentChannelName
			}
		}

		sourceID := fmt.Sprintf("discord:%s:%s:%s", guildID, channelID, m.ID)
		if isThread {
			sourceID = fmt.Sprintf("discord:%s:%s:%s:%s", guildID, channelID, threadID, m.ID)
		}

		// Derive title: use channel/thread name + short timestamp prefix.
		title := fmt.Sprintf("#%s — %s", channelName, m.Timestamp.Format("2006-01-02"))
		if isThread {
			title = fmt.Sprintf("#%s > %s — %s", channelName, threadName, m.Timestamp.Format("2006-01-02"))
		}

		metadata := map[string]any{
			"guild_id":     guildID,
			"channel_id":   channelID,
			"channel_name": channelName,
			"author_id":    m.Author.ID,
			"author_name":  m.Author.Username,
		}
		if isThread {
			metadata["thread_id"] = threadID
			metadata["thread_name"] = threadName
		}

		docs = append(docs, model.Document{
			ID:          uuid.New(),
			SourceType:  model.SourceDiscord,
			SourceID:    sourceID,
			Title:       title,
			Content:     m.Content,
			Metadata:    metadata,
			CollectedAt: time.Now().UTC(),
		})
	}

	if len(msgs) > 0 {
		slog.Info("discord: collected channel",
			"channel_id", ch.ID,
			"channel_name", ch.Name,
			"messages", len(msgs),
			"docs", len(docs),
		)
	}

	return docs, nil
}

// fetchMessages paginates through all messages in a channel since the given
// time, using discordgo's built-in rate-limit handling. For incremental runs
// (since > zero) it uses the message snowflake ID derived from the timestamp.
func (c *DiscordCollector) fetchMessages(
	_ context.Context,
	sess *discordgo.Session,
	channelID string,
	since time.Time,
) ([]*discordgo.Message, error) {
	const pageSize = 100

	// Convert `since` to a Discord snowflake ID so we can use the `after`
	// pagination parameter. Discord snowflake epoch starts 2015-01-01.
	afterID := timeToSnowflake(since)

	var all []*discordgo.Message
	lastID := afterID

	for {
		msgs, err := sess.ChannelMessages(channelID, pageSize, "", lastID, "")
		if err != nil {
			return nil, fmt.Errorf("channel messages %s: %w", channelID, err)
		}
		if len(msgs) == 0 {
			break
		}

		all = append(all, msgs...)

		if len(msgs) < pageSize {
			break
		}
		// Advance cursor to the last message ID (messages are returned in
		// ascending order when using `after`).
		lastID = msgs[len(msgs)-1].ID

		slog.Info("discord: backfill progress",
			"channel_id", channelID,
			"fetched_so_far", len(all),
		)
	}

	return all, nil
}

// --- Gateway (WebSocket) for mention responses ---

// Searcher is the interface the gateway uses to query the knowledge base.
// It is satisfied by *search.Service, but expressed here as an interface to
// keep the collector package free of a direct search import.
type Searcher interface {
	Search(ctx context.Context, q model.SearchQuery) ([]*model.SearchResult, error)
}

// DiscordGateway holds the WebSocket session used for real-time mention responses.
// It is decoupled from the REST-based DiscordCollector to keep concerns separate.
//
// When both searcher and llmClient are non-nil, it performs full RAG:
//
//	mention → search top-K docs → LLM chat completion → Discord reply
//
// When either dependency is nil it falls back to the legacy static message.
type DiscordGateway struct {
	botToken               string
	mentionResponseEnabled bool
	session                *discordgo.Session

	searcher  Searcher
	llmClient *llm.Client
}

// NewDiscordGateway creates a gateway that connects to the Discord WebSocket and
// responds to bot mentions when enabled.
//
// searcher and llmClient may be nil — in that case the gateway uses the legacy
// static response. Inject both to enable the RAG flow.
func NewDiscordGateway(
	botToken string,
	mentionResponseEnabled bool,
	searcher Searcher,
	llmClient *llm.Client,
) *DiscordGateway {
	return &DiscordGateway{
		botToken:               botToken,
		mentionResponseEnabled: mentionResponseEnabled,
		searcher:               searcher,
		llmClient:              llmClient,
	}
}

// Enabled reports whether the gateway has the minimum required configuration.
func (g *DiscordGateway) Enabled() bool { return g.botToken != "" }

// Run opens the Discord WebSocket gateway and blocks until ctx is cancelled.
// On cancellation it performs a graceful close.
func (g *DiscordGateway) Run(ctx context.Context) {
	if !g.Enabled() {
		slog.Info("discord gateway: disabled (no bot token)")
		return
	}

	sess, err := discordgo.New("Bot " + g.botToken)
	if err != nil {
		slog.Error("discord gateway: failed to create session", "error", err)
		return
	}

	// We only need the message-read intent for mention detection.
	sess.Identify.Intents = discordgo.IntentsGuildMessages

	if g.mentionResponseEnabled {
		sess.AddHandler(g.handleMessageCreate)
	}

	if err := sess.Open(); err != nil {
		slog.Error("discord gateway: failed to open WebSocket", "error", err)
		return
	}
	g.session = sess

	slog.Info("discord gateway: WebSocket connected",
		"mention_response", g.mentionResponseEnabled)

	<-ctx.Done()

	slog.Info("discord gateway: shutting down WebSocket")
	if err := sess.Close(); err != nil {
		slog.Warn("discord gateway: error during close", "error", err)
	}
}

// handleMessageCreate is the discordgo event handler for MESSAGE_CREATE events.
// It replies to any message that mentions the bot.
//
// When RAG dependencies (searcher + llmClient) are available the response is
// generated by searching the knowledge base and calling the LLM.
// Otherwise a static informational message is returned.
func (g *DiscordGateway) handleMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	// Ignore messages from the bot itself to prevent echo loops.
	if m.Author.ID == s.State.User.ID {
		return
	}

	// Check if the bot is mentioned.
	botMentioned := false
	for _, user := range m.Mentions {
		if user.ID == s.State.User.ID {
			botMentioned = true
			break
		}
	}
	if !botMentioned {
		return
	}

	slog.Info("discord gateway: bot mentioned",
		"channel_id", m.ChannelID,
		"author", m.Author.Username,
	)

	// Use a background context derived from the gateway lifetime.
	// discordgo does not pass a context to event handlers; we use a
	// per-request timeout to avoid hanging the WebSocket read loop.
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	ref := m.Reference()
	reply := g.buildReply(ctx, s, m)

	for _, chunk := range splitForDiscord(reply, 1900) {
		if _, err := s.ChannelMessageSendReply(m.ChannelID, chunk, ref); err != nil {
			slog.Warn("discord gateway: failed to send reply chunk",
				"channel_id", m.ChannelID,
				"error", err,
			)
			// Attempt plain send as fallback (original message may have been deleted).
			if _, err2 := s.ChannelMessageSend(m.ChannelID, chunk); err2 != nil {
				slog.Warn("discord gateway: fallback send also failed",
					"channel_id", m.ChannelID,
					"error", err2,
				)
			}
		}
	}
}

// discordSystemPrompt is the system prompt injected into every LLM call.
// It instructs the model to be a helpful team AI that uses RAG context when
// available but always answers using general knowledge as a fallback.
const discordSystemPrompt = `당신은 second-brain 팀의 AI 어시스턴트입니다. 이 서버(Discord)에서 팀원의 질문에 답하며, 팀 지식베이스(Slack, Discord, GitHub 등 수집된 문서)를 RAG로 참조할 수 있습니다.

역할:
- 친근하고 유능한 팀 동료 AI. 대화를 자연스럽게 이어가며 실질적 도움을 드립니다.
- RAG 문서가 있으면 그것을 우선 참고해 답변하되, 부족한 부분은 일반 지식으로 자유롭게 보완합니다.
- 채널 최근 대화 맥락을 활용해 후속 질문·지시대명사("그거", "위에서 말한 것")를 이해합니다.

응답 원칙:
1. 항상 도움이 되는 답변을 먼저 제공합니다. "모른다", "정보 없음" 같은 회피는 정말 답이 불가능할 때만 최소한으로.
2. RAG 문서가 있으면 답변 말미에 "출처: [제목]" 1-2개 간결히 표기.
3. 추측해서 틀린 정보를 주는 것은 금지. 확실하지 않으면 "~일 가능성이 높습니다"처럼 명시.
4. 코드/명령어/설정은 코드 블록 사용.
5. 답변은 한국어로 2000자 이내. 구체적이고 실용적으로.
6. 질문이 모호하면 짐작해서 가장 그럴듯한 해석으로 답하되, 한 줄로 가정을 명시.`

// buildReply orchestrates the RAG pipeline and returns the text to send to Discord.
//
// Flow:
//  1. Extract query (strip bot mention)
//  2. Show typing indicator
//  3. Fetch channel history (last 20 messages, non-fatal on error)
//  4. Search knowledge base (limit=10, non-fatal on error)
//  5. Build context block from search results
//  6. Convert channel history to conversation turns
//  7. Append current user turn (with embedded RAG context if available)
//  8. Call CompleteWithMessages for multi-turn LLM completion
//  9. Return answer
//
// It falls back to a static message when dependencies are not configured.
func (g *DiscordGateway) buildReply(
	ctx context.Context,
	s *discordgo.Session,
	m *discordgo.MessageCreate,
) string {
	// Fall back to legacy static message when RAG is not configured.
	if g.searcher == nil || g.llmClient == nil || !g.llmClient.Enabled() {
		return "검색 시스템 온라인입니다. 질문은 /api/v1/search 엔드포인트를 사용해주세요."
	}

	// 1. Extract query — strip the bot mention token(s).
	query := stripMention(m.Content, s.State.User.ID)
	query = strings.TrimSpace(query)
	if query == "" {
		return "질문을 함께 입력해주세요. 예: `@봇 X에 대해 알려줘`"
	}

	// 2. Show typing indicator while we work.
	_ = s.ChannelTyping(m.ChannelID)

	// 3. Fetch channel history for conversation context (non-fatal).
	// beforeID = m.ID so we get messages sent before this one.
	history, err := s.ChannelMessages(m.ChannelID, 20, m.ID, "", "")
	if err != nil {
		slog.Warn("discord gateway: failed to fetch channel history",
			"channel_id", m.ChannelID, "error", err)
		history = nil // non-fatal — continue without conversation context
	}
	// ChannelMessages with beforeID returns messages in descending order
	// (newest first). Reverse to get chronological order for the LLM.
	for i, j := 0, len(history)-1; i < j; i, j = i+1, j-1 {
		history[i], history[j] = history[j], history[i]
	}

	// 4. Search knowledge base (best-effort; search is optional context, not a gate).
	const searchLimit = 10
	results, err := g.searcher.Search(ctx, model.SearchQuery{
		Query:          query,
		Limit:          searchLimit,
		IncludeDeleted: false,
	})
	if err != nil {
		slog.Warn("discord gateway: search failed, proceeding without context",
			"error", err, "query", query)
		results = nil
	}

	// 5. Build the RAG context block for the LLM prompt (may be empty).
	contextBlock := buildContextBlock(results)

	// 6. Convert channel history to ordered conversation turns.
	conversationMsgs := buildConversationHistory(history, s.State.User.ID)

	// 7. Append the current user turn.
	// Embed RAG context into the final user message when available so the LLM
	// sees it alongside the question without polluting the history turns.
	var currentTurn string
	if contextBlock == "" {
		currentTurn = "질문: " + query
	} else {
		currentTurn = fmt.Sprintf("참고 가능한 내부 문서:\n%s\n\n질문: %s", contextBlock, query)
	}
	conversationMsgs = append(conversationMsgs, llm.Message{Role: "user", Content: currentTurn})

	// 8. Call LLM with full conversation history.
	answer, err := g.llmClient.CompleteWithMessages(ctx, discordSystemPrompt, conversationMsgs)
	if err != nil {
		slog.Error("discord gateway: LLM completion failed", "error", err)
		return "답변 생성 중 오류가 발생했어요. 잠시 후 다시 시도해주세요."
	}

	return answer
}

// buildConversationHistory converts a slice of Discord messages (in chronological
// order) to a sequence of llm.Message turns suitable for multi-turn completion.
//
// Bot messages become "assistant" turns with content as-is.
// All other messages become "user" turns prefixed with the author username so
// the LLM can distinguish multiple participants.
// Empty messages are skipped.
func buildConversationHistory(history []*discordgo.Message, botID string) []llm.Message {
	msgs := make([]llm.Message, 0, len(history))
	for _, msg := range history {
		if msg.Content == "" {
			continue
		}
		if msg.Author.ID == botID {
			msgs = append(msgs, llm.Message{
				Role:    "assistant",
				Content: msg.Content,
			})
		} else {
			msgs = append(msgs, llm.Message{
				Role:    "user",
				Content: fmt.Sprintf("%s: %s", msg.Author.Username, msg.Content),
			})
		}
	}
	return msgs
}

// --- Helper functions ---

// stripMention removes all `<@ID>` and `<@!ID>` mention tokens for the given
// bot user ID from content and returns the cleaned string.
func stripMention(content, botID string) string {
	// Handles both <@ID> and <@!ID> (nickname mention) forms.
	content = strings.ReplaceAll(content, "<@"+botID+">", "")
	content = strings.ReplaceAll(content, "<@!"+botID+">", "")
	return content
}

// buildContextBlock formats a slice of search results into a numbered context
// block suitable for injection into an LLM prompt.
// Returns an empty string when results is nil or empty so the caller can
// branch between "context available" and "no context" user prompts.
// Total length is capped at 8000 characters to stay within token budgets.
func buildContextBlock(results []*model.SearchResult) string {
	if len(results) == 0 {
		return ""
	}

	const maxContextLen = 12000
	const maxSnippetLen = 800

	var sb strings.Builder
	for i, r := range results {
		snippet := r.Content
		if utf8.RuneCountInString(snippet) > maxSnippetLen {
			// Truncate at a rune boundary.
			runes := []rune(snippet)
			snippet = string(runes[:maxSnippetLen]) + "…"
		}

		// Derive a source link from metadata if available; fall back to SourceID.
		sourceLink := r.SourceID
		if url, ok := r.Metadata["url"].(string); ok && url != "" {
			sourceLink = url
		}

		block := fmt.Sprintf("[%d] %s\n%s\n출처: %s\n---\n", i+1, r.Title, snippet, sourceLink)
		if sb.Len()+len(block) > maxContextLen {
			break
		}
		sb.WriteString(block)
	}
	return sb.String()
}

// splitForDiscord splits text into chunks of at most maxLen runes, breaking on
// paragraph boundaries (\n\n) when possible and on word boundaries otherwise.
// This ensures each Discord message respects the 2000-character limit.
func splitForDiscord(text string, maxLen int) []string {
	if utf8.RuneCountInString(text) <= maxLen {
		return []string{text}
	}

	var chunks []string
	for utf8.RuneCountInString(text) > maxLen {
		// Try to break on a paragraph boundary within the allowed length.
		runes := []rune(text)
		candidate := string(runes[:maxLen])

		breakAt := strings.LastIndex(candidate, "\n\n")
		if breakAt > 0 {
			chunks = append(chunks, strings.TrimSpace(string(runes[:breakAt])))
			text = strings.TrimSpace(string(runes[breakAt:]))
			continue
		}

		// Fall back to word boundary (last space).
		breakAt = strings.LastIndex(candidate, " ")
		if breakAt > 0 {
			chunks = append(chunks, strings.TrimSpace(string(runes[:breakAt])))
			text = strings.TrimSpace(string(runes[breakAt:]))
			continue
		}

		// No boundary found — hard-split at maxLen.
		chunks = append(chunks, candidate)
		text = strings.TrimSpace(string(runes[maxLen:]))
	}

	if text != "" {
		chunks = append(chunks, text)
	}
	return chunks
}

// --- Helpers ---

// isAllowedChannelType returns true only for channel types that represent
// guild text channels or guild public/private threads. This positive-list
// approach ensures DM and GroupDM channels are never collected even if the
// Discord API returns them unexpectedly.
func isAllowedChannelType(t discordgo.ChannelType) bool {
	switch t {
	case discordgo.ChannelTypeGuildText,
		discordgo.ChannelTypeGuildPublicThread,
		discordgo.ChannelTypeGuildPrivateThread:
		return true
	default:
		return false
	}
}

// getSession returns the cached discordgo.Session, creating it lazily if needed.
// Only a REST session (no WebSocket) is created here; the gateway is managed
// separately by DiscordGateway.
func (c *DiscordCollector) getSession() (*discordgo.Session, error) {
	if c.session != nil {
		return c.session, nil
	}
	sess, err := discordgo.New("Bot " + c.botToken)
	if err != nil {
		return nil, err
	}
	c.session = sess
	return sess, nil
}

// timeToSnowflake converts a time.Time to the string representation of the
// nearest Discord snowflake ID. Returns an empty string for the zero time
// (indicating "fetch all messages from the beginning").
//
// Discord snowflakes encode milliseconds since the Discord epoch (2015-01-01T00:00:00Z).
// The 22-bit right-shift recovers the time portion.
//
//	snowflake = (ms_since_discord_epoch) << 22
func timeToSnowflake(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	// Discord epoch: 2015-01-01T00:00:00Z
	const discordEpochMs int64 = 1420070400000
	ms := t.UnixMilli() - discordEpochMs
	if ms <= 0 {
		return ""
	}
	snowflake := ms << 22
	return strings.TrimSpace(fmt.Sprintf("%d", snowflake))
}
