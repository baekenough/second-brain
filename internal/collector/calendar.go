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

	// calendarStatusCancelled is the Google Calendar API event status value
	// for a deleted event or a cancelled instance of a recurring series.
	// See https://developers.google.com/calendar/api/v3/reference/events —
	// "status: cancelled" events frequently omit summary/description.
	calendarStatusCancelled = "cancelled"
)

// CalendarCancellationStore is the document persistence interface used by
// CalendarCollector to soft-delete documents for calendar events that have
// been cancelled upstream. It is a subset of store.DocumentStore.
type CalendarCancellationStore interface {
	SoftDeleteBySourceID(ctx context.Context, sourceType model.SourceType, sourceID string) (bool, error)
}

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

	// cancellationStore soft-deletes documents for cancelled calendar events.
	// When nil, cancellations are observed but not acted upon (the
	// corresponding document, if any, is left active) — see Collect.
	cancellationStore CalendarCancellationStore
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

// WithCancellationStore configures the store used to soft-delete documents
// for calendar events that are reported as cancelled. Passing nil disables
// cancellation handling (matches the pre-existing behavior).
func (c *CalendarCollector) WithCancellationStore(s CalendarCancellationStore) *CalendarCollector {
	c.cancellationStore = s
	return c
}

func (c *CalendarCollector) Name() string             { return "calendar" }
func (c *CalendarCollector) Source() model.SourceType { return model.SourceCalendar }
func (c *CalendarCollector) Enabled() bool {
	return c.cfg.CalendarCredentialsJSON != "" && c.cfg.CalendarTokenJSON != ""
}

// parseCalendarIDs parses the (possibly comma-separated) CALENDAR_ID config
// value into an ordered, de-duplicated list of calendar identifiers.
//
// Order is preserved because the FIRST entry keeps the legacy source_id
// scheme (see calendarEventToDocument) — reordering CALENDAR_ID would
// therefore change which calendar's documents keep their existing keys.
// Empty entries are dropped and whitespace trimmed. An empty result defaults
// to ["primary"], matching the pre-multi-calendar default.
func parseCalendarIDs(raw string) []string {
	parts := strings.Split(raw, ",")
	seen := make(map[string]bool, len(parts))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		id := strings.TrimSpace(p)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) == 0 {
		out = append(out, "primary")
	}
	return out
}

// Collect fetches Google Calendar events in the window
// [now - LookbehindDays, now + LookaheadDays] that were updated after since,
// across every calendar configured via CALENDAR_ID (comma-separated).
//
// A single failing calendar (revoked sharing, insufficient scope, transient
// 5xx, ...) does not abort the whole run — the remaining calendars keep
// collecting. Failures are logged and counted; only when EVERY configured
// calendar fails in the same run does Collect return an error. This matches
// this repo's "one dependency dying does not take down the rest of the
// pipeline" convention (e.g. neo4j being optional for search).
//
// Known trade-off: collector_state's watermark is keyed by (instance_id,
// source_type) only — there is no per-calendar column, and adding one would
// require a migration (out of scope for this change; see
// internal/store/document.go UpdateCollectorState). So when Collect returns
// nil (partial or full success), the scheduler advances ONE shared watermark
// for every configured calendar. A calendar that fails on some ticks and
// later recovers can therefore have a bounded gap of missed updates from
// its downtime window, once the shared watermark has moved past it. This is
// accepted here over the alternative (holding back an already-healthy
// calendar's documents whenever any other calendar is unhealthy) — the same
// resilience-over-strict-per-source-correctness trade-off this repo already
// makes elsewhere. If per-calendar precision becomes necessary, it requires
// a dedicated collector_state migration, not a workaround here.
func (c *CalendarCollector) Collect(ctx context.Context, since time.Time) ([]model.Document, error) {
	token, err := c.getAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("calendar: get access token: %w", err)
	}

	now := time.Now().UTC()
	timeMin := now.AddDate(0, 0, -c.cfg.CalendarLookbehindDays)
	timeMax := now.AddDate(0, 0, c.cfg.CalendarLookaheadDays)

	calIDs := parseCalendarIDs(c.cfg.CalendarID)
	collectAt := time.Now().UTC()

	var docs []model.Document
	var failedCalendars []string
	totalSkippedEmpty := 0
	totalCancelled := 0
	totalCancelledUnhandled := 0

	for i, calID := range calIDs {
		// The first configured calendar keeps the legacy (non-namespaced)
		// source_id scheme so upgrading from a single-calendar config never
		// re-keys existing documents. See calendarEventToDocument.
		legacy := i == 0

		events, err := c.listEvents(ctx, token, calID, timeMin, timeMax, since)
		if err != nil {
			slog.Warn("calendar: failed to list events for calendar — skipping this calendar for this run, others continue",
				"calendar_id", calID, "error", err)
			failedCalendars = append(failedCalendars, calID)
			continue
		}

		calSkippedEmpty := 0
		calCancelled := 0
		calCancelledUnhandled := 0
		for _, ev := range events {
			// A cancelled event (single deletion, or a cancelled instance of
			// a recurring series) must never become — or remain — an active
			// document. Google Calendar frequently omits summary/description
			// on a cancelled event, so this check MUST run before the
			// empty-summary skip below: otherwise a cancellation for a
			// previously-collected active event would silently fall through
			// the skip and leave its old (still-active, now stale) document
			// behind forever. Soft-deleting by source_id is a no-op when the
			// event was never collected while active, so this never
			// manufactures a new document out of a tombstone.
			if ev.Status == calendarStatusCancelled {
				calCancelled++
				sourceID := calendarSourceID(ev.ID, calID, legacy)
				if c.cancellationStore == nil {
					calCancelledUnhandled++
					continue
				}
				if _, err := c.cancellationStore.SoftDeleteBySourceID(ctx, model.SourceCalendar, sourceID); err != nil {
					slog.Warn("calendar: failed to soft-delete cancelled event",
						"calendar_id", calID, "id", ev.ID, "error", err)
				}
				continue
			}

			// A calendar shared at freeBusyReader (rather than "See all event
			// details") returns HTTP 200 with events that have no summary or
			// description — busy/free slots only, no identifiable content.
			// Ingesting these would pollute the corpus with title-less
			// documents, so they are skipped rather than collected.
			if strings.TrimSpace(ev.Summary) == "" {
				calSkippedEmpty++
				continue
			}
			doc, err := calendarEventToDocument(ev, calID, legacy, collectAt)
			if err != nil {
				slog.Warn("calendar: failed to convert event to document",
					"calendar_id", calID, "id", ev.ID, "error", err)
				continue
			}
			docs = append(docs, doc)
		}
		if calSkippedEmpty > 0 {
			// Same tone/pattern as the SMS collector's empty-source-file
			// warning: this is a permission/sharing signal, not a bug. Once
			// sharing is upgraded to "See all event details", these events
			// collect automatically on the next tick with no code change.
			slog.Warn("calendar: skipped events with no visible content — likely freeBusyReader-only sharing, not an error; will collect automatically once sharing permission is upgraded",
				"calendar_id", calID, "skipped", calSkippedEmpty, "total", len(events))
		}
		if calCancelledUnhandled > 0 {
			slog.Warn("calendar: cancellation store not configured — cancelled events observed but not soft-deleted",
				"calendar_id", calID, "cancelled", calCancelledUnhandled)
		}
		totalSkippedEmpty += calSkippedEmpty
		totalCancelled += calCancelled
		totalCancelledUnhandled += calCancelledUnhandled
	}

	if len(failedCalendars) > 0 {
		slog.Warn("calendar: one or more calendars failed this run",
			"failed_calendars", failedCalendars,
			"failed_count", len(failedCalendars),
			"configured_count", len(calIDs))
	}
	if len(failedCalendars) == len(calIDs) {
		return nil, fmt.Errorf("calendar: all %d configured calendar(s) failed: %v", len(calIDs), failedCalendars)
	}

	slog.Info("calendar: collected documents",
		"count", len(docs),
		"calendars", len(calIDs),
		"failed_calendars", len(failedCalendars),
		"skipped_empty", totalSkippedEmpty,
		"cancelled", totalCancelled,
		"cancelled_unhandled", totalCancelledUnhandled)
	return docs, nil
}

// --- Calendar API types ---

type calendarEvent struct {
	ID          string                `json:"id"`
	Summary     string                `json:"summary"`
	Description string                `json:"description"`
	Location    string                `json:"location"`
	Status      string                `json:"status"`
	HtmlLink    string                `json:"htmlLink"`
	Updated     string                `json:"updated"` // RFC3339
	Start       calendarEventDateTime `json:"start"`
	End         calendarEventDateTime `json:"end"`
	Organizer   *calendarPerson       `json:"organizer"`
	Attendees   []calendarAttendee    `json:"attendees"`
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

// listEvents retrieves all events for calID in the given window, filtered by
// updatedMin=since.
func (c *CalendarCollector) listEvents(
	ctx context.Context,
	token, calID string,
	timeMin, timeMax time.Time,
	since time.Time,
) ([]calendarEvent, error) {
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
//
// legacy controls the source_id scheme: the first calendar configured via
// CALENDAR_ID keeps the pre-multi-calendar "calendar:<eventID>" id so
// upgrading an existing single-calendar deployment never re-keys its
// existing documents (see the #144 SMS source_id lesson — a source_id
// re-key without a dedicated migration orphaned existing rows and was
// rejected twice in review; it was only safe as a one-off migration, not a
// collector-side change). Any additional calendar is namespaced with its
// calendar ID ("calendar:<calID>:<eventID>") because event IDs are only
// guaranteed unique within a single calendar, not across calendars.
func calendarEventToDocument(ev calendarEvent, calID string, legacy bool, collectAt time.Time) (model.Document, error) {
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
		"status":      ev.Status,
		"updated":     ev.Updated,
		"html_link":   ev.HtmlLink,
		"all_day":     allDay,
		"calendar_id": calID,
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
		SourceID:    calendarSourceID(ev.ID, calID, legacy),
		Title:       ev.Summary,
		Content:     content,
		Metadata:    meta,
		OccurredAt:  occurredAt,
		CollectedAt: collectAt,
	}, nil
}

// calendarSourceID computes the source_id for a calendar event, matching the
// legacy-vs-namespaced scheme documented on calendarEventToDocument. It is
// also used by the cancellation path in Collect, which needs the source_id
// without building a full document.
func calendarSourceID(eventID, calID string, legacy bool) string {
	if legacy {
		return "calendar:" + eventID
	}
	return "calendar:" + calID + ":" + eventID
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
