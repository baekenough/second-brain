package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/model"
)

// SlackCollector collects messages from Slack channels using the Web API.
type SlackCollector struct {
	botToken string
	teamID   string
	client   *http.Client
}

// NewSlackCollector returns a SlackCollector. When botToken is empty the
// collector is disabled and Collect will not be called.
func NewSlackCollector(botToken, teamID string) *SlackCollector {
	return &SlackCollector{
		botToken: botToken,
		teamID:   teamID,
		client:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *SlackCollector) Name() string             { return "slack" }
func (c *SlackCollector) Source() model.SourceType { return model.SourceSlack }
func (c *SlackCollector) Enabled() bool            { return c.botToken != "" }

// Collect fetches messages from all channels the bot is a member of, updated
// since the given time. For each top-level message that has thread replies, it
// additionally fetches all replies via conversations.replies and merges them as
// independent Documents.
// Known limitation: if a parent message predates `since` but received new replies
// after `since`, those replies will not be collected in incremental runs.
func (c *SlackCollector) Collect(ctx context.Context, since time.Time) ([]model.Document, error) {
	channels, err := c.listChannels(ctx)
	if err != nil {
		return nil, fmt.Errorf("slack list channels: %w", err)
	}

	var docs []model.Document
	for _, ch := range channels {
		chDocs, err := c.collectChannel(ctx, ch, since)
		if err != nil {
			slog.Warn("slack: failed to collect channel",
				"channel", ch.Name, "error", err)
			continue
		}
		docs = append(docs, chDocs...)
	}

	slog.Info("slack: collected documents", "count", len(docs))
	return docs, nil
}

// CollectChannel fetches messages from a single channel by ID and name.
// since=time.Time{} fetches the full history. This method is intended for
// the SlackChannelWatcher to trigger an immediate first-collection for
// newly joined channels.
func (c *SlackCollector) CollectChannel(ctx context.Context, channelID, channelName string, since time.Time) ([]model.Document, error) {
	return c.collectChannel(ctx, slackChannel{ID: channelID, Name: channelName}, since)
}

// collectChannel fetches and converts all messages in a single channel since
// the given timestamp. It is the shared implementation used by both Collect
// and CollectChannel.
func (c *SlackCollector) collectChannel(ctx context.Context, ch slackChannel, since time.Time) ([]model.Document, error) {
	msgs, err := c.channelHistory(ctx, ch.ID, since)
	if err != nil {
		return nil, fmt.Errorf("channel history %s: %w", ch.Name, err)
	}

	// Expand threads: for each top-level message with replies,
	// fetch conversations.replies and merge, deduplicated by ts.
	seen := make(map[string]struct{}, len(msgs))
	var expanded []slackMessage
	for _, m := range msgs {
		if _, ok := seen[m.Timestamp]; !ok {
			seen[m.Timestamp] = struct{}{}
			expanded = append(expanded, m)
		}
		if m.ReplyCount > 0 && m.Timestamp != "" {
			replies, err := c.channelReplies(ctx, ch.ID, m.Timestamp)
			if err != nil {
				slog.Warn("slack: failed to fetch thread replies",
					"channel", ch.Name, "thread_ts", m.Timestamp, "error", err)
				continue
			}
			for _, r := range replies {
				if _, ok := seen[r.Timestamp]; ok {
					continue
				}
				seen[r.Timestamp] = struct{}{}
				expanded = append(expanded, r)
			}
		}
	}

	var docs []model.Document
	for _, m := range expanded {
		if m.Text == "" {
			continue
		}
		docs = append(docs, model.Document{
			ID:         uuid.New(),
			SourceType: model.SourceSlack,
			SourceID:   fmt.Sprintf("%s:%s", ch.ID, m.Timestamp),
			Title:      fmt.Sprintf("#%s — %s", ch.Name, m.Timestamp),
			Content:    m.Text,
			Metadata: map[string]any{
				"channel_id":   ch.ID,
				"channel_name": ch.Name,
				"user":         m.User,
				"thread_ts":    m.ThreadTimestamp,
				"team_id":      c.teamID,
			},
			CollectedAt: time.Now().UTC(),
		})
	}
	return docs, nil
}

// --- Slack API helpers ---

type slackChannel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type slackMessage struct {
	Type            string `json:"type"`
	User            string `json:"user"`
	Text            string `json:"text"`
	Timestamp       string `json:"ts"`
	ThreadTimestamp string `json:"thread_ts"`
	ReplyCount      int    `json:"reply_count"`
}

// listChannels returns all channels (public and private) that the bot is a
// member of, using the users.conversations API. This avoids not_in_channel
// errors that occur when conversations.list returns channels the bot cannot
// read history from.
func (c *SlackCollector) listChannels(ctx context.Context) ([]slackChannel, error) {
	return c.ListMemberChannels(ctx)
}

// FindMemberChannelByName looks up a channel the bot belongs to by name.
// Matching is case-insensitive and ignores a leading "#".
// Returns (id, name, true, nil) on match, ("", "", false, nil) when not found,
// and ("", "", false, err) on API failure.
func (c *SlackCollector) FindMemberChannelByName(ctx context.Context, name string) (string, string, bool, error) {
	target := strings.ToLower(strings.TrimPrefix(name, "#"))
	channels, err := c.ListMemberChannels(ctx)
	if err != nil {
		return "", "", false, fmt.Errorf("list member channels: %w", err)
	}
	for _, ch := range channels {
		if strings.ToLower(ch.Name) == target {
			return ch.ID, ch.Name, true, nil
		}
	}
	return "", "", false, nil
}

// ListMemberChannels calls users.conversations to enumerate every channel the
// bot belongs to. It handles cursor-based pagination automatically.
func (c *SlackCollector) ListMemberChannels(ctx context.Context) ([]slackChannel, error) {
	var all []slackChannel
	cursor := ""
	for {
		params := url.Values{
			"exclude_archived": {"true"},
			"types":            {"public_channel,private_channel"},
			"limit":            {"200"},
		}
		if cursor != "" {
			params.Set("cursor", cursor)
		}

		var resp struct {
			OK       bool           `json:"ok"`
			Error    string         `json:"error"`
			Channels []slackChannel `json:"channels"`
			Meta     struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := c.get(ctx, "users.conversations", params, &resp); err != nil {
			return nil, err
		}
		if !resp.OK {
			return nil, fmt.Errorf("slack API error: %s", resp.Error)
		}
		all = append(all, resp.Channels...)
		if resp.Meta.NextCursor == "" {
			break
		}
		cursor = resp.Meta.NextCursor
	}
	return all, nil
}

func (c *SlackCollector) channelHistory(ctx context.Context, channelID string, since time.Time) ([]slackMessage, error) {
	var all []slackMessage
	cursor := ""
	for {
		params := url.Values{
			"channel": {channelID},
			"limit":   {"200"},
		}
		// Slack silently returns zero messages for negative oldest values
		// (e.g. time.Time{}.Unix() == -62135596800). Omit the parameter
		// entirely on the first run so Slack returns the full history.
		if since.Unix() > 0 {
			// Slack timestamps are Unix seconds with microsecond precision
			// (e.g. "1234567890.123456"). Use integer seconds to avoid
			// float64 precision loss on large epoch values.
			params.Set("oldest", strconv.FormatInt(since.Unix(), 10))
		}
		if cursor != "" {
			params.Set("cursor", cursor)
		}

		var resp struct {
			OK       bool           `json:"ok"`
			Error    string         `json:"error"`
			Messages []slackMessage `json:"messages"`
			Meta     struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := c.get(ctx, "conversations.history", params, &resp); err != nil {
			return nil, err
		}
		if !resp.OK {
			return nil, fmt.Errorf("slack API error: %s", resp.Error)
		}
		all = append(all, resp.Messages...)
		if resp.Meta.NextCursor == "" {
			break
		}
		cursor = resp.Meta.NextCursor
	}
	return all, nil
}

// channelReplies fetches all messages in a single thread via conversations.replies.
// The Slack API always includes the parent message as the first element; callers
// are responsible for deduplication by timestamp.
func (c *SlackCollector) channelReplies(ctx context.Context, channelID, threadTS string) ([]slackMessage, error) {
	var all []slackMessage
	cursor := ""
	for {
		params := url.Values{
			"channel": {channelID},
			"ts":      {threadTS},
			"limit":   {"200"},
		}
		if cursor != "" {
			params.Set("cursor", cursor)
		}

		var resp struct {
			OK       bool           `json:"ok"`
			Error    string         `json:"error"`
			Messages []slackMessage `json:"messages"`
			Meta     struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := c.get(ctx, "conversations.replies", params, &resp); err != nil {
			return nil, err
		}
		if !resp.OK {
			return nil, fmt.Errorf("slack replies API error: %s", resp.Error)
		}
		all = append(all, resp.Messages...)
		if resp.Meta.NextCursor == "" {
			break
		}
		cursor = resp.Meta.NextCursor
	}
	return all, nil
}

func (c *SlackCollector) get(ctx context.Context, method string, params url.Values, dest interface{}) error {
	u := fmt.Sprintf("https://slack.com/api/%s?%s", method, params.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.botToken)

	res, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, dest)
}

// post sends an application/x-www-form-urlencoded POST to the Slack API.
func (c *SlackCollector) post(ctx context.Context, method string, params url.Values, dest interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://slack.com/api/"+method,
		strings.NewReader(params.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.botToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack %s: status %d", method, resp.StatusCode)
	}
	return json.Unmarshal(body, dest)
}

// joinChannel attempts to join the given public channel so the bot can read
// its history. Non-fatal errors (archived, wrong type, already joined, etc.)
// are surfaced as errors so the caller can log and continue collection.
func (c *SlackCollector) joinChannel(ctx context.Context, channelID string) error {
	params := url.Values{"channel": {channelID}}
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := c.post(ctx, "conversations.join", params, &resp); err != nil {
		return err
	}
	if !resp.OK {
		switch resp.Error {
		case "already_in_channel":
			return nil
		case "is_archived", "method_not_supported_for_channel_type",
			"channel_not_found", "missing_scope", "not_authed",
			"invalid_auth", "no_permission":
			return fmt.Errorf("join skipped: %s", resp.Error)
		default:
			return fmt.Errorf("slack API error: %s", resp.Error)
		}
	}
	return nil
}
