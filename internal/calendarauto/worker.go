package calendarauto

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/baekenough/second-brain/internal/model"
)

const MaxContentRunes = 24000
const MinimumConfidence = 0.9

type Worker struct{ cfg Config }

func New(cfg Config) *Worker {
	if cfg.Store == nil || cfg.Decider == nil || cfg.Extractor == nil || cfg.Writer == nil {
		panic("calendar automation dependencies required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 10
	}
	return &Worker{cfg: cfg}
}

func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	for {
		if err := w.Tick(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("calendar automation tick failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) Tick(ctx context.Context) error {
	release, acquired, err := w.cfg.Store.AcquireLock(ctx)
	if err != nil {
		return err
	}
	if !acquired {
		return nil
	}
	defer release()
	cutoff, err := w.cfg.Store.Initialize(ctx)
	if err != nil {
		return err
	}
	jobs, err := w.cfg.Store.ListJobs(ctx, cutoff, w.cfg.BatchSize)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.process(ctx, job, cutoff); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) process(ctx context.Context, job Job, cutoff time.Time) error {
	doc := job.Document
	if doc == nil {
		return errors.New("calendar automation: missing document")
	}
	finish := func(status string) error { return w.cfg.Store.Finish(ctx, doc.ID, status, "") }
	// A pending transcript must remain eligible when it arrives, even if a
	// stale/racing store snapshot accidentally supplies the initial call log.
	if doc.SourceType == model.SourceCall && doc.Metadata["transcription"] != "done" {
		return nil
	}
	if !eligible(doc, cutoff) {
		return finish("skipped")
	}
	if len([]rune(doc.Content)) > MaxContentRunes {
		return finish("review")
	}
	d := job.Decision
	if d == nil {
		decision, err := w.cfg.Decider.Decide(ctx, doc, w.cfg.Now())
		if err != nil {
			return w.cfg.Store.Retry(ctx, doc.ID, "decision_failed")
		}
		if err := w.cfg.Store.SaveDecision(ctx, doc.ID, decision); err != nil {
			return err
		}
		d = &decision
	}
	if d.Action == "skip" {
		return finish("skipped")
	}
	if d.Action != "create" || math.IsNaN(d.Confidence) || math.IsInf(d.Confidence, 0) || d.Confidence < MinimumConfidence || d.Confidence > 1 {
		return finish("review")
	}
	event := job.Event
	if event == nil {
		var err error
		event, err = w.cfg.Extractor.Extract(ctx, doc, w.cfg.Now())
		if err != nil {
			return w.cfg.Store.Retry(ctx, doc.ID, "extraction_failed")
		}
		if event == nil {
			return finish("review")
		}
		if strings.TrimSpace(event.End) == "" {
			if start, err := time.Parse(time.RFC3339, event.Start); err == nil {
				event.End = start.Add(time.Hour).Format(time.RFC3339)
				event.Description = strings.TrimSpace(event.Description + "\n종료 시각 미제공: 60분 임시 일정")
			}
		}
		if !validEvent(*event, doc, w.cfg.Now()) {
			return finish("review")
		}
		if err := w.cfg.Store.SaveEvent(ctx, doc.ID, *event); err != nil {
			return err
		}
	}
	// Check again after extraction/persistence: an appointment may have started meanwhile.
	if !validEvent(*event, doc, w.cfg.Now()) {
		return finish("review")
	}
	eventID, created, err := w.cfg.Writer.EnsureEvent(ctx, *event)
	if errors.Is(err, ErrCalendarConflict) {
		return finish("review")
	}
	if err != nil {
		return w.cfg.Store.Retry(ctx, doc.ID, "calendar_write_failed")
	}
	if eventID == "" {
		return w.cfg.Store.Retry(ctx, doc.ID, "calendar_empty_event_id")
	}
	status := "created"
	if !created {
		status = "duplicate"
	}
	return w.cfg.Store.Finish(ctx, doc.ID, status, eventID)
}

func eligible(doc *model.Document, cutoff time.Time) bool {
	if doc.Status != "active" || doc.CreatedAt.Before(cutoff) || doc.OccurredAt == nil || doc.OccurredAt.Before(cutoff) {
		return false
	}
	switch doc.SourceType {
	case model.SourceGmail, model.SourceSMS:
		return true
	case model.SourceCall:
		return doc.Metadata["transcription"] == "done"
	default:
		return false
	}
}

func validEvent(e Event, doc *model.Document, now time.Time) bool {
	start, err := time.Parse(time.RFC3339, e.Start)
	if err != nil {
		return false
	}
	end, err := time.Parse(time.RFC3339, e.End)
	if err != nil {
		return false
	}
	evidence := strings.TrimSpace(e.Evidence)
	return strings.TrimSpace(e.Summary) != "" && len(e.Summary) <= 1024 && start.After(now) && !start.After(now.AddDate(1, 0, 0)) && end.After(start) && end.Sub(start) <= 7*24*time.Hour && len([]rune(evidence)) >= 8 && strings.Contains(doc.Content, evidence)
}
