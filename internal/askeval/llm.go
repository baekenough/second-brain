package askeval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/baekenough/second-brain/internal/llm"
)

// promptKind classifies which /ask pipeline stage issued one
// CompleteWithMessages call, detected from the system prompt's opening
// text. This package cannot import the real prompt constants: they are
// unexported (internal/api/ask_rewrite.go's askRewriteSystemPrompt,
// internal/api/ask.go's askSystemPromptTemplate,
// internal/intent/intent.go's classifySystemPrompt,
// internal/intent/plan.go's planSystemPrompt) — by design, per this
// package's own read of R006 separation of concerns, an eval harness has
// no business reaching into another package's private prompt text. If one
// of those four prompts' opening sentence is ever reworded, the matching
// case below silently stops matching that call and CompleteWithMessages
// falls through to the promptUnknown error path — runner_test.go's fixture
// run would then fail loudly (a missing rewrite/synthesis response), not
// misroute a scripted answer to the wrong stage.
type promptKind int

const (
	promptUnknown promptKind = iota
	promptRewrite
	promptClassify
	promptPlan
	promptSynthesis
)

var promptPrefixes = map[promptKind]string{
	promptRewrite:   "당신은 대화형 검색 시스템의 질의 재작성기입니다",
	promptClassify:  "You classify a user's Korean question",
	promptPlan:      "You plan document retrieval for a Korean personal-knowledge assistant",
	promptSynthesis: "당신은 사용자의 개인 지식 베이스에서 검색된 문서를 근거로 질문에 답하는 어시스턴트입니다",
}

func classifyPrompt(system string) promptKind {
	for kind, prefix := range promptPrefixes {
		if strings.HasPrefix(system, prefix) {
			return kind
		}
	}
	return promptUnknown
}

// call is one recorded CompleteWithMessages invocation.
type call struct {
	Kind     promptKind
	System   string
	Messages []llm.Message
}

const abstentionAnswer = "제공된 정보로는 답변할 수 없습니다."

// scriptedCompleter is issue #266's offline stand-in for llm.Completer. It
// never makes a network call, and it deliberately does NOT implement
// llm.StreamCompleter — ask.go's synthesize falls back to its
// CompleteWithMessages branch for any Completer that isn't also a
// StreamCompleter, which is exactly the single-shot "one token event"
// shape this runner needs (see runner.go's SSE parsing).
//
// It answers the classifier (Stage 1) and planner (Stage 1b) calls with an
// error on purpose: both intent.LLMClassifier and intent.LLMPlanner treat
// an LLM failure as "fall back to the deterministic/unconstrained path",
// never as a request failure (their own doc comments) — so every fixture's
// retrieval SHAPE ends up decided entirely by intent.DeterministicWindow's
// regex parser reading the fixture's own question text, which is the only
// thing a synthetic fixture can meaningfully script anyway (deep-plan #266
// plan §4: "classifier/planner -> error").
//
// One scriptedCompleter instance is reused across a Run's fixtures
// (forFixture rebinds it) rather than allocated fresh per fixture — see
// runner.go's Run for why fixtures execute strictly sequentially, which is
// what makes that reuse safe despite the shared mutable fixture/calls state
// below.
type scriptedCompleter struct {
	mu      sync.Mutex
	fixture *Fixture
	resolve func(alias string) (string, bool)
	calls   []call
}

func newScriptedCompleter() *scriptedCompleter { return &scriptedCompleter{} }

// forFixture points the completer at f for the next request(s) the runner
// sends, and clears any calls recorded for a previous fixture.
func (c *scriptedCompleter) forFixture(f *Fixture, resolve func(string) (string, bool)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fixture = f
	c.resolve = resolve
	c.calls = nil
}

func (c *scriptedCompleter) Enabled() bool { return true }

func (c *scriptedCompleter) CompleteWithMessages(_ context.Context, system string, messages []llm.Message) (string, error) {
	c.mu.Lock()
	fixture, resolve := c.fixture, c.resolve
	kind := classifyPrompt(system)
	c.calls = append(c.calls, call{Kind: kind, System: system, Messages: messages})
	c.mu.Unlock()

	if fixture == nil {
		return "", errors.New("askeval: scriptedCompleter invoked with no fixture bound (forFixture was never called)")
	}
	switch kind {
	case promptRewrite:
		if fixture.StandaloneQuestion == "" {
			return "", fmt.Errorf("askeval: fixture %s has conversation history but no standalone_question", fixture.ID)
		}
		return fixture.StandaloneQuestion, nil
	case promptClassify, promptPlan:
		return "", errors.New("askeval: scripted LLM intentionally declines classify/plan calls (falls back to the deterministic path)")
	case promptSynthesis:
		return c.synthesize(fixture, resolve, messages), nil
	default:
		return "", fmt.Errorf("askeval: unrecognized system prompt (first 48 bytes: %q)", firstBytes(system, 48))
	}
}

// synthesisPrompt returns the exact text of the MOST RECENT synthesis-stage
// call this completer has recorded — the runner's window into what
// buildBudgetedAskMessages (ask_context.go) actually placed in front of the
// model for the current fixture, which is what metrics.go's ContextHit
// detector inspects (never the fixture's own Gold data, and never the
// retrieval "sources" event, which can list a document the excerpt budget
// later dropped — deep-plan #268 finding F2).
func (c *scriptedCompleter) synthesisPrompt() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.calls) - 1; i >= 0; i-- {
		if c.calls[i].Kind == promptSynthesis {
			return joinMessageContent(c.calls[i].Messages), true
		}
	}
	return "", false
}

// synthesize is the Stage 3 answer generator.
//
// A fixture with ScriptedAnswer set gets that text back verbatim, after
// "{{doc:<alias>}}"/"{{fake}}" substitution — the adversarial-citation
// fixtures need a SPECIFIC malformed/fabricated/unknown citation shape the
// default oracle below cannot produce on its own.
//
// Every other fixture gets the "context-conditional oracle" (issue #266
// completion criteria explicitly requires reporting this as exactly that, a
// deterministic detector — never a real language model): for each
// Gold.Claims[i]/Gold.SupportSpans[i] pair, if SupportSpans[i] is a
// substring of the text the real pipeline ACTUALLY placed in front of this
// call, the oracle emits Claims[i] with a citation to SupportDocs[i]'s real
// document ID; if the span is missing, that specific claim is skipped
// rather than fabricated. A fixture with zero emittable claims (including
// every unanswerable fixture, which declares no spans at all) gets the
// abstention phrase — never an empty string, which askCitationStatus would
// misread as "missing" rather than "declined".
func (c *scriptedCompleter) synthesize(f *Fixture, resolve func(string) (string, bool), messages []llm.Message) string {
	if f.ScriptedAnswer != "" {
		return substituteAliases(f.ScriptedAnswer, resolve)
	}
	prompt := joinMessageContent(messages)
	var claims []string
	for i, span := range f.Gold.SupportSpans {
		if span == "" || !strings.Contains(prompt, span) {
			continue
		}
		if i >= len(f.Gold.SupportDocs) || i >= len(f.Gold.Claims) {
			continue
		}
		id, ok := resolve(f.Gold.SupportDocs[i])
		if !ok {
			continue
		}
		claims = append(claims, fmt.Sprintf("%s [근거](/documents/%s)", f.Gold.Claims[i], id))
	}
	if len(claims) == 0 {
		return abstentionAnswer
	}
	return strings.Join(claims, " ")
}

func firstBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func joinMessageContent(messages []llm.Message) string {
	var b strings.Builder
	for _, m := range messages {
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}
	return b.String()
}

// substituteAliases replaces every "{{doc:<alias>}}" with that alias's real
// (deterministic) document UUID string — falling back to a fixed,
// guaranteed-unresolvable UUID if the fixture author typos an alias, so a
// broken fixture fails its OWN citation-status assertion loudly instead of
// silently referencing a real document by accident — and every "{{fake}}"
// with a fixed UUID that aliasID (corpus.go) can never itself produce.
func substituteAliases(answer string, resolve func(string) (string, bool)) string {
	const openTag = "{{doc:"
	out := answer
	for {
		start := strings.Index(out, openTag)
		if start < 0 {
			break
		}
		rel := strings.Index(out[start:], "}}")
		if rel < 0 {
			break
		}
		end := start + rel
		alias := out[start+len(openTag) : end]
		id, ok := resolve(alias)
		if !ok {
			id = "99999999-9999-9999-9999-999999999999"
		}
		out = out[:start] + id + out[end+2:]
	}
	return strings.ReplaceAll(out, "{{fake}}", "ffffffff-ffff-ffff-ffff-ffffffffffff")
}
