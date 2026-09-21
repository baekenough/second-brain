package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/calendarauto"
	"github.com/google/uuid"
)

func TestCalendarAutomationForwardOnlySQL(t *testing.T) {
	for _, required := range []string{"d.created_at >= $1", "d.occurred_at >= $1", "d.status = 'active'", "'gmail', 'sms', 'call'", "d.metadata->>'transcription' = 'done'"} {
		if !strings.Contains(calendarAutomationEligibility, required) {
			t.Fatalf("eligibility missing %q", required)
		}
	}
	data, err := os.ReadFile("../../migrations/035_calendar_automation.sql")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "INSERT INTO calendar_automation_state") {
		t.Fatal("migration must not activate the worker before it is enabled")
	}
}

func TestCalendarAutomationDB(t *testing.T) {
	pg := actionTestDB(t)
	ctx := context.Background()
	s := NewCalendarAutomationStore(pg)
	cutoff, err := s.Initialize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Initialize(ctx)
	if err != nil || !again.Equal(cutoff) {
		t.Fatalf("activation cutoff changed: %v", err)
	}
	// A future fixture cutoff keeps unrelated documents out of this test.
	cutoff = time.Now().UTC().Add(24 * time.Hour)
	before, after := cutoff.Add(-time.Hour), cutoff.Add(time.Hour)
	type fixture struct {
		source, status, transcript string
		created                    time.Time
		occurred                   *time.Time
		want                       bool
	}
	fixtures := []fixture{
		{"gmail", "active", "", after, &after, true},
		{"sms", "active", "", after, &after, true},
		{"call", "active", "done", after, &after, true},
		{"call", "active", "pending", after, &after, false},
		{"gmail", "active", "", before, &after, false},
		{"sms", "active", "", after, &before, false},
		{"gmail", "active", "", after, nil, false},
		{"gmail", "deleted", "", after, &after, false},
		{"filesystem", "active", "", after, &after, false},
	}
	expected := map[uuid.UUID]bool{}
	var first uuid.UUID
	for i, f := range fixtures {
		id := uuid.New()
		_, err := pg.pool.Exec(ctx, `INSERT INTO documents(id, source_type, source_id, title, content, metadata, status, collected_at, created_at, occurred_at)
            VALUES ($1, $2, $3, 'calendar test', 'fixture', jsonb_build_object('transcription', $4::text), $5, $6, $6, $7)`, id, f.source, "calendar-auto-test-"+id.String(), f.transcript, f.status, f.created, f.occurred)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = pg.pool.Exec(context.Background(), `DELETE FROM documents WHERE id=$1`, id) })
		if f.want {
			expected[id] = true
		}
		if i == 0 {
			first = id
		}
	}
	jobs, err := s.ListJobs(ctx, cutoff, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != len(expected) {
		t.Fatalf("got %d eligible jobs, want %d", len(jobs), len(expected))
	}
	for _, j := range jobs {
		if !expected[j.Document.ID] {
			t.Fatalf("ineligible document queued: %s", j.Document.ID)
		}
	}
	decision := calendarauto.Decision{Action: "register", Confidence: 0.99}
	event := calendarauto.Event{Summary: "fixture appointment", Start: after.Format(time.RFC3339)}
	if err := s.SaveDecision(ctx, first, decision); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEvent(ctx, first, event); err != nil {
		t.Fatal(err)
	}
	if err := s.Retry(ctx, first, "private upstream error must not be persisted"); err != nil {
		t.Fatal(err)
	}
	var status, reason string
	var attempts int
	var next time.Time
	if err := pg.pool.QueryRow(ctx, `SELECT status, retry_reason, attempts, next_attempt_at FROM calendar_automation_jobs WHERE document_id=$1`, first).Scan(&status, &reason, &attempts, &next); err != nil {
		t.Fatal(err)
	}
	if status != "retry" || reason != "processing" || attempts != 1 || !next.After(time.Now()) {
		t.Fatalf("unexpected retry: %s %s %d %v", status, reason, attempts, next)
	}
	if _, err := pg.pool.Exec(ctx, `UPDATE calendar_automation_jobs SET next_attempt_at=now() WHERE document_id=$1`, first); err != nil {
		t.Fatal(err)
	}
	jobs, err = s.ListJobs(ctx, cutoff, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, j := range jobs {
		if j.Document.ID == first {
			found = true
			if j.Decision == nil || *j.Decision != decision || j.Event == nil || *j.Event != event || j.Attempts != 1 {
				t.Fatal("retry lost persisted decision or event")
			}
		}
	}
	if !found {
		t.Fatal("due retry absent")
	}
	for i := 1; i < calendarAutomationMaxAttempts; i++ {
		if err := s.Retry(ctx, first, "calendar_write_failed"); err != nil {
			t.Fatal(err)
		}
	}
	if err := pg.pool.QueryRow(ctx, `SELECT status, attempts FROM calendar_automation_jobs WHERE document_id=$1`, first).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || attempts != calendarAutomationMaxAttempts {
		t.Fatalf("retry limit not enforced: %s %d", status, attempts)
	}
	for id := range expected {
		if id != first {
			if err := s.Finish(ctx, id, "created", "fixture-event"); err != nil {
				t.Fatal(err)
			}
		}
	}
	jobs, err = s.ListJobs(ctx, cutoff, 100)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("terminal jobs requeued: %d %v", len(jobs), err)
	}
}

func TestCalendarAutomationLockDB(t *testing.T) {
	pg := actionTestDB(t)
	s := NewCalendarAutomationStore(pg)
	ctx, cancel := context.WithCancel(context.Background())
	release, acquired, err := s.AcquireLock(ctx)
	if err != nil || !acquired {
		t.Fatalf("first lock: %t %v", acquired, err)
	}
	defer release()
	_, acquired, err = s.AcquireLock(context.Background())
	if err != nil || acquired {
		t.Fatalf("second lock: %t %v", acquired, err)
	}
	cancel()
	release()
	release() // Cleanup is idempotent, including after context cancellation.
	releaseAgain, acquired, err := s.AcquireLock(context.Background())
	if err != nil || !acquired {
		t.Fatalf("lock leaked after release: %t %v", acquired, err)
	}
	releaseAgain()
}
