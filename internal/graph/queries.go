package graph

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// This file is the entire read surface of the graph. Four constant queries,
// every value bound as a parameter, every result bounded by a server-side
// clamp. There is deliberately no way to execute caller-supplied Cypher: the
// catalogue is the API.

// Result-size limits. Defaults are what the UI asks for; maxima are what the
// server will actually return no matter what the caller sends.
const (
	entryLimitDefault = 20
	entryLimitMax     = 50

	expandLimitDefault = 50
	expandLimitMax     = 200

	evidenceLimitDefault = 20
	evidenceLimitMax     = 100

	searchLimitDefault = 10
	searchLimitMax     = 50
)

// EntryFilter selects the starting nodes of an exploration.
type EntryFilter struct {
	Since         time.Time
	MinConfidence float64
	EntityTypes   []string
	Limit         int
}

// ExpandFilter selects one node's neighbours (always exactly one hop).
type ExpandFilter struct {
	EntityPgID    int64
	Since         time.Time
	MinConfidence float64
	RelTypes      []string
	Limit         int
}

// EntryNode is one candidate entry point, ranked by degree within the filter.
type EntryNode struct {
	PgID   int64  `json:"entity_id"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Degree int64  `json:"degree"`
}

// Neighbor is one entity adjacent to the expanded node. Weight is the number
// of parallel edges of that type (one edge per Postgres row), i.e. how many
// times the relation was observed.
type Neighbor struct {
	PgID      int64     `json:"entity_id"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	RelType   string    `json:"rel_type"`
	Direction string    `json:"direction"` // "out" | "in"
	Weight    int64     `json:"weight"`
	LastSeen  time.Time `json:"last_seen"`
}

// EvidenceRef points at the document that justified one observation. It
// carries no document text — the UI links to the existing document API, which
// is behind the same auth.
type EvidenceRef struct {
	DocumentID string     `json:"document_id"`
	SourceType string     `json:"source_type"`
	OccurredAt *time.Time `json:"occurred_at"`
	Confidence float64    `json:"confidence"`
	ObservedAt time.Time  `json:"observed_at"`
}

// EntityHit is one entity matched by prefix search.
type EntityHit struct {
	PgID int64  `json:"entity_id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// Reader executes the fixed query catalogue. Every method goes through
// Client.Read (read-mode session, 5s transaction timeout).
type Reader struct {
	c *Client
}

// NewReader returns a Reader backed by c.
func NewReader(c *Client) *Reader { return &Reader{c: c} }

// errReaderNoClient keeps a zero-value Reader (used by unit tests that only
// exercise validation) from panicking.
var errReaderNoClient = errors.New("graph: reader has no client")

// --- query catalogue ---

const (
	// entryQuery is the only query that scans relationships globally. At a few
	// thousand relationships that costs milliseconds; the plan flags 500k as
	// the point to add an observedAt relationship index. The e.pgId tiebreak
	// keeps the screen stable across reloads when degrees tie.
	entryQuery = `
		MATCH (e:Entity)-[r]-(:Entity)
		WHERE r.observedAt >= datetime($since) AND r.confidence >= $minConfidence
		  AND (size($types) = 0 OR e.type IN $types)
		WITH e, count(r) AS degree
		ORDER BY degree DESC, e.pgId ASC
		LIMIT $limit
		RETURN e.pgId AS pgId, e.name AS name, e.type AS type, degree`

	// expandQuery enters through the uniqueness-constrained pgId index, so a
	// supernode only costs its own adjacency, bounded further by LIMIT. No
	// variable-length paths: the UI expands exactly one hop at a time.
	expandQuery = `
		MATCH (e:Entity {pgId: $pgId})-[r]-(n:Entity)
		WHERE r.observedAt >= datetime($since) AND r.confidence >= $minConfidence
		  AND (size($relTypes) = 0 OR type(r) IN $relTypes)
		WITH n, type(r) AS relType,
		     CASE WHEN startNode(r) = e THEN 'out' ELSE 'in' END AS direction,
		     count(r) AS weight, max(r.observedAt) AS lastSeen
		ORDER BY weight DESC, lastSeen DESC, n.pgId ASC
		LIMIT $limit
		RETURN n.pgId AS pgId, n.name AS name, n.type AS type, relType, direction, weight, lastSeen`

	evidenceQuery = `
		MATCH (a:Entity {pgId: $from})-[r]-(b:Entity {pgId: $to})
		WHERE type(r) = $relType
		MATCH (d:Document {pgId: r.evidencePgId})
		RETURN d.pgId AS documentId, d.sourceType AS sourceType, d.occurredAt AS occurredAt,
		       r.confidence AS confidence, r.observedAt AS observedAt
		ORDER BY r.observedAt DESC
		LIMIT $limit`

	// searchQuery uses STARTS WITH, not CONTAINS: the range index on
	// normalizedName accelerates prefix matches only. Substring search would
	// need a text/full-text index (out of scope).
	searchQuery = `
		MATCH (e:Entity)
		WHERE e.normalizedName STARTS WITH $prefix
		RETURN e.pgId AS pgId, e.name AS name, e.type AS type
		ORDER BY e.normalizedName ASC
		LIMIT $limit`
)

// fixedQueries exposes the catalogue for the read-only guard test.
func fixedQueries() map[string]string {
	return map[string]string{
		"entry":    entryQuery,
		"expand":   expandQuery,
		"evidence": evidenceQuery,
		"search":   searchQuery,
	}
}

// --- input sanitising ---

// clampLimit applies the default for non-positive input and the ceiling for
// anything larger than max. Every read response is bounded this way.
func clampLimit(in, def, max int) int {
	if in <= 0 {
		return def
	}
	if in > max {
		return max
	}
	return in
}

// The four Clamp* wrappers exist so the HTTP layer clamps against exactly the
// same numbers this package enforces, instead of keeping a second copy of the
// constants that can drift. The Reader clamps again on its own input — the
// limit is enforced at the boundary that owns the query, not only at the edge.
func ClampEntryLimit(n int) int    { return clampLimit(n, entryLimitDefault, entryLimitMax) }
func ClampExpandLimit(n int) int   { return clampLimit(n, expandLimitDefault, expandLimitMax) }
func ClampEvidenceLimit(n int) int { return clampLimit(n, evidenceLimitDefault, evidenceLimitMax) }
func ClampSearchLimit(n int) int   { return clampLimit(n, searchLimitDefault, searchLimitMax) }

// sanitizeRelTypes keeps only the 8 whitelisted Cypher relationship literals.
func sanitizeRelTypes(in []string) []string {
	allowed := map[string]bool{}
	for _, t := range AllCypherRelTypes() {
		allowed[t] = true
	}
	out := make([]string, 0, len(in))
	for _, t := range in {
		if allowed[t] {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// sanitizeEntityTypes keeps only the 4 known entities.type values.
func sanitizeEntityTypes(in []string) []string {
	out := make([]string, 0, len(in))
	for _, t := range in {
		if _, ok := CypherEntityLabel(t); ok {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// toAnySlice converts to []any because the driver rejects []string for a
// parameter compared against a stored string property list in some versions;
// []any is always accepted.
func toAnySlice(in []string) []any {
	out := make([]any, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	return out
}

// sinceParam renders the lower time bound. A zero Since means "no bound",
// rendered as the Unix epoch rather than omitted, so the query text stays
// constant.
func sinceParam(t time.Time) string {
	if t.IsZero() {
		t = time.Unix(0, 0).UTC()
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// --- reads ---

// EntryPoints returns the highest-degree entities matching the filter.
func (r *Reader) EntryPoints(ctx context.Context, f EntryFilter) ([]EntryNode, error) {
	if r == nil || r.c == nil {
		return nil, errReaderNoClient
	}
	params := map[string]any{
		"since":         sinceParam(f.Since),
		"minConfidence": f.MinConfidence,
		"types":         toAnySlice(sanitizeEntityTypes(f.EntityTypes)),
		"limit":         clampLimit(f.Limit, entryLimitDefault, entryLimitMax),
	}

	out, err := r.c.Read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		res, err := tx.Run(ctx, entryQuery, params)
		if err != nil {
			return nil, err
		}
		var nodes []EntryNode
		for res.Next(ctx) {
			rec := res.Record()
			nodes = append(nodes, EntryNode{
				PgID:   recInt(rec, "pgId"),
				Name:   recString(rec, "name"),
				Type:   recString(rec, "type"),
				Degree: recInt(rec, "degree"),
			})
		}
		return nodes, res.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("graph: entry points: %w", err)
	}
	nodes, _ := out.([]EntryNode)
	return nodes, nil
}

// Expand returns one hop of neighbours around f.EntityPgID.
func (r *Reader) Expand(ctx context.Context, f ExpandFilter) ([]Neighbor, error) {
	if r == nil || r.c == nil {
		return nil, errReaderNoClient
	}
	params := map[string]any{
		"pgId":          f.EntityPgID,
		"since":         sinceParam(f.Since),
		"minConfidence": f.MinConfidence,
		"relTypes":      toAnySlice(sanitizeRelTypes(f.RelTypes)),
		"limit":         clampLimit(f.Limit, expandLimitDefault, expandLimitMax),
	}

	out, err := r.c.Read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		res, err := tx.Run(ctx, expandQuery, params)
		if err != nil {
			return nil, err
		}
		var neighbors []Neighbor
		for res.Next(ctx) {
			rec := res.Record()
			neighbors = append(neighbors, Neighbor{
				PgID:      recInt(rec, "pgId"),
				Name:      recString(rec, "name"),
				Type:      recString(rec, "type"),
				RelType:   recString(rec, "relType"),
				Direction: recString(rec, "direction"),
				Weight:    recInt(rec, "weight"),
				LastSeen:  recTime(rec, "lastSeen"),
			})
		}
		return neighbors, res.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("graph: expand: %w", err)
	}
	neighbors, _ := out.([]Neighbor)
	return neighbors, nil
}

// Evidence returns the documents backing one (from, to, relType) pair.
// relType must be one of the whitelisted literals; anything else is refused
// before a query is issued.
func (r *Reader) Evidence(ctx context.Context, fromPgID, toPgID int64, relType string, limit int) ([]EvidenceRef, error) {
	if len(sanitizeRelTypes([]string{relType})) != 1 {
		return nil, fmt.Errorf("graph: unknown relationship type")
	}
	if r == nil || r.c == nil {
		return nil, errReaderNoClient
	}
	params := map[string]any{
		"from":    fromPgID,
		"to":      toPgID,
		"relType": relType,
		"limit":   clampLimit(limit, evidenceLimitDefault, evidenceLimitMax),
	}

	out, err := r.c.Read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		res, err := tx.Run(ctx, evidenceQuery, params)
		if err != nil {
			return nil, err
		}
		var refs []EvidenceRef
		for res.Next(ctx) {
			rec := res.Record()
			ref := EvidenceRef{
				DocumentID: recString(rec, "documentId"),
				SourceType: recString(rec, "sourceType"),
				Confidence: recFloat(rec, "confidence"),
				ObservedAt: recTime(rec, "observedAt"),
			}
			if v, ok := rec.Get("occurredAt"); ok && v != nil {
				if t, ok := v.(time.Time); ok {
					tt := t
					ref.OccurredAt = &tt
				}
			}
			refs = append(refs, ref)
		}
		return refs, res.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("graph: evidence: %w", err)
	}
	refs, _ := out.([]EvidenceRef)
	return refs, nil
}

// SearchEntities does a prefix lookup over normalizedName. The prefix is
// normalised the same way the projection normalises names, so callers can pass
// what the user typed.
func (r *Reader) SearchEntities(ctx context.Context, prefix string, limit int) ([]EntityHit, error) {
	normalized := strings.ToLower(strings.TrimSpace(prefix))
	if normalized == "" {
		return nil, fmt.Errorf("graph: search prefix must not be empty")
	}
	if r == nil || r.c == nil {
		return nil, errReaderNoClient
	}
	params := map[string]any{
		"prefix": normalized,
		"limit":  clampLimit(limit, searchLimitDefault, searchLimitMax),
	}

	out, err := r.c.Read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		res, err := tx.Run(ctx, searchQuery, params)
		if err != nil {
			return nil, err
		}
		var hits []EntityHit
		for res.Next(ctx) {
			rec := res.Record()
			hits = append(hits, EntityHit{
				PgID: recInt(rec, "pgId"),
				Name: recString(rec, "name"),
				Type: recString(rec, "type"),
			})
		}
		return hits, res.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("graph: search entities: %w", err)
	}
	hits, _ := out.([]EntityHit)
	return hits, nil
}

// EntityTypeValues returns the 4 entity type values, used by callers that need
// to validate a filter before building an EntryFilter.
func EntityTypeValues() []string {
	out := []string{
		string(model.EntityTypePerson),
		string(model.EntityTypeOrg),
		string(model.EntityTypeConcept),
		string(model.EntityTypeOther),
	}
	sort.Strings(out)
	return out
}

// --- record helpers ---

func recInt(rec *neo4j.Record, key string) int64 {
	if v, ok := rec.Get(key); ok {
		if n, ok := v.(int64); ok {
			return n
		}
	}
	return 0
}

func recFloat(rec *neo4j.Record, key string) float64 {
	if v, ok := rec.Get(key); ok {
		switch n := v.(type) {
		case float64:
			return n
		case int64:
			return float64(n)
		}
	}
	return 0
}

func recString(rec *neo4j.Record, key string) string {
	if v, ok := rec.Get(key); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func recTime(rec *neo4j.Record, key string) time.Time {
	if v, ok := rec.Get(key); ok {
		if t, ok := v.(time.Time); ok {
			return t
		}
	}
	return time.Time{}
}
