package api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/baekenough/second-brain/internal/graph"
)

// Graph read API (Part B). Four GET routes over a fixed query catalogue.
//
// There is no write route and no endpoint that accepts Cypher. The projection
// worker is the only writer to Neo4j; this API can only read, and only through
// graph.Reader's constant queries.

// Default query parameters. Limits are clamped by the graph package so the
// numbers live in exactly one place.
const (
	graphDefaultDays          = 30
	graphDefaultMinConfidence = 0.5
	graphMaxDays              = 3650
)

// GraphReader is the read side of the projection. *graph.Reader satisfies it;
// declared here so handler tests can substitute a fake.
type GraphReader interface {
	EntryPoints(ctx context.Context, f graph.EntryFilter) ([]graph.EntryNode, error)
	Expand(ctx context.Context, f graph.ExpandFilter) ([]graph.Neighbor, error)
	Evidence(ctx context.Context, fromPgID, toPgID int64, relType string, limit int) ([]graph.EvidenceRef, error)
	SearchEntities(ctx context.Context, prefix string, limit int) ([]graph.EntityHit, error)
}

// WithGraph enables the /api/v1/graph/* routes. Must be called before
// Handler(); leaving it out is the rollback path — the routes then do not
// exist at all.
func (s *Server) WithGraph(g GraphReader) *Server {
	s.graph = g
	return s
}

// --- handlers ---

func (s *Server) graphEntryHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	days, err := parseGraphDays(q.Get("days"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	minConf, err := parseMinConfidence(q.Get("min_confidence"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	nodes, err := s.graph.EntryPoints(r.Context(), graph.EntryFilter{
		Since:         sinceDays(days),
		MinConfidence: minConf,
		EntityTypes:   splitCSV(q.Get("types")),
		Limit:         graph.ClampEntryLimit(parseIntDefault(q.Get("limit"), 0)),
	})
	if err != nil {
		graphUnavailable(w, "entry", err)
		return
	}
	if nodes == nil {
		nodes = []graph.EntryNode{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
}

func (s *Server) graphExpandHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	entityID, err := strconv.ParseInt(strings.TrimSpace(q.Get("entity_id")), 10, 64)
	if err != nil || entityID <= 0 {
		writeError(w, http.StatusBadRequest, "entity_id is required and must be a positive integer")
		return
	}
	days, err := parseGraphDays(q.Get("days"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	minConf, err := parseMinConfidence(q.Get("min_confidence"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	neighbors, err := s.graph.Expand(r.Context(), graph.ExpandFilter{
		EntityPgID:    entityID,
		Since:         sinceDays(days),
		MinConfidence: minConf,
		RelTypes:      splitCSV(q.Get("rel_types")),
		Limit:         graph.ClampExpandLimit(parseIntDefault(q.Get("limit"), 0)),
	})
	if err != nil {
		graphUnavailable(w, "expand", err)
		return
	}
	if neighbors == nil {
		neighbors = []graph.Neighbor{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"neighbors": neighbors})
}

func (s *Server) graphEvidenceHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, errFrom := strconv.ParseInt(strings.TrimSpace(q.Get("from")), 10, 64)
	to, errTo := strconv.ParseInt(strings.TrimSpace(q.Get("to")), 10, 64)
	relType := strings.TrimSpace(q.Get("rel_type"))
	if errFrom != nil || errTo != nil || from <= 0 || to <= 0 || relType == "" {
		writeError(w, http.StatusBadRequest, "from, to and rel_type are required")
		return
	}

	refs, err := s.graph.Evidence(r.Context(), from, to, relType,
		graph.ClampEvidenceLimit(parseIntDefault(q.Get("limit"), 0)))
	if err != nil {
		graphUnavailable(w, "evidence", err)
		return
	}
	if refs == nil {
		refs = []graph.EvidenceRef{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"evidence": refs})
}

func (s *Server) graphEntitiesHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix := strings.TrimSpace(q.Get("q"))
	if prefix == "" {
		writeError(w, http.StatusBadRequest, "q is required")
		return
	}

	hits, err := s.graph.SearchEntities(r.Context(), prefix,
		graph.ClampSearchLimit(parseIntDefault(q.Get("limit"), 0)))
	if err != nil {
		graphUnavailable(w, "entities", err)
		return
	}
	if hits == nil {
		hits = []graph.EntityHit{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entities": hits})
}

// --- helpers ---

// graphUnavailable maps a reader failure to 503, not 500: Neo4j is a derived
// store that can be down while everything else is healthy, and the frontend
// needs to tell those two cases apart.
//
// The log carries the endpoint name and the error only — never query
// parameters, which contain entity ids and search prefixes (privacy policy §4
// keeps names out of logs).
func graphUnavailable(w http.ResponseWriter, endpoint string, err error) {
	slog.Warn("graph read failed", "endpoint", endpoint, "error", err)
	writeError(w, http.StatusServiceUnavailable, "graph temporarily unavailable")
}

func parseIntDefault(raw string, def int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

func parseGraphDays(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return graphDefaultDays, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, errBadParam("days must be a positive integer")
	}
	if n > graphMaxDays {
		n = graphMaxDays
	}
	return n, nil
}

func parseMinConfidence(raw string) (float64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return graphDefaultMinConfidence, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 || v > 1 {
		return 0, errBadParam("min_confidence must be between 0 and 1")
	}
	return v, nil
}

// errBadParam is a tiny helper so the handlers can return the message
// verbatim without importing errors in four places.
type badParamError string

func (e badParamError) Error() string { return string(e) }

func errBadParam(msg string) error { return badParamError(msg) }

func splitCSV(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func sinceDays(days int) time.Time {
	return time.Now().AddDate(0, 0, -days)
}
