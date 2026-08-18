package worker

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// PgStructuralSignalLister — real-database test.
//
// listLatestPerThreadQuery gained two columns (title and
// COALESCE(occurred_at, collected_at)) so the action card can say WHICH
// conversation is waiting. A stub cannot prove a query compiles or that the
// scan targets line up — this project already shipped SQL that passed stub
// tests and failed in production (EXCLUDED inside RETURNING). So this runs
// against a real PostgreSQL addressed by TEST_DATABASE_URL and skips when it
// is unset.
//
// TEST_DATABASE_URL must NEVER point at production. Every row written here is
// prefixed with a sentinel and deleted in cleanup; all content is dummy.
// ---------------------------------------------------------------------------

const structuralSQLTestSourceID = "zz-dummy-structural-sql"

func structuralTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-database structural signal query test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(pool.Close)
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM documents WHERE source_id LIKE $1`, structuralSQLTestSourceID+"%"); err != nil {
			t.Errorf("cleanup documents: %v", err)
		}
	})
	return pool
}

func insertThreadDocument(t *testing.T, pool *pgxpool.Pool, sourceType, title, metadata string, occurredAt *time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO documents (id, source_type, source_id, title, content, metadata, collected_at, occurred_at, status)
		VALUES ($1, $2, $3, $4, 'dummy content', $5::jsonb, now(), $6, 'active')`,
		id, sourceType, structuralSQLTestSourceID+"-"+id.String(), title, metadata, occurredAt,
	)
	if err != nil {
		t.Fatalf("insert %s document: %v", sourceType, err)
	}
	return id
}

// TestListLatestPerThreadReturnsTitleAndEventTime pins the query's contract:
// one row per thread (the newest), carrying the two new card columns.
func TestListLatestPerThreadReturnsTitleAndEventTime(t *testing.T) {
	pool := structuralTestPool(t)

	newest := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	older := newest.Add(-48 * time.Hour)

	insertThreadDocument(t, pool, "sms", "SMS received zz-dummy", `{"contact_name":"zz-dummy-contact","direction":"received"}`, &older)
	newestID := insertThreadDocument(t, pool, "sms", "SMS received zz-dummy (newest)", `{"contact_name":"zz-dummy-contact","direction":"received"}`, &newest)

	got, err := NewPgStructuralSignalLister(pool).ListLatestPerThread(context.Background(), 500)
	if err != nil {
		t.Fatalf("ListLatestPerThread: %v", err)
	}

	var found *ThreadLatestMessage
	var sameThread int
	for i := range got {
		if name, _ := got[i].Metadata["contact_name"].(string); name == "zz-dummy-contact" {
			sameThread++
			found = &got[i]
		}
	}
	if sameThread != 1 {
		t.Fatalf("got %d rows for one thread, want 1 (the newest message only)", sameThread)
	}
	if found.DocumentID != newestID {
		t.Errorf("DocumentID = %s, want the newest message %s", found.DocumentID, newestID)
	}
	if found.Title != "SMS received zz-dummy (newest)" {
		t.Errorf("Title = %q, want the newest message's title", found.Title)
	}
	if !found.EventAt.Equal(newest) {
		t.Errorf("EventAt = %s, want %s (occurred_at)", found.EventAt, newest)
	}
}

// TestListLatestPerThreadSkipsDocumentsWithoutEventTime documents an existing
// limitation this change did NOT alter: the 30-day window is evaluated on
// occurred_at, so a thread whose newest message has none never produces an
// awaiting_my_reply candidate at all. EventAt's COALESCE to collected_at is
// therefore defence for any future caller that relaxes the window, not a
// behaviour change here — widening the window would create actions for threads
// that have never had them, which is a separate decision.
func TestListLatestPerThreadSkipsDocumentsWithoutEventTime(t *testing.T) {
	pool := structuralTestPool(t)

	insertThreadDocument(t, pool, "sms", "SMS received zz-dummy-noevent", `{"contact_name":"zz-dummy-contact-noevent","direction":"received"}`, nil)

	got, err := NewPgStructuralSignalLister(pool).ListLatestPerThread(context.Background(), 500)
	if err != nil {
		t.Fatalf("ListLatestPerThread: %v", err)
	}
	for _, m := range got {
		if name, _ := m.Metadata["contact_name"].(string); name == "zz-dummy-contact-noevent" {
			t.Fatalf("a document with NULL occurred_at was returned (EventAt=%s); the 30-day window filters on occurred_at", m.EventAt)
		}
	}
}
