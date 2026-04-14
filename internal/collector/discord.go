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

	"github.com/bwmarrin/discordgo"
	"github.com/google/uuid"
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

// DiscordGateway holds the WebSocket session used for real-time mention responses.
// It is decoupled from the REST-based DiscordCollector to keep concerns separate.
type DiscordGateway struct {
	botToken               string
	mentionResponseEnabled bool
	session                *discordgo.Session
}

// NewDiscordGateway creates a gateway that connects to the Discord WebSocket and
// responds to bot mentions when enabled.
func NewDiscordGateway(botToken string, mentionResponseEnabled bool) *DiscordGateway {
	return &DiscordGateway{
		botToken:               botToken,
		mentionResponseEnabled: mentionResponseEnabled,
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

	const reply = "검색 시스템 온라인입니다. 질문은 /api/v1/search 엔드포인트를 사용해주세요."

	ref := m.Reference()
	if _, err := s.ChannelMessageSendReply(m.ChannelID, reply, ref); err != nil {
		// Fallback to regular send if reply fails (e.g. original message deleted).
		if _, err2 := s.ChannelMessageSend(m.ChannelID, reply); err2 != nil {
			slog.Warn("discord gateway: failed to send mention reply",
				"channel_id", m.ChannelID,
				"error", err2,
			)
		}
	}
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
