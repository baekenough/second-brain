package graph

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// seedDummyGraph builds a 3-entity / 4-relation graph through the projector,
// so the read tests exercise exactly the shape the projection produces.
// Names are synthesised dummies (privacy policy §4).
func seedDummyGraph(t *testing.T, p *Projector) (ctx context.Context, observed time.Time) {
	t.Helper()
	ctx = context.Background()
	if err := p.Wipe(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	observed = time.Now().UTC().Add(-1 * time.Hour)

	rel := func(id, from, to int64, fromName, toName, fromType, toType string, rt model.RelationType, doc string) model.ProjectionRelation {
		return model.ProjectionRelation{
			ID: id, FromEntityID: from, FromName: fromName, FromType: fromType,
			ToEntityID: to, ToName: toName, ToType: toType, Type: rt,
			EvidenceDocumentID: doc, Confidence: 0.9, ObservedAt: observed,
		}
	}
	rows := []model.ProjectionRelation{
		rel(1, 11, 22, "더미갑", "더미을", "PERSON", "PERSON", model.RelationCommunicatedWith, "00000000-0000-0000-0000-0000000000a1"),
		rel(2, 11, 22, "더미갑", "더미을", "PERSON", "PERSON", model.RelationCommunicatedWith, "00000000-0000-0000-0000-0000000000a2"),
		rel(3, 11, 33, "더미갑", "더미주제", "PERSON", "CONCEPT", model.RelationAboutTopic, "00000000-0000-0000-0000-0000000000a3"),
		rel(4, 22, 33, "더미을", "더미주제", "PERSON", "CONCEPT", model.RelationAboutTopic, "00000000-0000-0000-0000-0000000000a3"),
	}
	if err := p.UpsertRelations(ctx, rows); err != nil {
		t.Fatal(err)
	}
	mentions := []model.ProjectionMention{
		{DocumentID: "00000000-0000-0000-0000-0000000000a1", SourceType: "sms", OccurredAt: &observed,
			EntityID: 11, EntityName: "더미갑", EntityType: "PERSON"},
		{DocumentID: "00000000-0000-0000-0000-0000000000a2", SourceType: "sms", OccurredAt: &observed,
			EntityID: 11, EntityName: "더미갑", EntityType: "PERSON"},
		{DocumentID: "00000000-0000-0000-0000-0000000000a3", SourceType: "gmail", OccurredAt: nil,
			EntityID: 22, EntityName: "더미을", EntityType: "PERSON"},
	}
	if err := p.UpsertMentions(ctx, mentions); err != nil {
		t.Fatal(err)
	}
	return ctx, observed
}

func TestReaderIntegration_EntryExpandEvidenceSearch(t *testing.T) {
	c := newTestClient(t)
	p := NewProjector(c)
	r := NewReader(c)
	ctx, observed := seedDummyGraph(t, p)
	since := observed.Add(-24 * time.Hour)

	// Entry points: 더미갑 (id 11) has degree 3, the highest.
	entries, err := r.EntryPoints(ctx, EntryFilter{Since: since, MinConfidence: 0.5, Limit: 999})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("entry nodes = %d, want 3", len(entries))
	}
	if entries[0].PgID != 11 || entries[0].Degree != 3 {
		t.Fatalf("top entry = %+v, want pgId 11 with degree 3", entries[0])
	}

	// Type filter.
	topics, err := r.EntryPoints(ctx, EntryFilter{Since: since, MinConfidence: 0.5, EntityTypes: []string{"CONCEPT"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(topics) != 1 || topics[0].PgID != 33 {
		t.Fatalf("CONCEPT-filtered entries = %+v, want only pgId 33", topics)
	}

	// Expand: 더미갑 has 2 neighbours; the COMMUNICATED_WITH edge to 22 was
	// observed twice, so weight is 2 — parallel edges, counted at query time.
	neighbors, err := r.Expand(ctx, ExpandFilter{EntityPgID: 11, Since: since, MinConfidence: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if len(neighbors) != 2 {
		t.Fatalf("neighbours = %d, want 2", len(neighbors))
	}
	if neighbors[0].PgID != 22 || neighbors[0].Weight != 2 || neighbors[0].Direction != "out" {
		t.Fatalf("top neighbour = %+v, want pgId 22 weight 2 direction out", neighbors[0])
	}

	// Rel-type filter.
	filtered, err := r.Expand(ctx, ExpandFilter{EntityPgID: 11, Since: since, MinConfidence: 0.5,
		RelTypes: []string{"ABOUT_TOPIC"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].RelType != "ABOUT_TOPIC" {
		t.Fatalf("ABOUT_TOPIC-filtered neighbours = %+v", filtered)
	}

	// Evidence: two documents back the 11–22 COMMUNICATED_WITH pair.
	evidence, err := r.Evidence(ctx, 11, 22, "COMMUNICATED_WITH", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 2 {
		t.Fatalf("evidence rows = %d, want 2", len(evidence))
	}

	// Search: prefix match on normalizedName.
	hits, err := r.SearchEntities(ctx, "더미", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("search hits = %d, want 3", len(hits))
	}
}

// TestReaderIntegration_ExpandPlanUsesIndexSeek pins the supernode defence:
// expansion must enter through the pgId uniqueness index, never a full scan.
func TestReaderIntegration_ExpandPlanUsesIndexSeek(t *testing.T) {
	c := newTestClient(t)
	p := NewProjector(c)
	r := NewReader(c)
	ctx, _ := seedDummyGraph(t, p)
	_ = r

	out, err := c.Read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		res, err := tx.Run(ctx, "EXPLAIN "+expandQuery, map[string]any{
			"pgId": int64(11), "since": sinceParam(time.Unix(0, 0)), "minConfidence": 0.0,
			"relTypes": []any{}, "limit": 10,
		})
		if err != nil {
			return nil, err
		}
		summary, err := res.Consume(ctx)
		if err != nil {
			return nil, err
		}
		return planOperators(summary.Plan()), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ops, _ := out.(string)
	if !strings.Contains(ops, "IndexSeek") {
		t.Errorf("expand plan has no index seek: %s", ops)
	}
	if strings.Contains(ops, "AllNodesScan") {
		t.Errorf("expand plan falls back to AllNodesScan: %s", ops)
	}
}

func planOperators(p neo4j.Plan) string {
	if p == nil {
		return ""
	}
	out := p.Operator()
	for _, child := range p.Children() {
		out += " " + planOperators(child)
	}
	return out
}
