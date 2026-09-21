// Package calendarauto registers confirmed future appointments from new communications.
package calendarauto

import (
	"context"
	"errors"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

var ErrCalendarConflict = errors.New("calendar automation: ambiguous overlapping event")

type Decision struct {
	Action string `json:"action"`
	// Confidence stores the selected class probability, not Jev concentration.
	Confidence float64 `json:"confidence"`
}

type Event struct {
	Summary     string `json:"summary"`
	Start       string `json:"start"`
	End         string `json:"end"`
	Location    string `json:"location"`
	Description string `json:"description"`
	Evidence    string `json:"evidence"`
}

type Job struct {
	Document *model.Document
	Decision *Decision
	Event    *Event
	Attempts int
}

type Store interface {
	Initialize(context.Context) (time.Time, error)
	AcquireLock(context.Context) (release func(), acquired bool, err error)
	ListJobs(context.Context, time.Time, int) ([]Job, error)
	SaveDecision(context.Context, uuid.UUID, Decision) error
	SaveEvent(context.Context, uuid.UUID, Event) error
	Finish(context.Context, uuid.UUID, string, string) error
	Retry(context.Context, uuid.UUID, string) error
}

type Decider interface {
	Decide(context.Context, *model.Document, time.Time) (Decision, error)
}

type Extractor interface {
	Extract(context.Context, *model.Document, time.Time) (*Event, error)
}

type Writer interface {
	EnsureEvent(context.Context, Event) (eventID string, created bool, err error)
}

type Config struct {
	Store     Store
	Decider   Decider
	Extractor Extractor
	Writer    Writer
	Interval  time.Duration
	BatchSize int
	Now       func() time.Time
}
