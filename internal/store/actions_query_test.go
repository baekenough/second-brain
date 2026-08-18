package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Action query store — real-database tests.
//
// This package has no SQL stub harness, and pure-function tests cannot prove a
// query is valid: this project already paid for that lesson once (referencing
// EXCLUDED inside a RETURNING clause compiled, passed stub tests, and failed at
// runtime in production). Everything below therefore runs against a real
// PostgreSQL instance addressed by TEST_DATABASE_URL and is skipped when that
// variable is unset, so `go test ./...` stays green on a machine with no
// database.
//
// TEST_DATABASE_URL must NEVER point at production. Every row these tests write
// is prefixed with the sentinel below and removed in t.Cleanup.
// ---------------------------------------------------------------------------

const (
	// actionTestKeyPrefix scopes both the seeded rows and the cleanup DELETE.
	actionTestKeyPrefix = "action:test-"
	// actionTestCounterpart is the normalized_name of the throwaway entity every
	// seeded action points at. Queries filter on it so that unrelated rows that
	// happen to live in the test database cannot influence an assertion.
	// Deliberately a nonsense token — no real person's name appears in tests.
	actionTestCounterpart = "zz-dummy-counterpart"
	// actionTestSourceID prefixes the throwaway documents seeded actions
	// reference (actions.document_id has an FK to documents.id).
	actionTestSourceID = "zz-dummy-action-test"
)

// actionTestDB connects to TEST_DATABASE_URL or skips the calling test.
func actionTestDB(t *testing.T) *Postgres {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-database action query test")
	}
	pg, err := NewPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(pg.Close)
	return pg
}

// seedActionFixtures inserts one throwaway document + one throwaway entity and
// registers the cleanup that removes every row this test file creates. Deleting
// the documents cascades to actions, which cascades to action_status.
func seedActionFixtures(t *testing.T, pg *Postgres) (uuid.UUID, int64) {
	t.Helper()
	ctx := context.Background()

	docID := uuid.New()
	_, err := pg.pool.Exec(ctx, `
		INSERT INTO documents (id, source_type, source_id, title, content, collected_at)
		VALUES ($1, 'filesystem', $2, 'dummy title', 'dummy content', now())`,
		docID, actionTestSourceID+"-"+docID.String(),
	)
	if err != nil {
		t.Fatalf("seed document: %v", err)
	}

	var entityID int64
	err = pg.pool.QueryRow(ctx, `
		INSERT INTO entities (name, type, normalized_name)
		VALUES ('dummy counterpart', 'person', $1)
		ON CONFLICT (normalized_name, type) DO UPDATE SET name = entities.name
		RETURNING id`,
		actionTestCounterpart,
	).Scan(&entityID)
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if _, err := pg.pool.Exec(cleanupCtx,
			`DELETE FROM actions WHERE identity_key LIKE $1`, actionTestKeyPrefix+"%"); err != nil {
			t.Errorf("cleanup actions: %v", err)
		}
		if _, err := pg.pool.Exec(cleanupCtx,
			`DELETE FROM documents WHERE source_id LIKE $1`, actionTestSourceID+"%"); err != nil {
			t.Errorf("cleanup documents: %v", err)
		}
		if _, err := pg.pool.Exec(cleanupCtx,
			`DELETE FROM entities WHERE normalized_name = $1`, actionTestCounterpart); err != nil {
			t.Errorf("cleanup entities: %v", err)
		}
	})

	return docID, entityID
}

// insertTestAction writes one action row. summary/counterpart are dummy tokens.
func insertTestAction(t *testing.T, pg *Postgres, docID uuid.UUID, entityID int64, key string, kind model.ActionKind, confidence float64, observedAt time.Time) {
	t.Helper()
	_, err := pg.pool.Exec(context.Background(), `
		INSERT INTO actions (identity_key, document_id, thread_key, kind, summary,
		                     counterpart_entity_id, due_at, detected_by, confidence, observed_at)
		VALUES ($1, $2, 'zz-dummy-thread', $3, 'dummy summary', $4, '1990-01-01T00:00:00Z', 'structural', $5, $6)`,
		key, docID, string(kind), entityID, confidence, observedAt,
	)
	if err != nil {
		t.Fatalf("insert action %s: %v", key, err)
	}
}

func insertTestStatus(t *testing.T, pg *Postgres, key string, state model.ActionState) {
	t.Helper()
	_, err := pg.pool.Exec(context.Background(),
		`INSERT INTO action_status (identity_key, state) VALUES ($1, $2)`, key, string(state))
	if err != nil {
		t.Fatalf("insert action_status %s: %v", key, err)
	}
}

// keysWithTestPrefix narrows a result set to rows this file seeded.
func keysWithTestPrefix(items []ActionListItem) []ActionListItem {
	out := make([]ActionListItem, 0, len(items))
	for _, it := range items {
		if strings.HasPrefix(it.IdentityKey, actionTestKeyPrefix) {
			out = append(out, it)
		}
	}
	return out
}

// TestListOpenActionsExcludesResolved pins the default behaviour: only actions
// whose effective state is 'open' are returned. An INNER JOIN on action_status
// would silently drop actions that have no status row at all, so the query must
// LEFT JOIN and COALESCE the missing state to 'open'.
func TestListOpenActionsExcludesResolved(t *testing.T) {
	pg := actionTestDB(t)
	ctx := context.Background()
	docID, entityID := seedActionFixtures(t, pg)

	now := time.Now().UTC()
	insertTestAction(t, pg, docID, entityID, actionTestKeyPrefix+"open", model.KindMyCommitment, 1.00, now)
	insertTestStatus(t, pg, actionTestKeyPrefix+"open", model.StateOpen)
	insertTestAction(t, pg, docID, entityID, actionTestKeyPrefix+"done", model.KindMyCommitment, 1.00, now)
	insertTestStatus(t, pg, actionTestKeyPrefix+"done", model.StateDone)
	insertTestAction(t, pg, docID, entityID, actionTestKeyPrefix+"ignored", model.KindMyCommitment, 1.00, now)
	insertTestStatus(t, pg, actionTestKeyPrefix+"ignored", model.StateIgnored)

	got, err := NewActionQueryStore(pg).ListOpenActions(ctx, ActionFilter{Counterpart: actionTestCounterpart})
	if err != nil {
		t.Fatalf("ListOpenActions: %v", err)
	}
	mine := keysWithTestPrefix(got)
	if len(mine) != 1 {
		t.Fatalf("want only the open action, got %d rows: %v", len(mine), identityKeysOf(mine))
	}
	if mine[0].IdentityKey != actionTestKeyPrefix+"open" {
		t.Fatalf("returned key = %q, want %q", mine[0].IdentityKey, actionTestKeyPrefix+"open")
	}
	if mine[0].State != model.StateOpen {
		t.Fatalf("State = %q, want %q", mine[0].State, model.StateOpen)
	}
	if mine[0].CounterpartName != actionTestCounterpart {
		t.Fatalf("CounterpartName = %q, want the joined entity name %q", mine[0].CounterpartName, actionTestCounterpart)
	}
}

// TestListOpenActionsKeepsStatuslessAction pins that an action with NO
// action_status row survives. Part A inserts the status row separately from the
// action row, so a crash between the two writes leaves exactly this shape; an
// INNER JOIN would make such an action invisible forever.
func TestListOpenActionsKeepsStatuslessAction(t *testing.T) {
	pg := actionTestDB(t)
	ctx := context.Background()
	docID, entityID := seedActionFixtures(t, pg)

	key := actionTestKeyPrefix + "nostatus"
	insertTestAction(t, pg, docID, entityID, key, model.KindScheduled, 0.80, time.Now().UTC())

	got, err := NewActionQueryStore(pg).ListOpenActions(ctx, ActionFilter{Counterpart: actionTestCounterpart})
	if err != nil {
		t.Fatalf("ListOpenActions: %v", err)
	}
	mine := keysWithTestPrefix(got)
	if len(mine) != 1 {
		t.Fatalf("action without an action_status row disappeared: got %v", identityKeysOf(mine))
	}
	if mine[0].State != model.StateOpen {
		t.Fatalf("State = %q, want %q for a row with no action_status", mine[0].State, model.StateOpen)
	}
}

// TestListOpenActionsClampsLimit pins that the response-size cap lives in the
// store, not only in the handler. A caller that asks for 100000 rows must not be
// able to pull the whole table into memory.
func TestListOpenActionsClampsLimit(t *testing.T) {
	pg := actionTestDB(t)
	ctx := context.Background()
	docID, entityID := seedActionFixtures(t, pg)

	const seeded = 205
	now := time.Now().UTC()
	for i := 0; i < seeded; i++ {
		insertTestAction(t, pg, docID, entityID,
			fmt.Sprintf("%sbulk-%03d", actionTestKeyPrefix, i),
			model.KindTheirCommitment, 1.00, now)
	}

	got, err := NewActionQueryStore(pg).ListOpenActions(ctx, ActionFilter{
		Counterpart: actionTestCounterpart,
		Limit:       100000,
	})
	if err != nil {
		t.Fatalf("ListOpenActions: %v", err)
	}
	if len(got) > 200 {
		t.Fatalf("Limit=100000 returned %d rows; store must clamp to 200", len(got))
	}
	if len(got) != 200 {
		t.Fatalf("returned %d rows, want exactly the 200-row clamp (seeded %d matching rows)", len(got), seeded)
	}

	// A zero Limit must fall back to the 50-row default rather than to "no limit".
	def, err := NewActionQueryStore(pg).ListOpenActions(ctx, ActionFilter{Counterpart: actionTestCounterpart})
	if err != nil {
		t.Fatalf("ListOpenActions (default limit): %v", err)
	}
	if len(def) != 50 {
		t.Fatalf("zero Limit returned %d rows, want the 50-row default", len(def))
	}
}

// readResolvedAt returns action_status.resolved_at for key, or the zero time
// when the column is NULL.
func readResolvedAt(t *testing.T, pg *Postgres, key string) time.Time {
	t.Helper()
	var at *time.Time
	err := pg.pool.QueryRow(context.Background(),
		`SELECT resolved_at FROM action_status WHERE identity_key = $1`, key).Scan(&at)
	if err != nil {
		t.Fatalf("read resolved_at for %s: %v", key, err)
	}
	if at == nil {
		return time.Time{}
	}
	return *at
}

func readActionState(t *testing.T, pg *Postgres, key string) string {
	t.Helper()
	var state string
	err := pg.pool.QueryRow(context.Background(),
		`SELECT state FROM action_status WHERE identity_key = $1`, key).Scan(&state)
	if err != nil {
		t.Fatalf("read state for %s: %v", key, err)
	}
	return state
}

// TestSetActionStateIsIdempotent pins the exact meaning of idempotence here:
// repeating a request must leave the database byte-for-byte identical.
// "ON CONFLICT DO UPDATE SET resolved_at = now()" would pass a naive
// same-state-after check while silently rewriting the timestamp on every call —
// and that timestamp is the only handle a recovery UPDATE has for isolating a
// batch of wrongly-resolved actions (see the plan's rollback section).
func TestSetActionStateIsIdempotent(t *testing.T) {
	pg := actionTestDB(t)
	ctx := context.Background()
	docID, entityID := seedActionFixtures(t, pg)
	s := NewActionQueryStore(pg)

	key := actionTestKeyPrefix + "idempotent"
	insertTestAction(t, pg, docID, entityID, key, model.KindMyCommitment, 1.00, time.Now().UTC())

	found, err := s.SetActionState(ctx, key, model.StateDone, "")
	if err != nil || !found {
		t.Fatalf("first SetActionState: found=%v err=%v", found, err)
	}
	if got := readActionState(t, pg, key); got != string(model.StateDone) {
		t.Fatalf("state = %q after first call, want %q", got, model.StateDone)
	}
	first := readResolvedAt(t, pg, key)
	if first.IsZero() {
		t.Fatal("resolved_at is NULL after a state transition; recovery UPDATEs would have nothing to filter on")
	}

	// The repeat call carries the same state — nothing about the row may move.
	time.Sleep(10 * time.Millisecond)
	found, err = s.SetActionState(ctx, key, model.StateDone, "")
	if err != nil || !found {
		t.Fatalf("repeat SetActionState: found=%v err=%v", found, err)
	}
	if second := readResolvedAt(t, pg, key); !second.Equal(first) {
		t.Fatalf("resolved_at moved on repeat call: %v -> %v (not idempotent)", first, second)
	}

	// A genuine transition, on the other hand, must advance resolved_at.
	time.Sleep(10 * time.Millisecond)
	if _, err := s.SetActionState(ctx, key, model.StateIgnored, ""); err != nil {
		t.Fatalf("transition to ignored: %v", err)
	}
	if got := readActionState(t, pg, key); got != string(model.StateIgnored) {
		t.Fatalf("state = %q, want %q", got, model.StateIgnored)
	}
	if third := readResolvedAt(t, pg, key); !third.After(first) {
		t.Fatalf("resolved_at did not advance on a real transition: %v -> %v", first, third)
	}
}

// TestSetActionStateReopen pins that 'open' is an accepted target state: a user
// who marks the wrong action done must be able to undo it.
func TestSetActionStateReopen(t *testing.T) {
	pg := actionTestDB(t)
	ctx := context.Background()
	docID, entityID := seedActionFixtures(t, pg)
	s := NewActionQueryStore(pg)

	key := actionTestKeyPrefix + "reopen"
	insertTestAction(t, pg, docID, entityID, key, model.KindScheduled, 1.00, time.Now().UTC())

	if _, err := s.SetActionState(ctx, key, model.StateDone, ""); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	if _, err := s.SetActionState(ctx, key, model.StateOpen, ""); err != nil {
		t.Fatalf("reopen: %v", err)
	}

	got, err := s.ListOpenActions(ctx, ActionFilter{Counterpart: actionTestCounterpart})
	if err != nil {
		t.Fatalf("ListOpenActions: %v", err)
	}
	if len(keysWithTestPrefix(got)) != 1 {
		t.Fatalf("reopened action is not listed as open: %v", identityKeysOf(got))
	}
}

// TestSetActionStateUnknownKey pins that a key with no matching action row is
// reported as not-found rather than as an error. The INSERT violates
// action_status' foreign key (SQLSTATE 23503); left unhandled that surfaces to
// the user as a 500 for what is really a 404.
func TestSetActionStateUnknownKey(t *testing.T) {
	pg := actionTestDB(t)
	ctx := context.Background()
	seedActionFixtures(t, pg) // registers cleanup
	s := NewActionQueryStore(pg)

	found, err := s.SetActionState(ctx, actionTestKeyPrefix+"does-not-exist", model.StateDone, "")
	if err != nil {
		t.Fatalf("unknown key returned an error instead of found=false: %v", err)
	}
	if found {
		t.Fatal("found = true for a key that was never inserted")
	}
}

// TestSetActionStateRejectsUnknownState pins that validation happens in the
// store too, not only in the handler.
func TestSetActionStateRejectsUnknownState(t *testing.T) {
	pg := actionTestDB(t)
	ctx := context.Background()
	docID, entityID := seedActionFixtures(t, pg)
	s := NewActionQueryStore(pg)

	key := actionTestKeyPrefix + "badstate"
	insertTestAction(t, pg, docID, entityID, key, model.KindScheduled, 1.00, time.Now().UTC())

	if _, err := s.SetActionState(ctx, key, model.ActionState("archived"), ""); err == nil {
		t.Fatal("store accepted a state outside the closed vocabulary")
	}
}

func identityKeysOf(items []ActionListItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.IdentityKey)
	}
	return out
}
