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
//  3. Attachment download + extraction pipeline per message
package collector

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/collector/extractor"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// maxAttachmentBytes is the hard cap for a single Discord attachment download.
// Attachments larger than this are skipped to protect memory.
const maxAttachmentBytes = 25 * 1024 * 1024 // 25 MB

// attachmentExtractTimeout is the per-attachment deadline for binary extraction
// (PDF, Office formats). Plain-text formats do not use this; they read in-memory.
const attachmentExtractTimeout = 15 * time.Second

// allowedAttachmentExts is a positive-list of extensions whose text can be
// extracted and indexed. Extensions not in this list are silently skipped.
var allowedAttachmentExts = map[string]bool{
	".txt":  true,
	".md":   true,
	".pdf":  true,
	".docx": true,
	".xlsx": true,
	".pptx": true,
	".html": true,
	".htm":  true,
	".csv":  true,
	".json": true,
	".yaml": true,
	".yml":  true,
}

// plainTextAttachmentExts are extensions that can be decoded directly as
// UTF-8 without an extractor (no binary parsing needed).
var plainTextAttachmentExts = map[string]bool{
	".txt":  true,
	".md":   true,
	".csv":  true,
	".json": true,
	".yaml": true,
	".yml":  true,
}

// AttachmentDocumentStore is the document persistence interface used by
// DiscordCollector for attachment documents. It is a subset of store.DocumentStore.
type AttachmentDocumentStore interface {
	Upsert(ctx context.Context, doc *model.Document) error
}

// DiscordCollector collects messages from Discord guild channels using the
// discordgo library. It respects discordgo's built-in rate-limit handler.
type DiscordCollector struct {
	botToken               string
	applicationID          string
	guildIDs               []string
	mentionResponseEnabled bool

	// session is the discordgo session used for REST API calls.
	// It is created lazily on the first Collect call and reused.
	session *discordgo.Session

	// httpClient is used for attachment downloads. When nil, http.DefaultClient is used.
	httpClient *http.Client

	// docStore is the document persistence layer for attachment documents.
	// When nil, attachment documents are not persisted.
	docStore AttachmentDocumentStore

	// extractionFailures records extraction failures for retry. When nil,
	// failures are only logged.
	extractionFailures *store.ExtractionFailureStore

	// extractorReg is the extractor registry for binary/structured formats.
	extractorReg *extractor.Registry
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
		botToken:               botToken,
		applicationID:         applicationID,
		guildIDs:               guildIDs,
		mentionResponseEnabled: mentionResponseEnabled,
		httpClient:             &http.Client{Timeout: 60 * time.Second},
		extractorReg:           extractor.NewRegistry(),
	}
}

// NewDiscordCollectorWithAttachments returns a DiscordCollector with full
// attachment processing support. docStore and extractionFailures may be nil
// (attachments are then skipped or failures only logged respectively).
func NewDiscordCollectorWithAttachments(
	botToken string,
	applicationID string,
	guildIDs []string,
	mentionResponseEnabled bool,
	docStore AttachmentDocumentStore,
	extractionFailures *store.ExtractionFailureStore,
) *DiscordCollector {
	return &DiscordCollector{
		botToken:               botToken,
		applicationID:         applicationID,
		guildIDs:               guildIDs,
		mentionResponseEnabled: mentionResponseEnabled,
		httpClient:             &http.Client{Timeout: 60 * time.Second},
		docStore:               docStore,
		extractionFailures:     extractionFailures,
		extractorReg:           extractor.NewRegistry(),
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

		// Process message body (skip empty-content messages).
		if m.Content != "" {
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

		// Process attachments — failures are logged but never abort message collection.
		for _, att := range m.Attachments {
			if err := c.processAttachment(ctx, guildID, channelID, m, att); err != nil {
				slog.Warn("discord: attachment processing failed",
					"guild", guildID,
					"channel", channelID,
					"message", m.ID,
					"filename", att.Filename,
					"err", err)
				// continue — one attachment failure does not abort other attachments
			}
		}
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

// FeedbackEntry is the domain type the Discord gateway uses to record reaction
// feedback. It mirrors store.Feedback but avoids a direct store import so the
// collector package stays decoupled from persistence details.
type FeedbackEntry struct {
	Source    string
	SessionID *string        // message ID used as session key
	UserID    *string        // opaque Discord user ID
	Thumbs    int16          // +1 (👍) or -1 (👎)
	Comment   *string        // bot answer used as context hint
	Metadata  map[string]any // guild_id, channel_id, message_id, emoji
}

// FeedbackRecorder is the minimal interface the gateway requires to persist
// reaction feedback. store.FeedbackStore satisfies this interface via
// feedbackStoreAdapter (see feedback_adapter.go) when the Feedback type
// signatures differ.
type FeedbackRecorder interface {
	Record(ctx context.Context, entry FeedbackEntry) (int64, error)
}

// legacyFallbackMessage is the static reply used when RAG dependencies are not
// configured. Defined as a constant so tests can assert on the exact value
// without hardcoding the string in multiple places (#31).
const legacyFallbackMessage = "지금은 AI 응답 기능이 비활성 상태입니다. 관리자에게 문의해주세요."

// DiscordGateway holds the WebSocket session used for real-time mention responses
// and real-time message collection.
// It is decoupled from the REST-based DiscordCollector to keep concerns separate.
//
// When both searcher and llmClient are non-nil, it performs full RAG:
//
//	mention → search top-K docs → LLM chat completion → Discord reply
//
// When either dependency is nil it falls back to the legacy static message.
//
// When docStore is non-nil (injected via SetDocStore), every incoming guild message
// is upserted immediately — reducing collection latency from 5 minutes to near-zero.
// The 5-minute cron collector continues to run as a backfill for messages missed
// during gateway downtime; duplicate source_ids are de-duplicated by the UNIQUE
// constraint on documents.source_id.
type DiscordGateway struct {
	botToken               string
	mentionResponseEnabled bool
	session                *discordgo.Session

	searcher  Searcher
	llmClient llm.Completer

	// docStore enables real-time message persistence. When nil, the gateway
	// only handles mention responses (backward-compatible with existing callers).
	docStore AttachmentDocumentStore

	// feedbackStore receives thumbs-up/down reactions on bot replies.
	// When nil, reaction feedback is silently ignored.
	feedbackStore FeedbackRecorder

	// metrics collects per-response latency and quality signals.
	// When nil, metrics recording is a no-op (backward-compatible).
	metrics *DiscordMetrics
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
	llmClient llm.Completer,
) *DiscordGateway {
	return &DiscordGateway{
		botToken:               botToken,
		mentionResponseEnabled: mentionResponseEnabled,
		searcher:               searcher,
		llmClient:              llmClient,
	}
}

// SetDocStore injects a document store for real-time message persistence.
// It must be called before Run. It is safe to call even after construction so
// existing callers of NewDiscordGateway do not need to change their call sites.
func (g *DiscordGateway) SetDocStore(s AttachmentDocumentStore) {
	g.docStore = s
}

// SetFeedbackStore injects a feedback recorder so that 👍/👎 reactions on bot
// replies are persisted. It must be called before Run. When nil (the default),
// reaction feedback is silently ignored — backward-compatible with callers that
// do not need feedback collection.
func (g *DiscordGateway) SetFeedbackStore(r FeedbackRecorder) {
	g.feedbackStore = r
}

// SetMetrics injects a DiscordMetrics instance for response-latency tracking.
// It must be called before Run. When nil (the default), metrics recording is
// skipped — backward-compatible with callers that do not need metrics.
func (g *DiscordGateway) SetMetrics(m *DiscordMetrics) {
	g.metrics = m
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

	// IntentsGuilds is required so the gateway receives GUILD_CREATE events,
	// which populate the state cache used by s.State.Channel(). Without it the
	// state cache is always empty and every handleMessageCreate call silently
	// drops the message at the channel-type guard.
	// GuildMessages is required for mention detection and real-time collection.
	// GuildMessageReactions is required to receive reaction add events for the
	// feedback path.
	sess.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsGuildMessageReactions

	// Register the MessageCreate handler whenever real-time collection or mention
	// response is enabled. A single handler covers both code paths to avoid
	// double-processing the same event.
	if g.mentionResponseEnabled || g.docStore != nil {
		sess.AddHandler(g.handleMessageCreate)
	}

	// Register the MessageReactionAdd handler when feedback recording is enabled.
	if g.feedbackStore != nil {
		sess.AddHandler(g.onReactionAdd)
	}

	if err := sess.Open(); err != nil {
		slog.Error("discord gateway: failed to open WebSocket", "error", err)
		return
	}
	g.session = sess

	slog.Info("discord gateway: WebSocket connected",
		"mention_response", g.mentionResponseEnabled,
		"realtime_collect", g.docStore != nil)

	<-ctx.Done()

	slog.Info("discord gateway: shutting down WebSocket")
	if err := sess.Close(); err != nil {
		slog.Warn("discord gateway: error during close", "error", err)
	}
}

// handleMessageCreate is the discordgo event handler for MESSAGE_CREATE events.
// It serves two independent purposes:
//
//  1. Real-time collection: when docStore is configured, every guild message is
//     immediately upserted to the document store (zero-latency path). This runs
//     concurrently and never blocks the WebSocket read loop.
//
//  2. Mention response: when mentionResponseEnabled is true and the bot is @mentioned,
//     the RAG pipeline is triggered and a reply is sent.
//
// Both paths are gated behind DM/bot-self guards and operate independently so
// that a failure in one does not affect the other.
func (g *DiscordGateway) handleMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	// Guard: ignore messages sent by the bot itself to prevent echo loops.
	if m.Author.ID == s.State.User.ID {
		return
	}

	// Guard: only process guild text channels and threads. DM and GroupDM
	// channels must never be collected (positive-list, mirrors DiscordCollector).
	//
	// resolveChannel tries the gateway state cache first and falls back to a REST
	// call when the cache is cold (e.g. IntentsGuilds was missing at startup).
	channel, err := resolveChannel(sessionAdapter{s}, m.ChannelID)
	if err != nil {
		slog.Warn("discord gateway: channel lookup failed",
			"channel_id", m.ChannelID,
			"error", err,
		)
		return
	}
	if !isAllowedChannelType(channel.Type) {
		// DM/GroupDM — silent skip is intentional (privacy guard).
		return
	}

	// Path 1: Real-time message persistence (non-blocking goroutine).
	// docStore nil-check ensures backward compatibility: callers that do not
	// call SetDocStore continue to work without any modification.
	if g.docStore != nil {
		go g.persistMessageRealtime(context.Background(), s, m)
	}

	// Path 2: Mention response (existing logic — unchanged).
	if !g.mentionResponseEnabled {
		return
	}

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

	// Use a per-request timeout to avoid hanging the WebSocket read loop.
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	ref := m.Reference()
	reply := g.buildReply(ctx, s, m)

	chunks := splitForDiscord(reply, 1900)
	for i, chunk := range chunks {
		sent, err := s.ChannelMessageSendReply(m.ChannelID, chunk, ref)
		if err != nil {
			slog.Warn("discord gateway: failed to send reply chunk",
				"channel_id", m.ChannelID,
				"error", err,
			)
			// Attempt plain send as fallback (original message may have been deleted).
			sent, err = s.ChannelMessageSend(m.ChannelID, chunk)
			if err != nil {
				slog.Warn("discord gateway: fallback send also failed",
					"channel_id", m.ChannelID,
					"error", err,
				)
				continue
			}
		}

		// Add 👍/👎 reactions to the first chunk only so users have a single
		// feedback target per RAG answer. The first chunk is preferred over the
		// last because it is immediately visible to the user.
		if i == 0 && g.feedbackStore != nil && sent != nil {
			_ = s.MessageReactionAdd(sent.ChannelID, sent.ID, "👍")
			_ = s.MessageReactionAdd(sent.ChannelID, sent.ID, "👎")
		}
	}
}

// persistMessageRealtime upserts a single Discord message to the document store
// immediately upon receipt from the WebSocket gateway.
//
// This is the zero-latency collection path that complements the 5-minute cron
// collector (DiscordCollector.Collect). Duplicate messages are de-duplicated at
// the store layer via the UNIQUE constraint on documents.source_id — no
// application-level dedup is needed.
//
// Attachment processing mirrors the cron collector's behaviour: each attachment
// is downloaded and text-extracted using the same pipeline (processGatewayAttachment).
// Attachment failures are soft — they are logged and never abort message persistence.
//
// The function is always called from a goroutine and must not reference the
// discordgo event structs after the goroutine returns (discordgo may reuse them).
// The *discordgo.MessageCreate value is passed by pointer but its fields are read
// synchronously before any blocking I/O so no data races occur.
func (g *DiscordGateway) persistMessageRealtime(
	ctx context.Context,
	s *discordgo.Session,
	m *discordgo.MessageCreate,
) {
	// Read all fields we need from the event struct before any I/O.
	guildID := m.GuildID
	channelID := m.ChannelID
	messageID := m.ID
	authorID := m.Author.ID
	authorName := m.Author.Username
	content := m.Content
	timestamp := m.Timestamp
	attachments := m.Attachments
	msg := m.Message // pointer for attachment processing

	// Resolve channel name from gateway state cache (best-effort; empty on miss).
	channelName := ""
	if ch, err := s.State.Channel(channelID); err == nil {
		channelName = ch.Name
	}

	// Only persist messages with a non-empty text body.
	// Attachment-only messages (content == "") are handled via the attachment path below.
	if content != "" {
		sourceID := fmt.Sprintf("discord:%s:%s:%s", guildID, channelID, messageID)

		title := fmt.Sprintf("#%s — %s", channelName, timestamp.Format("2006-01-02"))

		doc := &model.Document{
			ID:         uuid.New(),
			SourceType: model.SourceDiscord,
			SourceID:   sourceID,
			Title:      title,
			Content:    content,
			Metadata: map[string]any{
				"guild_id":     guildID,
				"channel_id":   channelID,
				"channel_name": channelName,
				"author_id":    authorID,
				"author_name":  authorName,
			},
			CollectedAt: time.Now().UTC(),
		}

		if err := g.docStore.Upsert(ctx, doc); err != nil {
			slog.Warn("discord gateway: realtime upsert failed",
				"guild", guildID,
				"channel", channelID,
				"message", messageID,
				"err", err)
			// Non-fatal: continue to attachment processing below.
		} else {
			slog.Debug("discord gateway: realtime upsert ok",
				"guild", guildID,
				"channel", channelID,
				"message", messageID)
		}
	}

	// Process attachments — mirrors DiscordCollector.collectChannel attachment loop.
	// Each attachment failure is soft (logged, not propagated).
	for _, att := range attachments {
		if err := g.processGatewayAttachment(ctx, guildID, channelID, msg, att); err != nil {
			slog.Warn("discord gateway: realtime attachment failed",
				"guild", guildID,
				"channel", channelID,
				"message", messageID,
				"filename", att.Filename,
				"err", err)
		}
	}
}

// processGatewayAttachment processes a single attachment from a real-time
// MessageCreate event. It delegates to a short-lived DiscordCollector adapter
// so the download/extract/upsert logic is not duplicated.
func (g *DiscordGateway) processGatewayAttachment(
	ctx context.Context,
	guildID, channelID string,
	msg *discordgo.Message,
	att *discordgo.MessageAttachment,
) error {
	// Build a minimal DiscordCollector that shares only the dependencies needed
	// for attachment processing: docStore, httpClient, and extractorReg.
	// botToken and guildIDs are not used by processAttachment.
	adapter := &DiscordCollector{
		docStore:     g.docStore,
		httpClient:   &http.Client{Timeout: 60 * time.Second},
		extractorReg: extractor.NewRegistry(),
	}
	return adapter.processAttachment(ctx, guildID, channelID, msg, att)
}

// onReactionAdd is the discordgo event handler for MESSAGE_REACTION_ADD events.
// It records 👍/👎 reactions on bot reply messages as feedback entries.
//
// The handler ignores:
//   - Reactions added by the bot itself (auto-added thumbs on send).
//   - Reactions on messages not authored by the bot.
//   - Emojis other than 👍 and 👎.
func (g *DiscordGateway) onReactionAdd(s *discordgo.Session, r *discordgo.MessageReactionAdd) {
	// Retrieve the reacted-to message to verify authorship.
	msg, err := s.ChannelMessage(r.ChannelID, r.MessageID)
	if err != nil {
		// Non-fatal: message may have been deleted or the bot may lack permissions.
		slog.Debug("discord gateway: could not fetch reacted message",
			"channel_id", r.ChannelID,
			"message_id", r.MessageID,
			"error", err)
		return
	}

	ctx := context.Background()
	processed := processReactionFeedback(
		ctx,
		s.State.User.ID,
		r.UserID,
		msg.Author.ID,
		r.Emoji.Name,
		r.ChannelID,
		r.MessageID,
		r.GuildID,
		msg.Content,
		g.feedbackStore,
	)
	if processed {
		slog.Info("discord gateway: feedback recorded",
			"user", r.UserID,
			"emoji", r.Emoji.Name,
			"message", r.MessageID)
	}
}

// processReactionFeedback is a pure function that encapsulates all reaction
// feedback logic without referencing *discordgo.Session. This makes it easy
// to unit-test without mocking the WebSocket session.
//
// Returns true when a feedback entry was submitted to the recorder, false when
// the reaction was filtered out (bot self-reaction, non-bot message, wrong emoji).
func processReactionFeedback(
	ctx context.Context,
	botUserID string,
	reactorUserID string,
	msgAuthorID string,
	emoji string,
	channelID, messageID, guildID string,
	msgContent string,
	recorder FeedbackRecorder,
) bool {
	// Ignore reactions added by the bot itself (the automatic 👍/👎 we add on send).
	if reactorUserID == botUserID {
		return false
	}

	// Only record reactions on messages authored by the bot.
	if msgAuthorID != botUserID {
		return false
	}

	// Only record 👍 and 👎; ignore all other emojis.
	var thumbs int16
	switch emoji {
	case "👍":
		thumbs = 1
	case "👎":
		thumbs = -1
	default:
		return false
	}

	// No-op when recorder is not configured.
	if recorder == nil {
		return false
	}

	msgIDCopy := messageID
	userIDCopy := reactorUserID
	contentCopy := msgContent

	// Use Upsert semantics: the FeedbackRecorder interface exposes Record so the
	// adapter layer decides whether to delegate to Record or Upsert on the store.
	_, err := recorder.Record(ctx, FeedbackEntry{
		Source:    "discord_bot",
		SessionID: &msgIDCopy,
		UserID:    &userIDCopy,
		Thumbs:    thumbs,
		Comment:   &contentCopy,
		Metadata: map[string]any{
			"guild_id":   guildID,
			"channel_id": channelID,
			"message_id": messageID,
			"emoji":      emoji,
		},
	})
	if err != nil {
		slog.Warn("discord gateway: failed to record feedback",
			"user", reactorUserID,
			"emoji", emoji,
			"message", messageID,
			"error", err)
		return false
	}
	return true
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
		return legacyFallbackMessage
	}

	start := time.Now()

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
	t1 := time.Now()
	history, err := s.ChannelMessages(m.ChannelID, 20, m.ID, "", "")
	historyLatencyMs := time.Since(t1).Milliseconds()
	if err != nil {
		slog.Warn("discord gateway: failed to fetch channel history",
			"channel_id", m.ChannelID,
			"error", err,
			"latency_ms", historyLatencyMs)
		history = nil // non-fatal — continue without conversation context
	}
	// ChannelMessages with beforeID returns messages in descending order
	// (newest first). Reverse to get chronological order for the LLM.
	for i, j := 0, len(history)-1; i < j; i, j = i+1, j-1 {
		history[i], history[j] = history[j], history[i]
	}

	// 4. Search knowledge base (best-effort; search is optional context, not a gate).
	const searchLimit = 10
	t2 := time.Now()
	results, err := g.searcher.Search(ctx, model.SearchQuery{
		Query:          query,
		Limit:          searchLimit,
		IncludeDeleted: false,
	})
	searchLatencyMs := time.Since(t2).Milliseconds()
	if err != nil {
		slog.Warn("discord gateway: search failed, proceeding without context",
			"error", err,
			"latency_ms", searchLatencyMs)
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
	t3 := time.Now()
	answer, llmErr := g.llmClient.CompleteWithMessages(ctx, discordSystemPrompt, conversationMsgs)
	llmLatencyMs := time.Since(t3).Milliseconds()

	totalLatency := time.Since(start)
	zeroResults := len(results) == 0

	// Structured response metrics log. Message content is never logged —
	// only character/chunk counts and IDs (channel_id, user_id are OK per spec).
	slog.Info("discord: response metrics",
		"total_ms", totalLatency.Milliseconds(),
		"history_ms", historyLatencyMs,
		"history_count", len(history),
		"search_ms", searchLatencyMs,
		"search_hits", len(results),
		"context_bytes", len(contextBlock),
		"llm_ms", llmLatencyMs,
		"reply_chars", utf8.RuneCountInString(answer),
		"chunks", len(splitForDiscord(answer, 1900)),
		"query_chars", utf8.RuneCountInString(query),
		"zero_search_results", zeroResults,
		"channel_id", m.ChannelID,
		"user_id", m.Author.ID,
	)

	// Record into in-memory metrics store when injected.
	if g.metrics != nil {
		g.metrics.Record(totalLatency, zeroResults)
	}

	if llmErr != nil {
		slog.Error("discord gateway: LLM completion failed",
			"error", llmErr,
			"latency_ms", llmLatencyMs)
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

// --- Attachment processing ---

// processAttachment downloads a single Discord attachment, extracts its text
// content, and upserts it as a separate document.
//
// The function is a no-op (returns nil) when:
//   - the attachment exceeds maxAttachmentBytes (size guard from Discord metadata)
//   - the extension is not in allowedAttachmentExts
//   - the collector has no docStore configured
//
// Non-nil errors are soft — callers should log and continue.
func (c *DiscordCollector) processAttachment(
	ctx context.Context,
	guildID, channelID string,
	msg *discordgo.Message,
	att *discordgo.MessageAttachment,
) error {
	// Guard: no docStore means nowhere to persist — skip silently.
	if c.docStore == nil {
		return nil
	}

	// Guard: size limit from Discord metadata (avoids even starting the download).
	if att.Size > maxAttachmentBytes {
		slog.Info("discord: skipping large attachment",
			"filename", att.Filename,
			"size", att.Size,
			"limit", maxAttachmentBytes)
		return nil
	}

	// Guard: positive-list extension filter.
	ext := strings.ToLower(filepath.Ext(att.Filename))
	if !allowedAttachmentExts[ext] {
		slog.Debug("discord: skipping attachment with unsupported extension",
			"filename", att.Filename,
			"ext", ext)
		return nil
	}

	// Download the attachment with context awareness.
	data, err := c.downloadAttachment(ctx, att.URL)
	if err != nil {
		return fmt.Errorf("download %q: %w", att.Filename, err)
	}

	// Double-check size after download (Discord metadata may be inaccurate).
	if len(data) > maxAttachmentBytes {
		return fmt.Errorf("attachment %q exceeds %d bytes after download (got %d)",
			att.Filename, maxAttachmentBytes, len(data))
	}

	// Extract text content.
	sourceID := fmt.Sprintf("discord:%s:%s:%s:att:%s", guildID, channelID, msg.ID, att.ID)
	text, err := c.extractAttachmentText(ctx, att.Filename, ext, data)
	if err != nil {
		// Record failure for retry pipeline and return error to caller.
		if c.extractionFailures != nil {
			_ = c.extractionFailures.Record(ctx, store.ExtractionFailure{
				SourceType:   "discord",
				SourceID:     sourceID,
				FilePath:     att.Filename,
				ErrorMessage: err.Error(),
			})
		}
		return fmt.Errorf("extract %q: %w", att.Filename, err)
	}

	if text == "" {
		slog.Debug("discord: attachment yielded no text content",
			"filename", att.Filename)
		return nil
	}

	// Build and upsert the attachment document.
	doc := &model.Document{
		ID:         uuid.New(),
		SourceType: model.SourceDiscord,
		SourceID:   sourceID,
		Title:      att.Filename,
		Content:    text,
		Metadata: map[string]any{
			"guild_id":      guildID,
			"channel_id":    channelID,
			"message_id":    msg.ID,
			"attachment_id": att.ID,
			"filename":      att.Filename,
			"size":          att.Size,
			"content_type":  att.ContentType,
			"author_id":     msg.Author.ID,
			"author_name":   msg.Author.Username,
		},
		CollectedAt: time.Now().UTC(),
	}
	if err := c.docStore.Upsert(ctx, doc); err != nil {
		return fmt.Errorf("upsert attachment document %q: %w", att.Filename, err)
	}

	slog.Info("discord: attachment processed",
		"filename", att.Filename,
		"size", att.Size,
		"text_bytes", len(text))
	return nil
}

// downloadAttachment fetches the attachment URL and returns its body as bytes.
// It honours the context deadline and enforces the maxAttachmentBytes cap at
// the stream level so oversized responses do not fully buffer in memory.
func (c *DiscordCollector) downloadAttachment(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}

	client := c.httpClient
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP status %d for %q", resp.StatusCode, url)
	}

	// Read up to maxAttachmentBytes+1 to detect oversized responses without
	// fully buffering them.
	buf := &bytes.Buffer{}
	if _, err := io.CopyN(buf, resp.Body, int64(maxAttachmentBytes)+1); err != nil && err != io.EOF {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if buf.Len() > maxAttachmentBytes {
		return nil, fmt.Errorf("response body exceeds %d bytes", maxAttachmentBytes)
	}
	return buf.Bytes(), nil
}

// extractAttachmentText dispatches to the appropriate extraction strategy:
//   - Plain text extensions (txt, md, csv, json, yaml, yml): decode bytes directly as UTF-8.
//   - Binary/structured extensions (pdf, docx, xlsx, pptx, html): write a temp file
//     and call the extractor registry, then remove the temp file.
func (c *DiscordCollector) extractAttachmentText(
	ctx context.Context,
	filename, ext string,
	data []byte,
) (string, error) {
	if plainTextAttachmentExts[ext] {
		return extractor.SanitizeText(string(data)), nil
	}

	// Binary formats require a temporary file because the extractor registry
	// uses file-path-based parsing (zip, PDF, XML readers).
	ex := c.extractorReg.Find(ext)
	if ex == nil {
		// Extension is in allowedAttachmentExts but has no extractor — should
		// not happen with the current config, but guard defensively.
		return "", fmt.Errorf("no extractor registered for extension %q", ext)
	}

	// Write to a temporary file.
	tmpFile, err := os.CreateTemp("", "discord-att-*"+ext)
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath) // best-effort cleanup

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return "", fmt.Errorf("write temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("close temp file: %w", err)
	}

	// Apply per-extraction timeout on top of the caller's context.
	extractCtx, cancel := context.WithTimeout(ctx, attachmentExtractTimeout)
	defer cancel()

	text, err := ex.Extract(extractCtx, tmpPath)
	if err != nil {
		return "", fmt.Errorf("extract %q: %w", filename, err)
	}
	return text, nil
}

// --- Helpers ---

// channelResolver abstracts the two channel-lookup paths used by
// resolveChannel. The interface exists solely for unit testing; production
// code always passes a sessionAdapter wrapping *discordgo.Session.
type channelResolver interface {
	// StateChannel returns the channel from the in-memory gateway state cache.
	StateChannel(id string) (*discordgo.Channel, error)
	// RESTChannel fetches the channel from the Discord REST API.
	RESTChannel(id string) (*discordgo.Channel, error)
}

// sessionAdapter adapts *discordgo.Session to channelResolver.
type sessionAdapter struct{ s *discordgo.Session }

func (a sessionAdapter) StateChannel(id string) (*discordgo.Channel, error) {
	return a.s.State.Channel(id)
}
func (a sessionAdapter) RESTChannel(id string) (*discordgo.Channel, error) {
	return a.s.Channel(id)
}

// resolveChannel returns the Discord channel for id using a state-first,
// REST-fallback strategy.  It returns an error only when both lookups fail.
// A warn-level log is emitted on state miss so that operators can diagnose
// missing IntentsGuilds without reading source code.
func resolveChannel(r channelResolver, id string) (*discordgo.Channel, error) {
	ch, err := r.StateChannel(id)
	if err == nil {
		return ch, nil
	}
	ch, err = r.RESTChannel(id)
	if err != nil {
		return nil, err
	}
	slog.Warn("discord gateway: channel resolved via REST (state cache miss — check IntentsGuilds)",
		"channel_id", id,
	)
	return ch, nil
}

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
