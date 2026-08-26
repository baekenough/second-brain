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
		cfg:         cfg,
		httpClient:  srv.Client(),
		baseURL:     srv.URL,
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

	doc, err := calendarEventToDocument(ev, "primary", true, collectAt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if doc.SourceType != model.SourceCalendar {
		t.Errorf("SourceType = %q, want %q", doc.SourceType, model.SourceCalendar)
	}
	if doc.SourceID != "calendar:evt-001" {
		t.Errorf("SourceID = %q, want %q", doc.SourceID, "calendar:evt-001")
	}
	if doc.Metadata["calendar_id"] != "primary" {
		t.Errorf("Metadata.calendar_id = %v, want %q", doc.Metadata["calendar_id"], "primary")
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

	doc, err := calendarEventToDocument(ev, "primary", true, collectAt)
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

// --- parseCalendarIDs ---

func TestParseCalendarIDs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "empty defaults to primary", raw: "", want: []string{"primary"}},
		{name: "single value backward compat", raw: "primary", want: []string{"primary"}},
		{name: "single custom value", raw: "team@example.com", want: []string{"team@example.com"}},
		{
			name: "comma-separated, trimmed",
			raw:  "primary, baeksy@agilesoda.ai ,  team@example.com",
			want: []string{"primary", "baeksy@agilesoda.ai", "team@example.com"},
		},
		{
			name: "empty entries dropped",
			raw:  "primary,,team@example.com,",
			want: []string{"primary", "team@example.com"},
		},
		{
			name: "duplicates de-duplicated, first occurrence order kept",
			raw:  "primary,team@example.com,primary",
			want: []string{"primary", "team@example.com"},
		},
		{name: "only whitespace/commas defaults to primary", raw: " , , ", want: []string{"primary"}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseCalendarIDs(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("parseCalendarIDs(%q) = %v, want %v", tc.raw, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("parseCalendarIDs(%q)[%d] = %q, want %q", tc.raw, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// --- calendarEventToDocument source_id namespacing ---

func TestCalendarEventToDocument_NamespacedForNonLegacyCalendar(t *testing.T) {
	t.Parallel()

	ev := calendarEvent{
		ID:      "evt-shared",
		Summary: "Shared Event",
		Status:  "confirmed",
		Updated: time.Now().UTC().Format(time.RFC3339),
		Start:   calendarEventDateTime{Date: "2024-07-04"},
		End:     calendarEventDateTime{Date: "2024-07-05"},
	}

	legacyDoc, err := calendarEventToDocument(ev, "primary", true, time.Now().UTC())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if legacyDoc.SourceID != "calendar:evt-shared" {
		t.Errorf("legacy SourceID = %q, want %q", legacyDoc.SourceID, "calendar:evt-shared")
	}

	namespacedDoc, err := calendarEventToDocument(ev, "baeksy@agilesoda.ai", false, time.Now().UTC())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantID := "calendar:baeksy@agilesoda.ai:evt-shared"
	if namespacedDoc.SourceID != wantID {
		t.Errorf("namespaced SourceID = %q, want %q", namespacedDoc.SourceID, wantID)
	}
	if namespacedDoc.Metadata["calendar_id"] != "baeksy@agilesoda.ai" {
		t.Errorf("Metadata.calendar_id = %v, want %q", namespacedDoc.Metadata["calendar_id"], "baeksy@agilesoda.ai")
	}

	// Same underlying event ID across two calendars must not collide once
	// namespaced — this is the whole point of the legacy/namespaced split.
	if legacyDoc.SourceID == namespacedDoc.SourceID {
		t.Error("legacy and namespaced source_id must differ for the same event ID across calendars")
	}
}

// --- Collect: multiple calendars ---

// TestCalendarCollector_Collect_MultipleCalendars verifies that Collect
// iterates every configured calendar, that the first (primary) calendar keeps
// its legacy source_id, that additional calendars are namespaced, and that
// metadata.calendar_id is populated for every document (issue: no way to
// tell which calendar a document came from before this change).
func TestCalendarCollector_Collect_MultipleCalendars(t *testing.T) {
	t.Parallel()

	start := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	mux := http.NewServeMux()
	mux.HandleFunc("/calendar/v3/calendars/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "primary"):
			ev := buildCalendarEventJSON("evt-primary", "Primary Event", "", "", "confirmed", start, end, "", nil)
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{ev}})
		case strings.Contains(r.URL.Path, "baeksy"):
			ev := buildCalendarEventJSON("evt-work", "Work Event", "", "", "confirmed", start, end, "", nil)
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{ev}})
		default:
			t.Errorf("unexpected calendar path: %q", r.URL.Path)
			_ = json.NewEncoder(w).Encode(map[string]any{"items": nil})
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := validCalendarConfig()
	cfg.CalendarID = "primary,baeksy@agilesoda.ai"
	c := newCalendarCollectorWithFakeSource(cfg, srv, nil)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("got %d docs, want 2", len(docs))
	}

	byID := make(map[string]model.Document, len(docs))
	for _, d := range docs {
		byID[d.SourceID] = d
	}

	primaryDoc, ok := byID["calendar:evt-primary"]
	if !ok {
		t.Fatalf("missing legacy-keyed primary doc; got source_ids: %v", keysOf(byID))
	}
	if primaryDoc.Metadata["calendar_id"] != "primary" {
		t.Errorf("primary doc calendar_id = %v, want %q", primaryDoc.Metadata["calendar_id"], "primary")
	}

	workDoc, ok := byID["calendar:baeksy@agilesoda.ai:evt-work"]
	if !ok {
		t.Fatalf("missing namespaced work doc; got source_ids: %v", keysOf(byID))
	}
	if workDoc.Metadata["calendar_id"] != "baeksy@agilesoda.ai" {
		t.Errorf("work doc calendar_id = %v, want %q", workDoc.Metadata["calendar_id"], "baeksy@agilesoda.ai")
	}
}

// TestCalendarCollector_Collect_SingleValueStillLegacy verifies that a
// single, non-default CALENDAR_ID value (the pre-multi-calendar shape) still
// produces the legacy, non-namespaced source_id — i.e. existing deployments
// with CALENDAR_ID set to one calendar are unaffected by this change.
func TestCalendarCollector_Collect_SingleValueStillLegacy(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/calendar/v3/calendars/", func(w http.ResponseWriter, r *http.Request) {
		ev := buildAllDayEventJSON("evt-solo", "Solo Calendar Event", "2024-07-04")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{ev}})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := validCalendarConfig()
	cfg.CalendarID = "team@example.com"
	c := newCalendarCollectorWithFakeSource(cfg, srv, nil)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d docs, want 1", len(docs))
	}
	if docs[0].SourceID != "calendar:evt-solo" {
		t.Errorf("SourceID = %q, want %q (legacy scheme for sole configured calendar)", docs[0].SourceID, "calendar:evt-solo")
	}
}

// TestCalendarCollector_Collect_OneCalendarFails_OthersContinue verifies that
// a failing calendar (e.g. 403 due to insufficient scope) does not abort the
// whole Collect call — the healthy calendar's documents are still returned
// and Collect returns nil (so the scheduler commits them and advances the
// watermark).
func TestCalendarCollector_Collect_OneCalendarFails_OthersContinue(t *testing.T) {
	t.Parallel()

	start := time.Now().UTC()
	end := start.Add(time.Hour)

	mux := http.NewServeMux()
	mux.HandleFunc("/calendar/v3/calendars/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "primary"):
			w.Header().Set("Content-Type", "application/json")
			ev := buildCalendarEventJSON("evt-ok", "Healthy Calendar Event", "", "", "confirmed", start, end, "", nil)
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{ev}})
		case strings.Contains(r.URL.Path, "revoked"):
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error": {"code": 403, "message": "insufficient permission"}}`))
		default:
			t.Errorf("unexpected calendar path: %q", r.URL.Path)
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := validCalendarConfig()
	cfg.CalendarID = "primary,revoked@example.com"
	c := newCalendarCollectorWithFakeSource(cfg, srv, nil)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: unexpected error, want nil (partial success): %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d docs, want 1 (only the healthy calendar)", len(docs))
	}
	if docs[0].SourceID != "calendar:evt-ok" {
		t.Errorf("SourceID = %q, want %q", docs[0].SourceID, "calendar:evt-ok")
	}
}

// TestCalendarCollector_Collect_AllCalendarsFail verifies that when every
// configured calendar fails, Collect returns an error (so the scheduler does
// not advance the shared watermark and retries from the same `since` on the
// next tick).
func TestCalendarCollector_Collect_AllCalendarsFail(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/calendar/v3/calendars/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error": {"code": 403, "message": "insufficient permission"}}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := validCalendarConfig()
	cfg.CalendarID = "primary,revoked@example.com"
	c := newCalendarCollectorWithFakeSource(cfg, srv, nil)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err == nil {
		t.Fatal("Collect: want error when all calendars fail, got nil")
	}
	if docs != nil {
		t.Errorf("got %d docs, want nil when all calendars fail", len(docs))
	}
}

// TestCalendarCollector_Collect_SkipsEmptySummaryEvents verifies the
// content-empty guard: an event with no summary (the freeBusyReader-only
// sharing symptom observed in production) is skipped, while a normal event
// in the same response is still collected.
func TestCalendarCollector_Collect_SkipsEmptySummaryEvents(t *testing.T) {
	t.Parallel()

	start := time.Now().UTC()
	end := start.Add(time.Hour)

	mux := http.NewServeMux()
	mux.HandleFunc("/calendar/v3/calendars/primary/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		empty := buildCalendarEventJSON("evt-empty", "", "", "", "confirmed", start, end, "", nil)
		whitespaceOnly := buildCalendarEventJSON("evt-whitespace", "   ", "", "", "confirmed", start, end, "", nil)
		visible := buildCalendarEventJSON("evt-visible", "Visible Meeting", "", "", "confirmed", start, end, "", nil)
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{empty, whitespaceOnly, visible}})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := newCalendarCollectorWithFakeSource(validCalendarConfig(), srv, nil)
	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d docs, want 1 (only the event with a visible summary)", len(docs))
	}
	if docs[0].SourceID != "calendar:evt-visible" {
		t.Errorf("SourceID = %q, want %q", docs[0].SourceID, "calendar:evt-visible")
	}
}

// TestCalendarCollector_Collect_NoSourceIDCollisionAcrossCalendars verifies
// that the same underlying event ID appearing in two different calendars
// does not collide into a single source_id — the second calendar's copy is
// namespaced and both documents are collected.
func TestCalendarCollector_Collect_NoSourceIDCollisionAcrossCalendars(t *testing.T) {
	t.Parallel()

	start := time.Now().UTC()
	end := start.Add(time.Hour)
	const sharedEventID = "evt-collision"

	mux := http.NewServeMux()
	mux.HandleFunc("/calendar/v3/calendars/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "primary"):
			ev := buildCalendarEventJSON(sharedEventID, "Primary Copy", "", "", "confirmed", start, end, "", nil)
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{ev}})
		case strings.Contains(r.URL.Path, "secondary"):
			ev := buildCalendarEventJSON(sharedEventID, "Secondary Copy", "", "", "confirmed", start, end, "", nil)
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{ev}})
		default:
			t.Errorf("unexpected calendar path: %q", r.URL.Path)
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := validCalendarConfig()
	cfg.CalendarID = "primary,secondary@example.com"
	c := newCalendarCollectorWithFakeSource(cfg, srv, nil)

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("got %d docs, want 2 (no collision — both copies collected)", len(docs))
	}

	ids := map[string]bool{docs[0].SourceID: true, docs[1].SourceID: true}
	if len(ids) != 2 {
		t.Fatalf("source_id collision: both docs got the same source_id %v", ids)
	}
	if !ids["calendar:"+sharedEventID] {
		t.Errorf("missing legacy-keyed primary copy, got: %v", ids)
	}
	if !ids["calendar:secondary@example.com:"+sharedEventID] {
		t.Errorf("missing namespaced secondary copy, got: %v", ids)
	}
}

// keysOf returns the keys of a source_id-keyed document map, for test failure messages.
func keysOf(m map[string]model.Document) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
