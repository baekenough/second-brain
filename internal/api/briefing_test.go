package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/briefing"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

const briefingTestDocID = "11111111-1111-1111-1111-111111111111"

// countingCompleter records how many times the model was actually called, which
// is how the cache tests tell a hit from a miss.
type countingCompleter struct {
	calls int
	reply string
}

func (c *countingCompleter) Enabled() bool { return true }

func (c *countingCompleter) CompleteWithMessages(_ context.Context, _ string, _ []llm.Message) (string, error) {
	c.calls++
	return c.reply, nil
}

func briefingTestItem() store.ActionListItem {
	return store.ActionListItem{
		Action: model.Action{
			IdentityKey: "action:aaaaaaaaaaaaaaaa",
			DocumentID:  uuid.MustParse(briefingTestDocID),
			ThreadKey:   "zz-dummy-thread",
			Kind:        model.KindMyCommitment,
			Summary:     "더미 요약",
			DetectedBy:  model.DetectedStructural,
			Confidence:  1.0,
			ObservedAt:  time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		},
		State: model.StateOpen,
	}
}

func newBriefingTestServer(lister ActionLister, c llm.Completer) *Server {
	return NewServer(nil, nil, nil, nil, c, "", "").
		WithActions(lister, nil).
		WithBriefing(briefing.NewCache(4), 40)
}

// TestBriefingHandlerSkipsLLMWhenNoOpenActions pins that an empty open set is
// answered without calling the model. A summary request with nothing to
// summarise produces invention, and it also costs money for no reason.
func TestBriefingHandlerSkipsLLMWhenNoOpenActions(t *testing.T) {
	llmStub := &countingCompleter{reply: `{"sentences":[]}`}
	srv := newBriefingTestServer(&stubActionLister{}, llmStub)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/briefing", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if llmStub.calls != 0 {
		t.Fatalf("LLM called %d times for an empty action set, want 0", llmStub.calls)
	}

	var body struct {
		Sentences   []briefing.Sentence `json:"sentences"`
		Degraded    bool                `json:"degraded"`
		ActionCount int                 `json:"action_count"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Sentences) != 0 || body.Degraded || body.ActionCount != 0 {
		t.Fatalf("body = %+v, want an empty, non-degraded briefing", body)
	}
}

// TestBriefingHandlerCachesOnIdenticalInput pins the cost control: the same open
// action set must not be summarised twice.
func TestBriefingHandlerCachesOnIdenticalInput(t *testing.T) {
	llmStub := &countingCompleter{
		reply: `{"sentences":[{"text":"근거 있는 문장.","document_ids":["` + briefingTestDocID + `"],"identity_keys":["action:aaaaaaaaaaaaaaaa"]}]}`,
	}
	lister := &stubActionLister{items: []store.ActionListItem{briefingTestItem()}}
	srv := newBriefingTestServer(lister, llmStub)

	var bodies []string
	var cachedFlags []bool
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/briefing", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status=%d body=%s", i, rec.Code, rec.Body.String())
		}
		var body struct {
			Sentences []briefing.Sentence `json:"sentences"`
			Cached    bool                `json:"cached"`
		}
		if err := json.NewDecoder(strings.NewReader(rec.Body.String())).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		bodies = append(bodies, rec.Body.String())
		cachedFlags = append(cachedFlags, body.Cached)
		if len(body.Sentences) != 1 {
			t.Fatalf("request %d returned %d sentences, want 1", i, len(body.Sentences))
		}
	}

	if llmStub.calls != 1 {
		t.Fatalf("LLM called %d times, want 1 (second call must hit the cache)", llmStub.calls)
	}
	if cachedFlags[0] || !cachedFlags[1] {
		t.Fatalf("cached flags = %v, want [false true]", cachedFlags)
	}
	if lister.calls != 2 {
		t.Fatalf("store queried %d times, want 2 (the cache key depends on the current open set, so it must be re-read)", lister.calls)
	}
}

// TestBriefingHandlerReportsDroppedSentences pins that the discard count reaches
// the client. It is a trust signal, not debug output: the user should be able to
// see that the server is checking the model's claims.
func TestBriefingHandlerReportsDroppedSentences(t *testing.T) {
	llmStub := &countingCompleter{
		reply: `{"sentences":[
			{"text":"근거 있는 문장.","document_ids":["` + briefingTestDocID + `"]},
			{"text":"환각 문장.","document_ids":["99999999-9999-9999-9999-999999999999"]}
		]}`,
	}
	srv := newBriefingTestServer(&stubActionLister{items: []store.ActionListItem{briefingTestItem()}}, llmStub)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/briefing", nil))

	var body struct {
		Sentences    []briefing.Sentence `json:"sentences"`
		DroppedCount int                 `json:"dropped_count"`
		ActionCount  int                 `json:"action_count"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Sentences) != 1 {
		t.Fatalf("sentences = %+v, want only the grounded one", body.Sentences)
	}
	if body.DroppedCount != 1 {
		t.Fatalf("dropped_count = %d, want 1", body.DroppedCount)
	}
	if body.ActionCount != 1 {
		t.Fatalf("action_count = %d, want 1", body.ActionCount)
	}
}

// TestBriefingHandlerNotRegisteredWithoutDependencies pins the rollback
// contract: the route must be absent unless both the LLM client and the action
// lister are wired.
func TestBriefingHandlerNotRegisteredWithoutDependencies(t *testing.T) {
	cases := map[string]*Server{
		"no briefing cache": NewServer(nil, nil, nil, nil, &countingCompleter{}, "", "").
			WithActions(&stubActionLister{}, nil),
		"no action lister": NewServer(nil, nil, nil, nil, &countingCompleter{}, "", "").
			WithBriefing(briefing.NewCache(4), 40),
		"no llm client": NewServer(nil, nil, nil, nil, nil, "", "").
			WithActions(&stubActionLister{}, nil).
			WithBriefing(briefing.NewCache(4), 40),
	}
	for name, srv := range cases {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/briefing", nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status=%d, want 404", rec.Code)
			}
		})
	}
}

// TestBriefingHandlerStoreFailure pins that a failed action query is a 500 with
// a fixed message — not a body carrying database text.
func TestBriefingHandlerStoreFailure(t *testing.T) {
	lister := &stubActionLister{err: context.DeadlineExceeded}
	srv := newBriefingTestServer(lister, &countingCompleter{})

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/briefing", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "deadline") {
		t.Fatalf("error body leaked the underlying error: %s", rec.Body.String())
	}
}
