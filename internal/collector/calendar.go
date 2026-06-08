package collector

// TODO(#calendar): implement Google Calendar collector using the Calendar REST API.
// This stub satisfies the collector.Collector interface so that the binary
// compiles and the scheduler can be registered without a real implementation.
//
// Implementation notes:
//   - Use CalendarCredentialsJSON + CalendarTokenJSON (OAuth2) from config.Config.
//   - List events on cfg.CalendarID in the window:
//       [now - CalendarLookbehindDays, now + CalendarLookaheadDays]
//     AND filter by updatedMin=since for incremental runs.
//   - Convert each event to a model.Document with SourceType=SourceCalendar.
//   - SourceID format: "calendar:<event-id>"
//   - OccurredAt: use the event start time (dateTime or date).
//   - Title: event summary; Content: description + location + attendees.

import (
	"context"
	"time"

	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/model"
)

// CalendarCollector collects events from Google Calendar via the Calendar REST API.
// It is disabled when credentials or token are not configured.
type CalendarCollector struct {
	cfg *config.Config
}

// NewCalendarCollector returns a CalendarCollector configured from cfg.
// When CalendarCredentialsJSON or CalendarTokenJSON is empty, Enabled() returns false
// and the scheduler will not call Collect.
func NewCalendarCollector(cfg *config.Config) *CalendarCollector {
	return &CalendarCollector{cfg: cfg}
}

func (c *CalendarCollector) Name() string             { return "calendar" }
func (c *CalendarCollector) Source() model.SourceType { return model.SourceCalendar }
func (c *CalendarCollector) Enabled() bool {
	return c.cfg.CalendarCredentialsJSON != "" && c.cfg.CalendarTokenJSON != ""
}

// Collect is not yet implemented.
// TODO(#calendar): implement incremental Google Calendar event collection.
func (c *CalendarCollector) Collect(_ context.Context, _ time.Time) ([]model.Document, error) {
	return nil, nil
}
