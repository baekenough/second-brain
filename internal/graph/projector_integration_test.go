package graph

import (
	"context"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// All fixtures below use synthesised dummy names (더미갑/더미을). Real personal
// data never enters a test fixture (privacy policy §4).

func newTestProjector(t *testing.T) *Projector {
	t.Helper()
	return NewProjector(newTestClient(t)) // skips when NEO4J_TEST_URI is unset
}

func countScalar(t *testing.T, p *Projector, cypher string) int64 {
	t.Helper()
	ctx := context.Background()
	out, err := p.c.Read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		res, err := tx.Run(ctx, cypher, nil)
		if err != nil {
			return nil, err
		}
		rec, err := res.Single(ctx)
		if err != nil {
			return nil, err
		}
		v, _ := rec.Get("n")
		return v, nil
	})
	if err != nil {
		t.Fatalf("count query %q: %v", cypher, err)
	}
	n, ok := out.(int64)
	if !ok {
		t.Fatalf("count query %q returned %T, want int64", cypher, out)
	}
	return n
}

func countRels(t *testing.T, p *Projector) int64 {
	return countScalar(t, p, `MATCH ()-[r]->() RETURN count(r) AS n`)
}

func countNodes(t *testing.T, p *Projector, label string) int64 {
	// label is a test-local constant, never user input.
	return countScalar(t, p, `MATCH (x:`+label+`) RETURN count(x) AS n`)
}

// Every test in this file is named *Integration* on purpose: the documented
// verification command is `go test ./internal/graph/ -run Integration`, and a
// name that does not contain "Integration" is silently excluded from it — the
// tests would look "green" while never having touched a server.
//
// TestProjectorIntegration_UpsertRelations_Idempotent is the load-bearing test of this
// whole design: the relationship MERGE keys on pgId (= entity_relations.id),
// so replaying the same Postgres rows must never grow the graph. Every
// rebuild/overlap strategy elsewhere assumes this holds.
func TestProjectorIntegration_UpsertRelations_Idempotent(t *testing.T) {
	p := newTestProjector(t)
	ctx := context.Background()
	if err := p.Wipe(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	rows := []model.ProjectionRelation{{
		ID: 1, FromEntityID: 11, FromName: "더미갑", FromType: "PERSON",
		ToEntityID: 22, ToName: "더미을", ToType: "PERSON",
		Type:               model.RelationCommunicatedWith,
		EvidenceDocumentID: "00000000-0000-0000-0000-0000000000aa",
		Confidence:         0.9, ObservedAt: time.Now().UTC(),
	}}
	for i := 0; i < 2; i++ {
		if err := p.UpsertRelations(ctx, rows); err != nil {
			t.Fatal(err)
		}
	}
	if got := countRels(t, p); got != 1 {
		t.Fatalf("rel count after 2 identical upserts = %d, want 1", got)
	}
	if got := countNodes(t, p, "Entity"); got != 2 {
		t.Fatalf("Entity count = %d, want 2", got)
	}
	if got := countNodes(t, p, "Person"); got != 2 {
		t.Fatalf("Person label count = %d, want 2", got)
	}
}

// TestProjectorIntegration_UpsertRelations_SkipsUnknownType pins that a relation type
// outside the whitelist is dropped rather than concatenated into Cypher.
func TestProjectorIntegration_UpsertRelations_SkipsUnknownType(t *testing.T) {
	p := newTestProjector(t)
	ctx := context.Background()
	if err := p.Wipe(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	rows := []model.ProjectionRelation{{
		ID: 2, FromEntityID: 11, FromName: "더미갑", FromType: "PERSON",
		ToEntityID: 22, ToName: "더미을", ToType: "PERSON",
		Type:               model.RelationType("MENTIONS`]->() DETACH DELETE n //"),
		EvidenceDocumentID: "00000000-0000-0000-0000-0000000000aa",
		Confidence:         0.9, ObservedAt: time.Now().UTC(),
	}}
	if err := p.UpsertRelations(ctx, rows); err != nil {
		t.Fatalf("unknown type should be skipped, not error: %v", err)
	}
	if got := countRels(t, p); got != 0 {
		t.Fatalf("rel count = %d, want 0 (row must be skipped)", got)
	}
}

// TestProjectorIntegration_UpsertMentions_Idempotent covers the second projection pass
// and the privacy invariant that no document text reaches Neo4j.
func TestProjectorIntegration_UpsertMentions_Idempotent(t *testing.T) {
	p := newTestProjector(t)
	ctx := context.Background()
	if err := p.Wipe(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	when := time.Now().UTC()
	rows := []model.ProjectionMention{
		{DocumentID: "00000000-0000-0000-0000-0000000000ab", SourceType: "sms", OccurredAt: &when,
			EntityID: 11, EntityName: "더미갑", EntityType: "PERSON"},
		{DocumentID: "00000000-0000-0000-0000-0000000000ac", SourceType: "sms", OccurredAt: nil,
			EntityID: 11, EntityName: "더미갑", EntityType: "PERSON"},
	}
	for i := 0; i < 2; i++ {
		if err := p.UpsertMentions(ctx, rows); err != nil {
			t.Fatal(err)
		}
	}
	if got := countRels(t, p); got != 2 {
		t.Fatalf("MENTIONED_IN count = %d, want 2", got)
	}
	if got := countNodes(t, p, "Document"); got != 2 {
		t.Fatalf("Document count = %d, want 2", got)
	}
	if got := countScalar(t, p,
		`MATCH (d:Document) WHERE d.title IS NOT NULL OR d.content IS NOT NULL RETURN count(d) AS n`); got != 0 {
		t.Fatalf("%d Document nodes carry title/content — privacy invariant broken", got)
	}
}

func TestProjectorIntegration_State_RoundTrip(t *testing.T) {
	p := newTestProjector(t)
	ctx := context.Background()
	if err := p.Wipe(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.SetLastRelationID(ctx, 42); err != nil {
		t.Fatal(err)
	}
	st, err := p.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.LastRelationID != 42 {
		t.Fatalf("LastRelationID = %d, want 42", st.LastRelationID)
	}
	if err := p.SetResetToken(ctx, "token-1"); err != nil {
		t.Fatal(err)
	}
	st, err = p.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.ResetToken != "token-1" || st.LastRelationID != 42 {
		t.Fatalf("state = %+v, want {42 token-1}", st)
	}
}

// TestProjectorIntegration_Wipe_ClearsWatermark pins that a rebuild also drops the
// watermark — otherwise the next cycle would resume mid-stream over an empty
// graph and silently lose everything before the watermark.
func TestProjectorIntegration_Wipe_ClearsWatermark(t *testing.T) {
	p := newTestProjector(t)
	ctx := context.Background()
	if err := p.SetLastRelationID(ctx, 99); err != nil {
		t.Fatal(err)
	}
	if err := p.Wipe(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := p.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.LastRelationID != 0 || st.ResetToken != "" {
		t.Fatalf("state after Wipe = %+v, want zero values", st)
	}
}
