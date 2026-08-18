package store

import (
	"context"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// ActionStore.UpsertAction — counterpart resolution, real-database tests.
//
// These cover the second half of the "50 identical, nameless cards" defect:
// counterpart_entity_id was NULL for 51% of rows because nothing ever resolved
// one for awaiting_my_reply. The resolution now happens here, in the write
// path, and only a real database can prove the CTE-free two-step actually
// writes the id and that ON CONFLICT does not erase it again.
//
// Skipped unless TEST_DATABASE_URL is set. It must NEVER point at production:
// every row written below is prefixed with a sentinel and deleted in cleanup.
// ---------------------------------------------------------------------------

const (
	upsertTestKeyPrefix  = "action:test-cp-"
	upsertTestSourceID   = "zz-dummy-upsert-test"
	upsertTestEntityName = "ZZ-Dummy-Counterpart-Label"
	// normalizeEntityName's output for the label above — what the UI reads
	// back as counterpart_name.
	upsertTestEntityNormalized = "zz-dummy-counterpart-label"
)

// seedUpsertDocument inserts one throwaway document for actions.document_id's
// FK and registers cleanup for every row this file creates.
func seedUpsertDocument(t *testing.T, pg *Postgres) uuid.UUID {
	t.Helper()
	docID := uuid.New()
	_, err := pg.pool.Exec(context.Background(), `
		INSERT INTO documents (id, source_type, source_id, title, content, collected_at)
		VALUES ($1, 'filesystem', $2, 'dummy title', 'dummy content', now())`,
		docID, upsertTestSourceID+"-"+docID.String(),
	)
	if err != nil {
		t.Fatalf("seed document: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := pg.pool.Exec(ctx, `DELETE FROM actions WHERE identity_key LIKE $1`, upsertTestKeyPrefix+"%"); err != nil {
			t.Errorf("cleanup actions: %v", err)
		}
		if _, err := pg.pool.Exec(ctx, `DELETE FROM documents WHERE source_id LIKE $1`, upsertTestSourceID+"%"); err != nil {
			t.Errorf("cleanup documents: %v", err)
		}
		if _, err := pg.pool.Exec(ctx, `DELETE FROM entities WHERE normalized_name = $1`, upsertTestEntityNormalized); err != nil {
			t.Errorf("cleanup entities: %v", err)
		}
	})
	return docID
}

// readCounterpart returns the stored counterpart_entity_id and the joined
// entity's normalized_name (empty when the id is NULL) — i.e. exactly what the
// /actions query renders.
func readCounterpart(t *testing.T, pg *Postgres, identityKey string) (*int64, string) {
	t.Helper()
	var (
		id   *int64
		name string
	)
	err := pg.pool.QueryRow(context.Background(), `
		SELECT a.counterpart_entity_id, COALESCE(e.normalized_name, '')
		FROM actions a LEFT JOIN entities e ON e.id = a.counterpart_entity_id
		WHERE a.identity_key = $1`, identityKey,
	).Scan(&id, &name)
	if err != nil {
		t.Fatalf("read counterpart for %s: %v", identityKey, err)
	}
	return id, name
}

func dummyAction(docID uuid.UUID, identityKey string) model.Action {
	return model.Action{
		IdentityKey: identityKey,
		DocumentID:  docID,
		ThreadKey:   "zz-dummy-thread",
		Kind:        model.KindAwaitingMyReply,
		Summary:     "dummy summary",
		DetectedBy:  model.DetectedStructural,
		Confidence:  1.0,
		ObservedAt:  time.Date(2026, 8, 18, 3, 0, 0, 0, time.UTC),
	}
}

// TestUpsertActionResolvesCounterpartName is the core of the fix: a writer
// that knows only a label gets a counterpart_entity_id, so the card can show
// who is waiting.
func TestUpsertActionResolvesCounterpartName(t *testing.T) {
	pg := actionTestDB(t)
	docID := seedUpsertDocument(t, pg)
	store := NewActionStore(pg)
	key := upsertTestKeyPrefix + "resolve"

	a := dummyAction(docID, key)
	a.CounterpartName = upsertTestEntityName
	a.CounterpartType = model.EntityTypePerson
	if err := store.UpsertAction(context.Background(), a); err != nil {
		t.Fatalf("UpsertAction: %v", err)
	}

	id, name := readCounterpart(t, pg, key)
	if id == nil {
		t.Fatal("counterpart_entity_id is NULL; the label was not resolved")
	}
	if name != upsertTestEntityNormalized {
		t.Errorf("counterpart_name = %q, want %q", name, upsertTestEntityNormalized)
	}
}

// TestUpsertActionDedupsCounterpartAcrossActions pins that two threads with
// the same counterpart converge on ONE entity row — otherwise the /actions
// counterpart filter would need to match several ids for one person.
func TestUpsertActionDedupsCounterpartAcrossActions(t *testing.T) {
	pg := actionTestDB(t)
	docID := seedUpsertDocument(t, pg)
	store := NewActionStore(pg)
	ctx := context.Background()

	first := dummyAction(docID, upsertTestKeyPrefix+"dedup-1")
	first.CounterpartName = upsertTestEntityName
	first.CounterpartType = model.EntityTypePerson
	second := dummyAction(docID, upsertTestKeyPrefix+"dedup-2")
	// Same person, differently cased — normalization must fold them together.
	second.CounterpartName = "  zz-dummy-COUNTERPART-label  "
	second.CounterpartType = model.EntityTypePerson

	for _, a := range []model.Action{first, second} {
		if err := store.UpsertAction(ctx, a); err != nil {
			t.Fatalf("UpsertAction %s: %v", a.IdentityKey, err)
		}
	}

	id1, _ := readCounterpart(t, pg, first.IdentityKey)
	id2, _ := readCounterpart(t, pg, second.IdentityKey)
	if id1 == nil || id2 == nil {
		t.Fatalf("counterpart ids = (%v, %v), want both resolved", id1, id2)
	}
	if *id1 != *id2 {
		t.Errorf("counterpart ids %d and %d differ for the same normalized name", *id1, *id2)
	}
}

// TestUpsertActionBackfillsNullCounterpart is what makes the 450 existing NULL
// rows repair themselves: the structural worker re-asserts every open thread
// on its next tick, and the ON CONFLICT branch must accept the newly resolved
// id instead of leaving the old NULL in place.
func TestUpsertActionBackfillsNullCounterpart(t *testing.T) {
	pg := actionTestDB(t)
	docID := seedUpsertDocument(t, pg)
	store := NewActionStore(pg)
	ctx := context.Background()
	key := upsertTestKeyPrefix + "backfill"

	// Pre-fix state: an action written with no counterpart at all.
	if err := store.UpsertAction(ctx, dummyAction(docID, key)); err != nil {
		t.Fatalf("UpsertAction (first): %v", err)
	}
	if id, _ := readCounterpart(t, pg, key); id != nil {
		t.Fatalf("counterpart_entity_id = %d before the fix-up write, want NULL", *id)
	}

	next := dummyAction(docID, key)
	next.CounterpartName = upsertTestEntityName
	next.CounterpartType = model.EntityTypePerson
	if err := store.UpsertAction(ctx, next); err != nil {
		t.Fatalf("UpsertAction (second): %v", err)
	}

	id, name := readCounterpart(t, pg, key)
	if id == nil {
		t.Fatal("counterpart_entity_id is still NULL after a re-upsert that carried a label")
	}
	if name != upsertTestEntityNormalized {
		t.Errorf("counterpart_name = %q, want %q", name, upsertTestEntityNormalized)
	}
}

// TestUpsertActionKeepsResolvedCounterpartOnNamelessReupsert is the mirror
// guard. The LLM extraction worker can re-assert the same identity_key without
// a label; that must not blank a counterpart the structural worker resolved.
func TestUpsertActionKeepsResolvedCounterpartOnNamelessReupsert(t *testing.T) {
	pg := actionTestDB(t)
	docID := seedUpsertDocument(t, pg)
	store := NewActionStore(pg)
	ctx := context.Background()
	key := upsertTestKeyPrefix + "keep"

	labelled := dummyAction(docID, key)
	labelled.CounterpartName = upsertTestEntityName
	labelled.CounterpartType = model.EntityTypePerson
	if err := store.UpsertAction(ctx, labelled); err != nil {
		t.Fatalf("UpsertAction (labelled): %v", err)
	}
	before, _ := readCounterpart(t, pg, key)
	if before == nil {
		t.Fatal("setup failed: counterpart was not resolved")
	}

	nameless := dummyAction(docID, key)
	nameless.DetectedBy = model.DetectedLLM
	if err := store.UpsertAction(ctx, nameless); err != nil {
		t.Fatalf("UpsertAction (nameless): %v", err)
	}

	after, name := readCounterpart(t, pg, key)
	if after == nil {
		t.Fatal("a nameless re-upsert erased the resolved counterpart")
	}
	if *after != *before {
		t.Errorf("counterpart id changed %d -> %d", *before, *after)
	}
	if name != upsertTestEntityNormalized {
		t.Errorf("counterpart_name = %q, want %q", name, upsertTestEntityNormalized)
	}
}

// TestUpsertActionPrefersExplicitEntityID keeps the extraction worker's
// existing behaviour authoritative: when it has already resolved an entity for
// a my_commitment/their_commitment/scheduled action, that id wins over any
// label.
func TestUpsertActionPrefersExplicitEntityID(t *testing.T) {
	pg := actionTestDB(t)
	docID := seedUpsertDocument(t, pg)
	entityStore := NewEntityStore(pg)
	ctx := context.Background()

	explicitID, err := entityStore.UpsertEntity(ctx, upsertTestEntityName, model.EntityTypePerson)
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	key := upsertTestKeyPrefix + "explicit"
	a := dummyAction(docID, key)
	a.CounterpartEntityID = &explicitID
	// A label that would resolve to a DIFFERENT row if it were consulted.
	a.CounterpartName = "zz-dummy-should-be-ignored"
	a.CounterpartType = model.EntityTypeOther
	if err := NewActionStore(pg).UpsertAction(ctx, a); err != nil {
		t.Fatalf("UpsertAction: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pg.pool.Exec(context.Background(),
			`DELETE FROM entities WHERE normalized_name = $1`, "zz-dummy-should-be-ignored"); err != nil {
			t.Errorf("cleanup stray entity: %v", err)
		}
	})

	id, _ := readCounterpart(t, pg, key)
	if id == nil || *id != explicitID {
		t.Errorf("counterpart_entity_id = %v, want %d (explicit id must win)", id, explicitID)
	}
	var strays int
	if err := pg.pool.QueryRow(ctx,
		`SELECT count(*) FROM entities WHERE normalized_name = $1`, "zz-dummy-should-be-ignored").Scan(&strays); err != nil {
		t.Fatalf("count stray entities: %v", err)
	}
	if strays != 0 {
		t.Errorf("%d entity rows created from an ignored label; resolution must be skipped entirely", strays)
	}
}

// TestUpsertActionUnresolvableLabelStillWritesAction guards the failure mode
// that matters operationally: an entity upsert that fails (blank label after
// normalization) must not cost the user the action row itself.
func TestUpsertActionUnresolvableLabelStillWritesAction(t *testing.T) {
	pg := actionTestDB(t)
	docID := seedUpsertDocument(t, pg)
	ctx := context.Background()
	key := upsertTestKeyPrefix + "blank-label"

	a := dummyAction(docID, key)
	a.CounterpartName = "   " // normalizes to "", which UpsertEntity rejects
	a.CounterpartType = model.EntityTypePerson
	if err := NewActionStore(pg).UpsertAction(ctx, a); err != nil {
		t.Fatalf("UpsertAction returned %v; an unusable label must not fail the action write", err)
	}

	id, _ := readCounterpart(t, pg, key)
	if id != nil {
		t.Errorf("counterpart_entity_id = %d, want NULL", *id)
	}
}
