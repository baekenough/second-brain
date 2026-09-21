package calendarauto

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

type memoryStore struct {
	cutoff                    time.Time
	job                       Job
	status, eventID, retry    string
	decisionSaves, eventSaves int
}

func (s *memoryStore) Initialize(context.Context) (time.Time, error)     { return s.cutoff, nil }
func (s *memoryStore) AcquireLock(context.Context) (func(), bool, error) { return func() {}, true, nil }
func (s *memoryStore) ListJobs(context.Context, time.Time, int) ([]Job, error) {
	if s.status != "" {
		return nil, nil
	}
	return []Job{s.job}, nil
}
func (s *memoryStore) SaveDecision(_ context.Context, _ uuid.UUID, d Decision) error {
	s.job.Decision = &d
	s.decisionSaves++
	return nil
}
func (s *memoryStore) SaveEvent(_ context.Context, _ uuid.UUID, e Event) error {
	s.job.Event = &e
	s.eventSaves++
	return nil
}
func (s *memoryStore) Finish(_ context.Context, _ uuid.UUID, status, id string) error {
	s.status = status
	s.eventID = id
	return nil
}
func (s *memoryStore) Retry(_ context.Context, _ uuid.UUID, reason string) error {
	s.retry = reason
	return nil
}

type fakeEvaluator struct {
	decision               Decision
	event                  *Event
	decisions, extractions int
}

func (e *fakeEvaluator) Decide(context.Context, *model.Document, time.Time) (Decision, error) {
	e.decisions++
	return e.decision, nil
}
func (e *fakeEvaluator) Extract(context.Context, *model.Document, time.Time) (*Event, error) {
	e.extractions++
	return e.event, nil
}

type fakeWriter struct {
	calls int
	err   error
	event Event
}

func (w *fakeWriter) EnsureEvent(_ context.Context, e Event) (string, bool, error) {
	w.calls++
	w.event = e
	return "stable-id", true, w.err
}
func fixture() (*Worker, *memoryStore, *fakeEvaluator, *fakeWriter) {
	now := time.Date(2026, 9, 21, 3, 0, 0, 0, time.UTC)
	occurred := now.Add(-time.Minute)
	doc := &model.Document{ID: uuid.New(), Status: "active", SourceType: model.SourceSMS, CreatedAt: occurred, OccurredAt: &occurred, Content: "Confirmed appointment September 22 at 17:00 until 18:00."}
	s := &memoryStore{cutoff: now.Add(-time.Hour), job: Job{Document: doc}}
	e := &fakeEvaluator{decision: Decision{"create", .99}, event: &Event{Summary: "Appointment", Start: "2026-09-22T17:00:00+09:00", End: "2026-09-22T18:00:00+09:00", Evidence: doc.Content}}
	writer := &fakeWriter{}
	return New(Config{Store: s, Decider: e, Extractor: e, Writer: writer, Now: func() time.Time { return now }}), s, e, writer
}
func TestForwardOnlyAndTranscriptGate(t *testing.T) {
	for _, scenario := range []string{"old_created", "old_occurred", "missing_occurred", "pending_call", "no_recording", "calendar_source"} {
		t.Run(scenario, func(t *testing.T) {
			w, s, e, writer := fixture()
			old := s.cutoff.Add(-time.Second)
			switch scenario {
			case "old_created":
				s.job.Document.CreatedAt = old
			case "old_occurred":
				s.job.Document.OccurredAt = &old
			case "missing_occurred":
				s.job.Document.OccurredAt = nil
			case "pending_call", "no_recording":
				s.job.Document.SourceType = model.SourceCall
				s.job.Document.Metadata = map[string]any{"transcription": "pending"}
			case "calendar_source":
				s.job.Document.SourceType = model.SourceCalendar
			}
			if err := w.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if e.decisions != 0 || writer.calls != 0 {
				t.Fatal("ineligible source processed")
			}
			if scenario == "pending_call" && s.status != "" {
				t.Fatal("pending transcript was permanently finished")
			}
		})
	}
}
func TestDecisionGateBeforeExtraction(t *testing.T) {
	for _, d := range []Decision{{"skip", .99}, {"uncertain", .99}, {"create", .89}, {"create", 0}, {"bogus", 1}, {"create", 1.1}} {
		t.Run(d.Action+time.Duration(d.Confidence*100).String(), func(t *testing.T) {
			w, s, e, writer := fixture()
			e.decision = d
			if err := w.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if e.extractions != 0 || writer.calls != 0 || s.status == "" {
				t.Fatal("unapproved event reached extraction/writer")
			}
		})
	}
}
func TestRetryReusesPersistedDecisionAndEvent(t *testing.T) {
	w, s, e, writer := fixture()
	writer.err = errors.New("temporary write failure")
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.retry != "calendar_write_failed" {
		t.Fatal(s.retry)
	}
	writer.err = nil
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.decisions != 1 || e.extractions != 1 || s.decisionSaves != 1 || s.eventSaves != 1 || writer.calls != 2 || s.status != "created" {
		t.Fatalf("not resumable: %+v %+v", s, e)
	}
}
func TestConflictingCalendarEventNeedsReview(t *testing.T) {
	w, s, _, writer := fixture()
	writer.err = ErrCalendarConflict
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.status != "review" || s.retry != "" {
		t.Fatalf("conflict: %+v", s)
	}
}
func TestMissingEndUsesVisibleOneHourPlaceholder(t *testing.T) {
	w, s, e, writer := fixture()
	e.event.End = ""
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.status != "created" || writer.event.End != "2026-09-22T18:00:00+09:00" || writer.event.Description != "종료 시각 미제공: 60분 임시 일정" {
		t.Fatalf("wrong default: %+v", writer.event)
	}
}
func TestInvalidOrUnsupportedExtractionNeverWrites(t *testing.T) {
	for _, scenario := range []string{"past", "far_future", "bad_offset", "reversed", "invented_evidence", "empty", "long_source"} {
		t.Run(scenario, func(t *testing.T) {
			w, s, e, writer := fixture()
			switch scenario {
			case "past":
				e.event.Start = "2026-09-20T17:00:00+09:00"
			case "far_future":
				e.event.Start = "2028-09-22T17:00:00+09:00"
				e.event.End = "2028-09-22T18:00:00+09:00"
			case "bad_offset":
				e.event.Start = "2026-09-22T17:00:00"
			case "reversed":
				e.event.End = e.event.Start
			case "invented_evidence":
				e.event.Evidence = "This text is not in the source"
			case "empty":
				e.event = nil
			case "long_source":
				s.job.Document.Content = string(make([]rune, MaxContentRunes+1))
			}
			if err := w.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if writer.calls != 0 || s.status != "review" {
				t.Fatal("unsafe extraction wrote event")
			}
		})
	}
}
func TestExpiredPersistedEventNeverWrites(t *testing.T) {
	w, s, e, writer := fixture()
	s.job.Decision = &e.decision
	s.job.Event = e.event
	w.cfg.Now = func() time.Time { return time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC) }
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if writer.calls != 0 || s.status != "review" || e.decisions != 0 {
		t.Fatal("expired retry processed")
	}
}

func TestCallProcessedOnlyAfterTranscriptArrives(t *testing.T) {
	w, s, e, writer := fixture()
	s.job.Document.SourceType = model.SourceCall
	s.job.Document.Metadata = map[string]any{"transcription": "pending"}
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.job.Document.Metadata["transcription"] = "done"
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.decisions != 1 || writer.calls != 1 || s.status != "created" {
		t.Fatal("transcript arrival not processed")
	}
}

func TestRecheckNowAfterExtraction(t *testing.T) {
	w, s, _, writer := fixture()
	original := w.cfg.Now()
	calls := 0
	w.cfg.Now = func() time.Time {
		calls++
		if calls >= 4 {
			return original.Add(48 * time.Hour)
		}
		return original
	}
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if writer.calls != 0 || s.status != "review" {
		t.Fatal("event elapsed during extraction still written")
	}
}
