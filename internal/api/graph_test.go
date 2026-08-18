package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/graph"
)

// --- fake reader ---

type fakeGraphReader struct {
	lastEntryFilter  graph.EntryFilter
	lastExpandFilter graph.ExpandFilter
	lastEvidence     struct {
		from, to int64
		relType  string
		limit    int
	}
	lastSearchPrefix string
	lastSearchLimit  int

	calls int
	err   error
}

func (f *fakeGraphReader) EntryPoints(_ context.Context, filter graph.EntryFilter) ([]graph.EntryNode, error) {
	f.calls++
	f.lastEntryFilter = filter
	if f.err != nil {
		return nil, f.err
	}
	return []graph.EntryNode{{PgID: 11, Name: "더미갑", Type: "PERSON", Degree: 3}}, nil
}

func (f *fakeGraphReader) Expand(_ context.Context, filter graph.ExpandFilter) ([]graph.Neighbor, error) {
	f.calls++
	f.lastExpandFilter = filter
	if f.err != nil {
		return nil, f.err
	}
	return []graph.Neighbor{{PgID: 22, Name: "더미을", Type: "PERSON", RelType: "COMMUNICATED_WITH", Direction: "out", Weight: 2}}, nil
}

func (f *fakeGraphReader) Evidence(_ context.Context, from, to int64, relType string, limit int) ([]graph.EvidenceRef, error) {
	f.calls++
	f.lastEvidence.from, f.lastEvidence.to = from, to
	f.lastEvidence.relType, f.lastEvidence.limit = relType, limit
	if f.err != nil {
		return nil, f.err
	}
	return []graph.EvidenceRef{{DocumentID: "00000000-0000-0000-0000-0000000000a1", SourceType: "sms"}}, nil
}

func (f *fakeGraphReader) SearchEntities(_ context.Context, prefix string, limit int) ([]graph.EntityHit, error) {
	f.calls++
	f.lastSearchPrefix, f.lastSearchLimit = prefix, limit
	if f.err != nil {
		return nil, f.err
	}
	return []graph.EntityHit{{PgID: 11, Name: "더미갑", Type: "PERSON"}}, nil
}

// --- helpers ---

// newGraphTestServer builds a Server with Bearer auth enabled, mirroring the
// production wiring (graph routes live inside the authenticated group).
func newGraphTestServer() *Server {
	return NewServer(&stubDocumentStore{}, nil, nil, nil, nil, "", "test-key")
}

func doGraphGet(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

// --- tests ---

func TestGraphEntryHandler_ClampsLimit(t *testing.T) {
	fake := &fakeGraphReader{}
	srv := newGraphTestServer().WithGraph(fake)
	rr := doGraphGet(t, srv, "/api/v1/graph/entry?limit=9999")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if fake.lastEntryFilter.Limit != 50 {
		t.Fatalf("Limit = %d, want clamped to 50", fake.lastEntryFilter.Limit)
	}
}

func TestGraphEntryHandler_Defaults(t *testing.T) {
	fake := &fakeGraphReader{}
	srv := newGraphTestServer().WithGraph(fake)
	if rr := doGraphGet(t, srv, "/api/v1/graph/entry"); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	f := fake.lastEntryFilter
	if f.MinConfidence != 0.5 {
		t.Errorf("MinConfidence = %v, want 0.5", f.MinConfidence)
	}
	if f.Limit != 20 {
		t.Errorf("Limit = %d, want 20", f.Limit)
	}
	// days defaults to 30 → Since roughly 30 days ago.
	age := time.Since(f.Since)
	if age < 29*24*time.Hour || age > 31*24*time.Hour {
		t.Errorf("Since = %v (age %v), want ~30 days ago", f.Since, age)
	}
}

func TestGraphExpandHandler_RequiresEntityID(t *testing.T) {
	fake := &fakeGraphReader{}
	srv := newGraphTestServer().WithGraph(fake)
	rr := doGraphGet(t, srv, "/api/v1/graph/expand")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if fake.calls != 0 {
		t.Fatalf("reader called %d times, want 0", fake.calls)
	}
}

func TestGraphExpandHandler_PassesRelTypes(t *testing.T) {
	fake := &fakeGraphReader{}
	srv := newGraphTestServer().WithGraph(fake)
	rr := doGraphGet(t, srv, "/api/v1/graph/expand?entity_id=11&rel_types=COMMUNICATED_WITH,ABOUT_TOPIC&limit=1000")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := fake.lastExpandFilter.RelTypes; len(got) != 2 {
		t.Fatalf("RelTypes = %v, want 2 entries", got)
	}
	if fake.lastExpandFilter.Limit != 200 {
		t.Fatalf("Limit = %d, want clamped to 200", fake.lastExpandFilter.Limit)
	}
}

func TestGraphEvidenceHandler_RequiresAllParams(t *testing.T) {
	for _, path := range []string{
		"/api/v1/graph/evidence?to=22&rel_type=MENTIONS",
		"/api/v1/graph/evidence?from=11&rel_type=MENTIONS",
		"/api/v1/graph/evidence?from=11&to=22",
	} {
		fake := &fakeGraphReader{}
		srv := newGraphTestServer().WithGraph(fake)
		if rr := doGraphGet(t, srv, path); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", path, rr.Code)
		}
		if fake.calls != 0 {
			t.Errorf("%s: reader called %d times, want 0", path, fake.calls)
		}
	}
}

func TestGraphEntitiesHandler_RequiresQuery(t *testing.T) {
	fake := &fakeGraphReader{}
	srv := newGraphTestServer().WithGraph(fake)
	if rr := doGraphGet(t, srv, "/api/v1/graph/entities?q="); rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if fake.calls != 0 {
		t.Fatalf("reader called %d times, want 0", fake.calls)
	}
}

// TestGraphHandler_ReaderErrorIs503: Neo4j is a derived store that may be
// down while the rest of the API is healthy. 503 (not 500) is what lets the
// frontend say "graph temporarily unavailable" instead of "something broke".
func TestGraphHandler_ReaderErrorIs503(t *testing.T) {
	fake := &fakeGraphReader{err: errors.New("neo4j down")}
	srv := newGraphTestServer().WithGraph(fake)
	if rr := doGraphGet(t, srv, "/api/v1/graph/entry"); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

// TestGraphRoutes_NotRegisteredWithoutWithGraph pins the rollback story: with
// the graph unwired the feature simply does not exist in the router.
func TestGraphRoutes_NotRegisteredWithoutWithGraph(t *testing.T) {
	srv := newGraphTestServer() // WithGraph not called
	for _, path := range []string{
		"/api/v1/graph/entry", "/api/v1/graph/expand",
		"/api/v1/graph/evidence", "/api/v1/graph/entities",
	} {
		if rr := doGraphGet(t, srv, path); rr.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404 when graph is not wired", path, rr.Code)
		}
	}
}

// TestGraphRoutes_WriteMethodsNotRegistered: the API exposes GET only, which
// is the simplest possible proof that it cannot mutate the projection.
func TestGraphRoutes_WriteMethodsNotRegistered(t *testing.T) {
	srv := newGraphTestServer().WithGraph(&fakeGraphReader{})
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/v1/graph/entry", nil)
		req.Header.Set("Authorization", "Bearer test-key")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code == http.StatusOK {
			t.Errorf("%s /api/v1/graph/entry returned 200, want a rejection", method)
		}
	}
}

func TestGraphEntryHandler_ResponseShape(t *testing.T) {
	srv := newGraphTestServer().WithGraph(&fakeGraphReader{})
	rr := doGraphGet(t, srv, "/api/v1/graph/entry")
	var body struct {
		Nodes []graph.EntryNode `json:"nodes"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v (%s)", err, rr.Body.String())
	}
	if len(body.Nodes) != 1 || body.Nodes[0].PgID != 11 {
		t.Fatalf("nodes = %+v, want one node with entity_id 11", body.Nodes)
	}
}
