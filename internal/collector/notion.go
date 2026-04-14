package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/model"
)

const notionAPIVersion = "2022-06-28"

// NotionCollector collects pages from a Notion workspace.
type NotionCollector struct {
	token  string
	client *http.Client
}

// NewNotionCollector returns a NotionCollector. When token is empty the
// collector is disabled.
func NewNotionCollector(token string) *NotionCollector {
	return &NotionCollector{
		token:  token,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *NotionCollector) Name() string             { return "notion" }
func (c *NotionCollector) Source() model.SourceType { return model.SourceNotion }
func (c *NotionCollector) Enabled() bool            { return c.token != "" }

// Collect searches all pages edited after since and fetches their block content.
func (c *NotionCollector) Collect(ctx context.Context, since time.Time) ([]model.Document, error) {
	pages, err := c.searchPages(ctx, since)
	if err != nil {
		return nil, fmt.Errorf("notion search pages: %w", err)
	}

	var docs []model.Document
	for _, p := range pages {
		blocks, err := c.pageContent(ctx, p.ID)
		if err != nil {
			slog.Warn("notion: failed to fetch page content", "id", p.ID, "title", p.Title, "error", err)
			continue
		}
		docs = append(docs, model.Document{
			ID:         uuid.New(),
			SourceType: model.SourceNotion,
			SourceID:   p.ID,
			Title:      p.Title,
			Content:    blocks,
			Metadata: map[string]any{
				"url":         p.URL,
				"last_edited": p.LastEdited,
			},
			CollectedAt: time.Now().UTC(),
		})
	}

	slog.Info("notion: collected documents", "count", len(docs))
	return docs, nil
}

// --- Notion API helpers ---

type notionPage struct {
	ID         string
	Title      string
	URL        string
	LastEdited string
}

func (c *NotionCollector) searchPages(ctx context.Context, since time.Time) ([]notionPage, error) {
	var all []notionPage
	cursor := ""

	for {
		payload := map[string]interface{}{
			"filter": map[string]string{"property": "object", "value": "page"},
			"sort":   map[string]string{"direction": "descending", "timestamp": "last_edited_time"},
		}
		if cursor != "" {
			payload["start_cursor"] = cursor
		}

		var resp struct {
			Results    []json.RawMessage `json:"results"`
			HasMore    bool              `json:"has_more"`
			NextCursor string            `json:"next_cursor"`
		}
		if err := c.post(ctx, "/v1/search", payload, &resp); err != nil {
			return nil, err
		}

		for _, raw := range resp.Results {
			p, ok := c.parsePage(raw)
			if !ok {
				continue
			}
			// Stop once we reach pages older than since.
			if p.LastEdited != "" {
				t, err := time.Parse(time.RFC3339, p.LastEdited)
				if err == nil && t.Before(since) {
					return all, nil
				}
			}
			all = append(all, p)
		}

		if !resp.HasMore {
			break
		}
		cursor = resp.NextCursor
	}
	return all, nil
}

func (c *NotionCollector) parsePage(raw json.RawMessage) (notionPage, bool) {
	var obj struct {
		ID         string `json:"id"`
		URL        string `json:"url"`
		LastEdited string `json:"last_edited_time"`
		Properties struct {
			Title struct {
				Title []struct {
					PlainText string `json:"plain_text"`
				} `json:"title"`
			} `json:"title"`
			Name struct {
				Title []struct {
					PlainText string `json:"plain_text"`
				} `json:"title"`
			} `json:"Name"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return notionPage{}, false
	}

	title := ""
	for _, t := range obj.Properties.Title.Title {
		title += t.PlainText
	}
	if title == "" {
		for _, t := range obj.Properties.Name.Title {
			title += t.PlainText
		}
	}

	return notionPage{
		ID:         obj.ID,
		Title:      title,
		URL:        obj.URL,
		LastEdited: obj.LastEdited,
	}, true
}

// pageContent retrieves and concatenates the plain text of all blocks in a page.
func (c *NotionCollector) pageContent(ctx context.Context, pageID string) (string, error) {
	var sb strings.Builder
	cursor := ""

	for {
		path := fmt.Sprintf("/v1/blocks/%s/children?page_size=100", pageID)
		if cursor != "" {
			path += "&start_cursor=" + cursor
		}

		var resp struct {
			Results    []json.RawMessage `json:"results"`
			HasMore    bool              `json:"has_more"`
			NextCursor string            `json:"next_cursor"`
		}
		if err := c.get(ctx, path, &resp); err != nil {
			return "", err
		}

		for _, raw := range resp.Results {
			text := extractBlockText(raw)
			if text != "" {
				sb.WriteString(text)
				sb.WriteByte('\n')
			}
		}

		if !resp.HasMore {
			break
		}
		cursor = resp.NextCursor
	}
	return sb.String(), nil
}

// extractBlockText pulls plain text from the most common Notion block types.
func extractBlockText(raw json.RawMessage) string {
	var block struct {
		Type      string          `json:"type"`
		Paragraph json.RawMessage `json:"paragraph"`
		Heading1  json.RawMessage `json:"heading_1"`
		Heading2  json.RawMessage `json:"heading_2"`
		Heading3  json.RawMessage `json:"heading_3"`
		BulletedListItem json.RawMessage `json:"bulleted_list_item"`
		NumberedListItem json.RawMessage `json:"numbered_list_item"`
		ToDo      json.RawMessage `json:"to_do"`
		Toggle    json.RawMessage `json:"toggle"`
		Code      json.RawMessage `json:"code"`
		Quote     json.RawMessage `json:"quote"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return ""
	}

	var rich json.RawMessage
	switch block.Type {
	case "paragraph":
		rich = block.Paragraph
	case "heading_1":
		rich = block.Heading1
	case "heading_2":
		rich = block.Heading2
	case "heading_3":
		rich = block.Heading3
	case "bulleted_list_item":
		rich = block.BulletedListItem
	case "numbered_list_item":
		rich = block.NumberedListItem
	case "to_do":
		rich = block.ToDo
	case "toggle":
		rich = block.Toggle
	case "code":
		rich = block.Code
	case "quote":
		rich = block.Quote
	default:
		return ""
	}

	var content struct {
		RichText []struct {
			PlainText string `json:"plain_text"`
		} `json:"rich_text"`
	}
	if err := json.Unmarshal(rich, &content); err != nil {
		return ""
	}

	var sb strings.Builder
	for _, rt := range content.RichText {
		sb.WriteString(rt.PlainText)
	}
	return sb.String()
}

func (c *NotionCollector) post(ctx context.Context, path string, payload interface{}, dest interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.notion.com"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Notion-Version", notionAPIVersion)

	return c.do(req, dest)
}

func (c *NotionCollector) get(ctx context.Context, path string, dest interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.notion.com"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Notion-Version", notionAPIVersion)

	return c.do(req, dest)
}

func (c *NotionCollector) do(req *http.Request, dest interface{}) error {
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
		return fmt.Errorf("notion API %s: status %d: %s", req.URL.Path, res.StatusCode, b)
	}
	return json.Unmarshal(b, dest)
}
