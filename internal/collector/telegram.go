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

const telegramAPIBase = "https://api.telegram.org/bot"

// TelegramCollector collects messages from configured Telegram chats via the
// Bot API. It tracks the highest seen update_id so each call to Collect only
// fetches new updates since the previous invocation.
type TelegramCollector struct {
	token        string
	chatIDs      map[int64]bool
	client       *http.Client
	lastUpdateID int64
}

// NewTelegramCollector returns a TelegramCollector. When token is empty or
// chatIDs is empty the collector is disabled.
func NewTelegramCollector(token string, chatIDs []int64) *TelegramCollector {
	idSet := make(map[int64]bool, len(chatIDs))
	for _, id := range chatIDs {
		idSet[id] = true
	}
	return &TelegramCollector{
		token:   token,
		chatIDs: idSet,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *TelegramCollector) Name() string             { return "telegram" }
func (c *TelegramCollector) Source() model.SourceType { return model.SourceTelegram }
func (c *TelegramCollector) Enabled() bool            { return c.token != "" && len(c.chatIDs) > 0 }

// Collect fetches new updates from the Telegram Bot API, filters them to the
// configured chat IDs and messages sent after since, and returns them as
// Documents.
func (c *TelegramCollector) Collect(ctx context.Context, since time.Time) ([]model.Document, error) {
	updates, err := c.getUpdates(ctx)
	if err != nil {
		return nil, fmt.Errorf("telegram getUpdates: %w", err)
	}

	var docs []model.Document
	for _, u := range updates {
		msg := u.Message
		if msg == nil {
			msg = u.ChannelPost
		}
		if msg == nil {
			continue
		}

		// Filter by configured chat IDs.
		if !c.chatIDs[msg.Chat.ID] {
			continue
		}

		// Filter by time — skip messages older than since.
		msgTime := time.Unix(int64(msg.Date), 0).UTC()
		if msgTime.Before(since) {
			continue
		}

		text := messageText(msg)
		if text == "" {
			continue
		}

		title := chatTitle(msg)
		docs = append(docs, model.Document{
			ID:         uuid.New(),
			SourceType: model.SourceTelegram,
			SourceID:   fmt.Sprintf("%d_%d", msg.Chat.ID, msg.MessageID),
			Title:      title,
			Content:    text,
			Metadata:   messageMetadata(msg),
			CollectedAt: time.Now().UTC(),
		})
	}

	slog.Info("telegram: collected documents", "count", len(docs))
	return docs, nil
}

// --- Telegram API helpers ---

// telegramUpdate is a single update object returned by getUpdates.
type telegramUpdate struct {
	UpdateID    int64            `json:"update_id"`
	Message     *telegramMessage `json:"message"`
	ChannelPost *telegramMessage `json:"channel_post"`
}

// telegramMessage holds the fields of a Telegram Message object we care about.
type telegramMessage struct {
	MessageID int64        `json:"message_id"`
	From      *telegramUser `json:"from"`
	Chat      telegramChat `json:"chat"`
	Date      int64        `json:"date"`
	Text      string       `json:"text"`
	Caption   string       `json:"caption"`
}

type telegramUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

type telegramChat struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"` // "private", "group", "supergroup", "channel"
	Title string `json:"title"`
	Username string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

// getUpdates calls the Telegram getUpdates method with the offset set to
// lastUpdateID+1 so only new updates are returned. It advances lastUpdateID
// after a successful call.
func (c *TelegramCollector) getUpdates(ctx context.Context) ([]telegramUpdate, error) {
	params := url.Values{}
	params.Set("offset", strconv.FormatInt(c.lastUpdateID+1, 10))
	params.Set("limit", "100")
	params.Set("timeout", "0")

	var resp struct {
		OK     bool              `json:"ok"`
		Result []telegramUpdate  `json:"result"`
	}
	if err := c.get(ctx, "getUpdates", params, &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("telegram API returned ok=false for getUpdates")
	}

	// Advance the offset so the next call does not re-process these updates.
	for _, u := range resp.Result {
		if u.UpdateID > c.lastUpdateID {
			c.lastUpdateID = u.UpdateID
		}
	}

	return resp.Result, nil
}

// get performs a GET request to https://api.telegram.org/bot{token}/{method}
// with the given query parameters and unmarshals the JSON response into dest.
func (c *TelegramCollector) get(ctx context.Context, method string, params url.Values, dest interface{}) error {
	rawURL := telegramAPIBase + c.token + "/" + method
	if len(params) > 0 {
		rawURL += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}

	res, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	b, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode >= 400 {
		return fmt.Errorf("telegram API %s: status %d: %s", method, res.StatusCode, b)
	}
	return json.Unmarshal(b, dest)
}

// --- message field helpers ---

// messageText extracts displayable text from a message, preferring Text over
// Caption (used for media messages).
func messageText(msg *telegramMessage) string {
	if msg.Text != "" {
		return msg.Text
	}
	return msg.Caption
}

// chatTitle builds a human-readable title for the chat a message belongs to.
func chatTitle(msg *telegramMessage) string {
	switch msg.Chat.Type {
	case "private":
		parts := []string{msg.Chat.FirstName, msg.Chat.LastName}
		var nonEmpty []string
		for _, p := range parts {
			if p != "" {
				nonEmpty = append(nonEmpty, p)
			}
		}
		name := strings.Join(nonEmpty, " ")
		if name == "" {
			name = msg.Chat.Username
		}
		return "DM with " + name
	default:
		if msg.Chat.Title != "" {
			return msg.Chat.Title
		}
		return fmt.Sprintf("Chat %d", msg.Chat.ID)
	}
}

// messageMetadata builds the metadata map stored alongside the document.
func messageMetadata(msg *telegramMessage) map[string]any {
	meta := map[string]any{
		"chat_id":      msg.Chat.ID,
		"chat_type":    msg.Chat.Type,
		"message_id":   msg.MessageID,
		"date":         time.Unix(int64(msg.Date), 0).UTC().Format(time.RFC3339),
		"message_type": "text",
	}
	if msg.Caption != "" && msg.Text == "" {
		meta["message_type"] = "media_with_caption"
	}
	if msg.From != nil {
		from := map[string]any{
			"id":         msg.From.ID,
			"first_name": msg.From.FirstName,
		}
		if msg.From.LastName != "" {
			from["last_name"] = msg.From.LastName
		}
		if msg.From.Username != "" {
			from["username"] = msg.From.Username
		}
		meta["from"] = from
	}
	return meta
}
