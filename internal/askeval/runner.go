package askeval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/baekenough/second-brain/internal/api"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// askEvalAPIKey is the fixed Bearer token every request this runner sends
// uses. It never leaves the process (api.NewServer's apiKey param is
// compared, never transmitted anywhere) and has no relationship to any
// real deployment's API_KEY.
const askEvalAPIKey = "askeval-offline-key"

// --- wire-shape mirrors ---
//
// These structs duplicate the JSON field names of internal/api's unexported
// SSE payload types (askSourcesPayload, askPlanPayload, askDonePayload,
// askVerificationPayload — ask.go). They are NOT imported: those types are
// unexported on purpose (R006 separation of concerns — an eval harness
// consumes the WIRE contract, the same one web/src/lib/sseEvents.ts
// consumes, not the server's internal representation of it).
// TestAskHandler_SSEFieldNames (internal/api/ask_test.go) pins those field
// names against the same frontend contract this file also depends on, so a
// silent rename on either side fails a Go test before it fails a JSON
// unmarshal here.

type sourceItem struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	SourceType string     `json:"source_type"`
	Score      float64    `json:"score"`
	OccurredAt *time.Time `json:"occurred_at"`
}

type planPayload struct {
	Origin       string   `json:"origin"`
	Reason       string   `json:"reason"`
	OccurredFrom string   `json:"occurred_from"`
	OccurredTo   string   `json:"occurred_to"`
	SourceTypes  []string `json:"source_types"`
	Limit        int      `json:"limit"`
}

type sourcesPayload struct {
	Sources []sourceItem `json:"sources"`
	Plan    *planPayload `json:"plan,omitempty"`
}

type verificationPayload struct {
	CitationStatus    string   `json:"citation_status"`
	CitedIDs          []string `json:"cited_ids"`
	UnknownIDs        []string `json:"unknown_ids"`
	MalformedLinks    int      `json:"malformed_links"`
	InferredCitedIDs  []string `json:"inferred_cited_ids"`
	PromptEvidenceIDs []string `json:"prompt_evidence_ids"`
	ClaimSupport      string   `json:"claim_support"`
}

type donePayload struct {
	FinishReason string               `json:"finish_reason"`
	Verification *verificationPayload `json:"verification,omitempty"`
}

type tokenPayload struct {
	Text string `json:"text"`
}

// sseEvent is one "event: <name>\ndata: <json>\n\n" frame.
type sseEvent struct {
	Name string
	Data string
}

// parseSSE is a close, package-local port of internal/api/ask_test.go's
// unexported parseSSEFrames (that helper lives in a _test.go file and
// cannot be imported across packages).
func parseSSE(body string) []sseEvent {
	var out []sseEvent
	var name string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			out = append(out, sseEvent{Name: name, Data: strings.TrimPrefix(line, "data: ")})
		}
	}
	return out
}

// memSessions is an in-memory api.AskConversationStore fake, used to seed a
// fixture's History as real prior turns (rather than replaying them through
// extra scripted LLM calls) before the runner fires the fixture's actual
// request — resolveConversation (ask_history.go) then loads them exactly as
// it would load real ask_sessions rows.
type memSessions struct {
	mu     sync.Mutex
	byConv map[uuid.UUID][]store.AskSession
}

func newMemSessions() *memSessions {
	return &memSessions{byConv: map[uuid.UUID][]store.AskSession{}}
}

func (m *memSessions) Insert(_ context.Context, s store.AskSession) (store.AskSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	s.CreatedAt = time.Now()
	m.byConv[s.ConversationID] = append(m.byConv[s.ConversationID], s)
	return s, nil
}

func (m *memSessions) ListConversationTurns(_ context.Context, conversationID uuid.UUID) ([]store.AskSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]store.AskSession{}, m.byConv[conversationID]...), nil
}

func (m *memSessions) ListRecentConversations(context.Context, int) ([]store.AskSession, error) {
	return nil, nil // unused by the runner; only per-conversation replay matters here.
}

// CaseResult is one fixture's raw run outcome plus its scored CaseMetrics.
type CaseResult struct {
	Fixture      Fixture
	Metrics      CaseMetrics
	RawAnswer    string
	Sources      []sourceItem
	Plan         *planPayload
	Verification *verificationPayload
	FinishReason string
	LatencyMS    map[string]float64
	// Err is set when the fixture itself could not be executed at all (a
	// harness/fixture-authoring bug: bad as_of, unresolvable alias,
	// scripted-LLM contract violation) — distinct from a normal metric
	// failure, which always has Err==nil and a Metrics.Pass==false
	// instead. report.go surfaces Err separately so a broken fixture is
	// never silently counted as "the pipeline produced a wrong answer".
	Err error
}

// RunOptions configures Run. Zero value is DefaultRunOptions' shape.
type RunOptions struct {
	// AskTopK/AskInsightM feed api.Server.WithAskConfig. Defaulted (see
	// DefaultRunOptions) to comfortably exceed every fixture's corpus size
	// so a fixture author does not have to reason about the production
	// default (8) truncating an intentionally larger synthetic corpus.
	AskTopK, AskInsightM int
	RerankDefault        bool
}

// DefaultRunOptions returns the RunOptions every eval/ask/fixtures case in
// this repository is authored against.
func DefaultRunOptions() RunOptions {
	return RunOptions{AskTopK: 12, AskInsightM: 3}
}

// Run executes every fixture against the REAL /ask handler (api.Server,
// via httptest) and returns one CaseResult per fixture, in input order.
//
// Fixtures run strictly SEQUENTIALLY, one *api.Server per fixture: each
// fixture declares its own isolated corpus (Fixture.Corpus), and the
// scripted LLM (llm.go) is stateful — forFixture rebinds its currently-
// scripted fixture and resets its call log — so two fixtures in flight at
// once would race on which one is being scripted at any given moment. This
// keeps the runner simple and fully deterministic at the cost of wall-clock
// time; parallelizing across fixtures (each with its own scriptedCompleter
// instance) is possible future work, not required by issue #266's
// completion criteria.
func Run(ctx context.Context, fixtures []Fixture, opts RunOptions) []CaseResult {
	results := make([]CaseResult, 0, len(fixtures))
	for _, f := range fixtures {
		results = append(results, runOne(ctx, f, opts))
	}
	return results
}

func runOne(ctx context.Context, f Fixture, opts RunOptions) CaseResult {
	res := CaseResult{Fixture: f, LatencyMS: map[string]float64{}}

	asOf, err := time.Parse(time.RFC3339, f.AsOf)
	if err != nil {
		res.Err = fmt.Errorf("askeval: fixture %s: as_of: %w", f.ID, err)
		return res
	}

	cp, err := buildCorpus(f, asOf)
	if err != nil {
		res.Err = fmt.Errorf("askeval: fixture %s: %w", f.ID, err)
		return res
	}

	completer := newScriptedCompleter()
	completer.forFixture(&f, cp.resolveAlias)

	svc := search.NewService(cp, hashedEmbedder{}).WithChunkStore(cp)

	sessions := newMemSessions()
	convID := uuid.New()
	for i, turn := range f.History {
		if _, err := sessions.Insert(ctx, store.AskSession{
			ConversationID: convID,
			TurnIndex:      i,
			Question:       turn.Question,
			Answer:         turn.Answer,
			FinishReason:   "stop",
		}); err != nil {
			res.Err = fmt.Errorf("askeval: fixture %s: seed history turn %d: %w", f.ID, i, err)
			return res
		}
	}

	srv := api.NewServer(nil, svc, nil, nil, completer, "", askEvalAPIKey).
		WithAskConfig(0, opts.AskTopK, opts.AskInsightM).
		WithAskRerankDefault(opts.RerankDefault).
		WithAskSessions(sessions).
		WithClock(func() time.Time { return asOf })

	body, err := json.Marshal(map[string]string{
		"question":        f.Question,
		"conversation_id": convID.String(),
	})
	if err != nil {
		res.Err = fmt.Errorf("askeval: fixture %s: encode request: %w", f.ID, err)
		return res
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ask", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+askEvalAPIKey)
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()

	start := time.Now()
	srv.Handler().ServeHTTP(rr, req)

	var gotFirstToken bool
	for _, fr := range parseSSE(rr.Body.String()) {
		switch fr.Name {
		case "sources":
			res.LatencyMS["sources_ms"] = msSince(start)
			var payload sourcesPayload
			if err := json.Unmarshal([]byte(fr.Data), &payload); err == nil {
				res.Sources = payload.Sources
				res.Plan = payload.Plan
			}
		case "token":
			if !gotFirstToken {
				gotFirstToken = true
				res.LatencyMS["first_token_ms"] = msSince(start)
			}
			var tok tokenPayload
			if err := json.Unmarshal([]byte(fr.Data), &tok); err == nil {
				res.RawAnswer += tok.Text
			}
		case "done":
			res.LatencyMS["done_ms"] = msSince(start)
			var payload donePayload
			if err := json.Unmarshal([]byte(fr.Data), &payload); err == nil {
				res.FinishReason = payload.FinishReason
				res.Verification = payload.Verification
			}
		}
	}

	if res.FinishReason == "" {
		res.Err = fmt.Errorf("askeval: fixture %s: handler produced no \"done\" event (HTTP status %d)", f.ID, rr.Code)
		return res
	}

	prompt, _ := completer.synthesisPrompt()
	res.Metrics = computeMetrics(f, cp, res, prompt)
	return res
}

func msSince(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000.0
}
