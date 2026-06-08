package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/model"
)

const (
	calendarScope   = "https://www.googleapis.com/auth/calendar.readonly"
	calendarBaseURL = "https://www.googleapis.com"
)

// CalendarCollector collects events from Google Calendar via the Calendar REST API.
// It is disabled when credentials or token are not configured.
type CalendarCollector struct {
	cfg *config.Config

	// httpClient and baseURL are overridable for testing.
	httpClient *http.Client
	baseURL    string

	// tokenMu guards tokenSource and cachedToken.
	tokenMu     sync.Mutex
	tokenSource oauth2.TokenSource
	cachedToken *oauth2.Token
}

// NewCalendarCollector returns a CalendarCollector configured from cfg.
// When CalendarCredentialsJSON or CalendarTokenJSON is empty, Enabled() returns false
// and the scheduler will not call Collect.
func NewCalendarCollector(cfg *config.Config) *CalendarCollector {
	return &CalendarCollector{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    calendarBaseURL,
	}
}

func (c *CalendarCollector) Name() string             { return "calendar" }
func (c *CalendarCollector) Source() model.SourceType { return model.SourceCalendar }
func (c *CalendarCollector) Enabled() bool {
	return c.cfg.CalendarCredentialsJSON != "" && c.cfg.CalendarTokenJSON != ""
}

// Collect fetches Google Calendar events in the window
// [now - LookbehindDays, now + LookaheadDays] that were updated after since.
func (c *CalendarCollector) Collect(ctx context.Context, since time.Time) ([]model.Document, error) {
	token, err := c.getAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("calendar: get access token: %w", err)
	}

	now := time.Now().UTC()
	timeMin := now.AddDate(0, 0, -c.cfg.CalendarLookbehindDays)
	timeMax := now.AddDate(0, 0, c.cfg.CalendarLookaheadDays)

	events, err := c.listEvents(ctx, token, timeMin, timeMax, since)
	if err != nil {
		return nil, fmt.Errorf("calendar: list events: %w", err)
	}

	docs := make([]model.Document, 0, len(events))
	collectAt := time.Now().UTC()
	for _, ev := range events {
		doc, err := calendarEventToDocument(ev, collectAt)
		if err != nil {
			slog.Warn("calendar: failed to convert event to document", "id", ev.ID, "error", err)
			continue
		}
		docs = append(docs, doc)
	}

	slog.Info("calendar: collected documents", "count", len(docs))
	return docs, nil
}

// --- Calendar API types ---

type calendarEvent struct {
	ID          string                 `json:"id"`
	Summary     string                 `json:"summary"`
	Description string                 `json:"description"`
	Location    string                 `json:"location"`
	Status      string                 `json:"status"`
	HtmlLink    string                 `json:"htmlLink"`
	Updated     string                 `json:"updated"` // RFC3339
	Start       calendarEventDateTime  `json:"start"`
	End         calendarEventDateTime  `json:"end"`
	Organizer   *calendarPerson        `json:"organizer"`
	Attendees   []calendarAttendee     `json:"attendees"`
}

type calendarEventDateTime struct {
	DateTime string `json:"dateTime"` // RFC3339, set for timed events
	Date     string `json:"date"`     // "YYYY-MM-DD", set for all-day events
	TimeZone string `json:"timeZone"`
}

type calendarPerson struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
}

type calendarAttendee struct {
	Email          string `json:"email"`
	DisplayName    string `json:"displayName"`
	ResponseStatus string `json:"responseStatus"`
	Organizer      bool   `json:"organizer"`
	Self           bool   `json:"self"`
}

// listEvents retrieves all events in the given window, filtered by updatedMin=since.
func (c *CalendarCollector) listEvents(
	ctx context.Context,
	token string,
	timeMin, timeMax time.Time,
	since time.Time,
) ([]calendarEvent, error) {
	calID := c.cfg.CalendarID
	if calID == "" {
		calID = "primary"
	}

	var all []calendarEvent
	pageToken := ""

	for {
		params := url.Values{
			"timeMin":      {timeMin.Format(time.RFC3339)},
			"timeMax":      {timeMax.Format(time.RFC3339)},
			"singleEvents": {"true"},
			"orderBy":      {"updated"},
			"maxResults":   {"2500"},
		}
		if !since.IsZero() && since.Unix() > 0 {
			params.Set("updatedMin", since.Format(time.RFC3339))
		}
		if pageToken != "" {
			params.Set("pageToken", pageToken)
		}

		u := fmt.Sprintf(
			"%s/calendar/v3/calendars/%s/events?%s",
			c.baseURL,
			url.PathEscape(calID),
			params.Encode(),
		)

		var resp struct {
			Items         []calendarEvent `json:"items"`
			NextPageToken string          `json:"nextPageToken"`
		}
		if err := c.calendarDoRequest(ctx, token, u, &resp); err != nil {
			return nil, err
		}

		all = append(all, resp.Items...)
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}

	return all, nil
}

// calendarEventToDocument converts a Calendar API event to a model.Document.
func calendarEventToDocument(ev calendarEvent, collectAt time.Time) (model.Document, error) {
	occurredAt, allDay := parseCalendarDateTime(ev.Start)

	// Build content: description + location + attendees summary.
	var contentParts []string
	if ev.Description != "" {
		contentParts = append(contentParts, ev.Description)
	}
	if ev.Location != "" {
		contentParts = append(contentParts, "Location: "+ev.Location)
	}
	if len(ev.Attendees) > 0 {
		emails := make([]string, 0, len(ev.Attendees))
		for _, a := range ev.Attendees {
			name := a.DisplayName
			if name == "" {
				name = a.Email
			}
			emails = append(emails, name)
		}
		contentParts = append(contentParts, "Attendees: "+strings.Join(emails, ", "))
	}
	content := strings.Join(contentParts, "\n")

	// Build metadata.
	meta := map[string]any{
		"status":    ev.Status,
		"updated":   ev.Updated,
		"html_link": ev.HtmlLink,
		"all_day":   allDay,
	}
	if ev.Location != "" {
		meta["location"] = ev.Location
	}
	if ev.Organizer != nil {
		meta["organizer"] = ev.Organizer.Email
	}
	if len(ev.Attendees) > 0 {
		attendeeList := make([]map[string]any, 0, len(ev.Attendees))
		for _, a := range ev.Attendees {
			attendeeList = append(attendeeList, map[string]any{
				"email":           a.Email,
				"display_name":    a.DisplayName,
				"response_status": a.ResponseStatus,
				"organizer":       a.Organizer,
				"self":            a.Self,
			})
		}
		meta["attendees"] = attendeeList
	}

	// End time.
	if endTime, _ := parseCalendarDateTime(ev.End); endTime != nil {
		meta["end"] = endTime.Format(time.RFC3339)
	}

	return model.Document{
		ID:          uuid.New(),
		SourceType:  model.SourceCalendar,
		SourceID:    "calendar:" + ev.ID,
		Title:       ev.Summary,
		Content:     content,
		Metadata:    meta,
		OccurredAt:  occurredAt,
		CollectedAt: collectAt,
	}, nil
}

// parseCalendarDateTime parses a CalendarEventDateTime into a *time.Time.
// Returns the parsed time and whether the event is all-day (date-only).
func parseCalendarDateTime(dt calendarEventDateTime) (*time.Time, bool) {
	if dt.DateTime != "" {
		t, err := time.Parse(time.RFC3339, dt.DateTime)
		if err == nil {
			t = t.UTC()
			return &t, false
		}
	}
	if dt.Date != "" {
		t, err := time.Parse("2006-01-02", dt.Date)
		if err == nil {
			t = t.UTC()
			return &t, true
		}
	}
	return nil, false
}

// --- OAuth2 token management ---

// getAccessToken returns a valid OAuth2 Bearer token, refreshing if necessary.
func (c *CalendarCollector) getAccessToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	if c.cachedToken != nil && c.cachedToken.Valid() {
		return c.cachedToken.AccessToken, nil
	}

	if c.tokenSource == nil {
		ts, err := c.buildCalendarTokenSource(context.Background())
		if err != nil {
			return "", err
		}
		c.tokenSource = ts
	}

	tok, err := c.tokenSource.Token()
	if err != nil {
		if strings.Contains(err.Error(), "invalid_grant") {
			slog.Error("calendar: OAuth2 refresh token is invalid or revoked — re-authentication required",
				"credentials_path", c.cfg.CalendarCredentialsJSON,
				"token_path", c.cfg.CalendarTokenJSON,
			)
		}
		return "", fmt.Errorf("calendar: fetch access token: %w", err)
	}
	c.cachedToken = tok
	return tok.AccessToken, nil
}

// buildCalendarTokenSource constructs an oauth2.TokenSource from the credentials
// and token files specified in the config. Both fields are treated as file paths.
func (c *CalendarCollector) buildCalendarTokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	credJSON, err := os.ReadFile(c.cfg.CalendarCredentialsJSON)
	if err != nil {
		return nil, fmt.Errorf("calendar: read credentials file %q: %w", c.cfg.CalendarCredentialsJSON, err)
	}

	tokenJSON, err := os.ReadFile(c.cfg.CalendarTokenJSON)
	if err != nil {
		return nil, fmt.Errorf("calendar: read token file %q: %w", c.cfg.CalendarTokenJSON, err)
	}

	// Parse credentials — reuse the same gmailCredentials shape (installed/web).
	var creds gmailCredentials
	if err := json.Unmarshal(credJSON, &creds); err != nil {
		return nil, fmt.Errorf("calendar: parse credentials JSON: %w", err)
	}

	clientID := creds.Installed.ClientID
	clientSecret := creds.Installed.ClientSecret
	tokenURI := creds.Installed.TokenURI
	if clientID == "" {
		clientID = creds.Web.ClientID
		clientSecret = creds.Web.ClientSecret
		tokenURI = creds.Web.TokenURI
	}
	if clientID == "" {
		return nil, fmt.Errorf("calendar: credentials JSON has neither 'installed' nor 'web' client_id")
	}
	if tokenURI == "" {
		tokenURI = "https://oauth2.googleapis.com/token"
	}

	var tok gmailTokenFile
	if err := json.Unmarshal(tokenJSON, &tok); err != nil {
		return nil, fmt.Errorf("calendar: parse token JSON: %w", err)
	}

	oauthCfg := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       []string{calendarScope},
	}
	if tokenURI != "" {
		oauthCfg.Endpoint.TokenURL = tokenURI
	}

	oauthToken := &oauth2.Token{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		TokenType:    tok.TokenType,
		Expiry:       tok.Expiry,
	}

	ts := oauthCfg.TokenSource(ctx, oauthToken)
	return oauth2.ReuseTokenSource(oauthToken, ts), nil
}

// calendarDoRequest performs a GET request, attaches the Bearer token,
// reads the response body, and JSON-decodes it into dest.
func (c *CalendarCollector) calendarDoRequest(ctx context.Context, token, u string, dest interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	res, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode >= 400 {
		b, _ := io.ReadAll(res.Body)
		return fmt.Errorf("calendar API %s: status %d: %s", u, res.StatusCode, b)
	}

	b, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dest)
}
