package collector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/model"
)

// --- helpers ---

// newCalendarCollectorWithFakeSource creates a CalendarCollector with an already-built
// token source so tests bypass file I/O and network calls.
func newCalendarCollectorWithFakeSource(cfg *config.Config, srv *httptest.Server, ts oauth2.TokenSource) *CalendarCollector {
	return &CalendarCollector{
		cfg:        cfg,
		httpClient: srv.Client(),
		baseURL:    srv.URL,
		tokenSource: ts,
		cachedToken: &oauth2.Token{
			AccessToken: "test-token",
			Expiry:      time.Now().Add(time.Hour),
		},
	}
}

// validCalendarConfig returns a minimal config with non-empty credential paths.
func validCalendarConfig() *config.Config {
	return &config.Config{
		CalendarCredentialsJSON: "/fake/credentials.json",
		CalendarTokenJSON:       "/fake/token.json",
		CalendarID:              "primary",
		CalendarLookaheadDays:   90,
		CalendarLookbehindDays:  30,
	}
}

// buildCalendarEventJSON builds a timed (non-all-day) event payload.
func buildCalendarEventJSON(id, summary, description, location, status string,
	startDT, endDT time.Time, organizer string, attendees []map[string]any,
) map[string]any {
	ev := map[string]any{
		"id":          id,
		"summary":     summary,
		"description": description,
		"location":    location,
		"status":      status,
		"htmlLink":    "https://calendar.google.com/event?eid=" + id,
		"updated":     time.Now().UTC().Format(time.RFC3339),
		"start":       map[string]any{"dateTime": startDT.Format(time.RFC3339)},
		"end":         map[string]any{"dateTime": endDT.Format(time.RFC3339)},
	}
	if organizer != "" {
		ev["organizer"] = map[string]any{"email": organizer}
	}
	if len(attendees) > 0 {
		ev["attendees"] = attendees
	}
	return ev
}

// buildAllDayEventJSON builds an all-day event payload.
func buildAllDayEventJSON(id, summary, dateStr string) map[string]any {
	return map[string]any{
		"id":      id,
		"summary": summary,
		"status":  "confirmed",
		"updated": time.Now().UTC().Format(time.RFC3339),
		"start":   map[string]any{"date": dateStr},
		"end":     map[string]any{"date": dateStr},
	}
}

// --- Enabled() ---

func TestCalendarCollector_Enabled(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{
			name: "both paths set — enabled",
			cfg:  &config.Config{CalendarCredentialsJSON: "/creds.json", CalendarTokenJSON: "/token.json"},
			want: true,
		},
		{
			name: "credentials missing — disabled",
			cfg:  &config.Config{CalendarCredentialsJSON: "", CalendarTokenJSON: "/token.json"},
			want: false,
		},
		{
			name: "token missing — disabled",
			cfg:  &config.Config{CalendarCredentialsJSON: "/creds.json", CalendarTokenJSON: ""},
			want: false,
		},
		{
			name: "both empty — disabled",
			cfg:  &config.Config{},
			want: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := NewCalendarCollector(tc.cfg)
			if c.Enabled() != tc.want {
				t.Errorf("Enabled() = %v, want %v", c.Enabled(), tc.want)
			}
		})
	}
}

// --- parseCalendarDateTime ---

func TestParseCalendarDateTime_TimedEvent(t *testing.T) {
	t.Parallel()

	want := time.Date(2024, 3, 15, 9, 0, 0, 0, time.UTC)
	dt := calendarEventDateTime{DateTime: want.Format(time.RFC3339)}
	got, allDay := parseCalendarDateTime(dt)
	if got == nil {
		t.Fatal("got nil, want non-nil")
	}
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", *got, want)
	}
	if allDay {
		t.Error("allDay = true, want false for timed event")
	}
}

func TestParseCalendarDateTime_AllDayEvent(t *testing.T) {
	t.Parallel()

	dt := calendarEventDateTime{Date: "2024-03-15"}
	got, allDay := parseCalendarDateTime(dt)
	if got == nil {
		t.Fatal("got nil, want non-nil")
	}
	if !allDay {
		t.Error("allDay = false, want true for all-day event")
	}
	if got.Year() != 2024 || got.Month() != 3 || got.Day() != 15 {
		t.Errorf("unexpected date: %v", *got)
	}
}

func TestParseCalendarDateTime_Empty(t *testing.T) {
	t.Parallel()

	dt := calendarEventDateTime{}
	got, allDay := parseCalendarDateTime(dt)
	if got != nil {
		t.Errorf("expected nil, got %v", *got)
	}
	if allDay {
		t.Error("allDay = true, want false for empty datetime")
	}
}

// --- calendarEventToDocument ---

func TestCalendarEventToDocument_TimedEvent(t *testing.T) {
	t.Parallel()

	start := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	collectAt := time.Now().UTC()

	ev := calendarEvent{
		ID:          "evt-001",
		Summary:     "Team Meeting",
		Description: "Weekly sync",
		Location:    "Conference Room A",
		Status:      "confirmed",
		HtmlLink:    "https://calendar.google.com/event?eid=evt-001",
		Updated:     time.Now().UTC().Format(time.RFC3339),
		Start:       calendarEventDateTime{DateTime: start.Format(time.RFC3339)},
		End:         calendarEventDateTime{DateTime: end.Format(time.RFC3339)},
		Organizer:   &calendarPerson{Email: "organizer@example.com"},
		Attendees: []calendarAttendee{
			{Email: "alice@example.com", DisplayName: "Alice", ResponseStatus: "accepted"},
			{Email: "bob@example.com", ResponseStatus: "tentative"},
		},
	}

	doc, err := calendarEventToDocument(ev, collectAt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if doc.SourceType != model.SourceCalendar {
		t.Errorf("SourceType = %q, want %q", doc.SourceType, model.SourceCalendar)
	}
	if doc.SourceID != "calendar:evt-001" {
		t.Errorf("SourceID = %q, want %q", doc.SourceID, "calendar:evt-001")
	}
	if doc.Title != "Team Meeting" {
		t.Errorf("Title = %q, want %q", doc.Title, "Team Meeting")
	}
	if !strings.Contains(doc.Content, "Weekly sync") {
		t.Errorf("Content missing description: %q", doc.Content)
	}
	if !strings.Contains(doc.Content, "Conference Room A") {
		t.Errorf("Content missing location: %q", doc.Content)
	}
	if !strings.Contains(doc.Content, "Alice") {
		t.Errorf("Content missing attendee Alice: %q", doc.Content)
	}
	if doc.OccurredAt == nil {
		t.Error("OccurredAt is nil, want non-nil")
	} else if !doc.OccurredAt.Equal(start) {
		t.Errorf("OccurredAt = %v, want %v", *doc.OccurredAt, start)
	}
	if doc.Metadata["status"] != "confirmed" {
		t.Errorf("Metadata.status = %v, want 'confirmed'", doc.Metadata["status"])
	}
	if doc.Metadata["organizer"] != "organizer@example.com" {
		t.Errorf("Metadata.organizer = %v, want organizer@example.com", doc.Metadata["organizer"])
	}
	if doc.Metadata["all_day"] != false {
		t.Errorf("Metadata.all_day = %v, want false", doc.Metadata["all_day"])
	}
}

func TestCalendarEventToDocument_AllDayEvent(t *testing.T) {
	t.Parallel()

	collectAt := time.Now().UTC()
	ev := calendarEvent{
		ID:      "all-day-001",
		Summary: "Company Holiday",
		Status:  "confirmed",
		Updated: time.Now().UTC().Format(time.RFC3339),
		Start:   calendarEventDateTime{Date: "2024-07-04"},
		End:     calendarEventDateTime{Date: "2024-07-05"},
	}

	doc, err := calendarEventToDocument(ev, collectAt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc.Metadata["all_day"] != true {
		t.Errorf("Metadata.all_day = %v, want true", doc.Metadata["all_day"])
	}
	if doc.OccurredAt == nil {
		t.Error("OccurredAt is nil, want non-nil for all-day event")
	}
}

// --- Collect with httptest server ---

func TestCalendarCollector_Collect_SingleEvent(t *testing.T) {
	t.Parallel()

	start := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	mux := http.NewServeMux()
	mux.HandleFunc("/calendar/v3/calendars/primary/events", func(w http.ResponseWriter, r *http.Request) {
		// Verify timeMin, timeMax, singleEvents are present.
		q := r.URL.Query()
		if q.Get("singleEvents") != "true" {
			t.Errorf("singleEvents should be true, got %q", q.Get("singleEvents"))
		}
		if q.Get("timeMin") == "" {
			t.Error("timeMin should be set")
		}
		if q.Get("timeMax") == "" {
			t.Error("timeMax should be set")
		}
		// updatedMin should be set since since is non-zero.
		if q.Get("updatedMin") == "" {
			t.Error("updatedMin should be set when since is non-zero")
		}

		ev := buildCalendarEventJSON(
			"evt-001", "Team Meeting", "Weekly sync", "Room A", "confirmed",
			start, end, "org@example.com",
			[]map[string]any{
				{"email": "alice@example.com", "displayName": "Alice", "responseStatus": "accepted"},
			},
		)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{ev}})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := validCalendarConfig()
	c := newCalendarCollectorWithFakeSource(cfg, srv, nil)

	since := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	docs, err := c.Collect(context.Background(), since)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d docs, want 1", len(docs))
	}

	doc := docs[0]
	if doc.SourceType != model.SourceCalendar {
		t.Errorf("SourceType = %q, want %q", doc.SourceType, model.SourceCalendar)
	}
	if doc.SourceID != "calendar:evt-001" {
		t.Errorf("SourceID = %q, want %q", doc.SourceID, "calendar:evt-001")
	}
	if doc.Title != "Team Meeting" {
		t.Errorf("Title = %q, want %q", doc.Title, "Team Meeting")
	}
	if doc.OccurredAt == nil {
		t.Error("OccurredAt is nil")
	}
}

func TestCalendarCollector_Collect_Pagination(t *testing.T) {
	t.Parallel()

	start := time.Now().UTC()
	end := start.Add(time.Hour)
	callCount := 0

	mux := http.NewServeMux()
	mux.HandleFunc("/calendar/v3/calendars/primary/events", func(w http.ResponseWriter, r *http.Request) {
		callCount++
		pt := r.URL.Query().Get("pageToken")
		w.Header().Set("Content-Type", "application/json")

		ev1 := buildCalendarEventJSON("evt-page1", "Event Page 1", "", "", "confirmed", start, end, "", nil)
		ev2 := buildCalendarEventJSON("evt-page2", "Event Page 2", "", "", "confirmed", start, end, "", nil)

		if pt == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items":         []any{ev1},
				"nextPageToken": "page2token",
			})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []any{ev2},
			})
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := newCalendarCollectorWithFakeSource(validCalendarConfig(), srv, nil)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 2 {
		t.Errorf("got %d docs, want 2", len(docs))
	}
	if callCount != 2 {
		t.Errorf("events list called %d times, want 2 (pagination)", callCount)
	}
}

func TestCalendarCollector_Collect_SinceZero_NoUpdatedMin(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/calendar/v3/calendars/primary/events", func(w http.ResponseWriter, r *http.Request) {
		updatedMin := r.URL.Query().Get("updatedMin")
		if updatedMin != "" {
			t.Errorf("zero since should not produce updatedMin, got %q", updatedMin)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": nil})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := newCalendarCollectorWithFakeSource(validCalendarConfig(), srv, nil)
	_, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
}

func TestCalendarCollector_Collect_AllDayEvent(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/calendar/v3/calendars/primary/events", func(w http.ResponseWriter, r *http.Request) {
		ev := buildAllDayEventJSON("all-day-001", "Public Holiday", "2024-07-04")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{ev}})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := newCalendarCollectorWithFakeSource(validCalendarConfig(), srv, nil)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d docs, want 1", len(docs))
	}
	if docs[0].Metadata["all_day"] != true {
		t.Errorf("Metadata.all_day = %v, want true", docs[0].Metadata["all_day"])
	}
	if docs[0].OccurredAt == nil {
		t.Error("OccurredAt is nil for all-day event")
	}
}

func TestCalendarCollector_Collect_CustomCalendarID(t *testing.T) {
	t.Parallel()

	const customID = "my-custom-calendar@group.calendar.google.com"
	mux := http.NewServeMux()

	handlerCalled := false
	// The calendar ID is URL path-escaped in the request URL.
	mux.HandleFunc("/calendar/v3/calendars/", func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		if !strings.Contains(r.URL.Path, "my-custom-calendar") {
			t.Errorf("expected custom calendar ID in path, got %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": nil})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := validCalendarConfig()
	cfg.CalendarID = customID
	c := newCalendarCollectorWithFakeSource(cfg, srv, nil)

	_, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !handlerCalled {
		t.Error("calendar events handler was not called")
	}
}

func TestCalendarCollector_Collect_EmptyItems(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/calendar/v3/calendars/primary/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": nil})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := newCalendarCollectorWithFakeSource(validCalendarConfig(), srv, nil)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("got %d docs, want 0", len(docs))
	}
}
