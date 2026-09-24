// Package askeval implements issue #266's offline end-to-end evaluation
// runner for POST /api/v1/ask: it drives the REAL handler (internal/api,
// via httptest — see runner.go) against a synthetic, isolated corpus (see
// corpus.go) and a scripted LLM (see llm.go), so retrieval, query
// rewriting, the query planner, excerpt budgeting, synthesis, and the
// deterministic citation validator (issue #268) all run exactly as they do
// in production — never a mock of /ask's own logic.
//
// This package intentionally contains NO real personal data: every fixture
// in eval/ask/fixtures/ is synthetic, and the corpus it builds exists only
// for the duration of one Run call (see runner.go's per-fixture *corpus).
package askeval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Turn is one prior question/answer exchange replayed as a fixture's
// conversation history — the shape ask_history.go's askHistoryTurn takes,
// minus that type's package-private-ness (this package cannot import an
// unexported api.askHistoryTurn).
type Turn struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

// Gold is a fixture's ground truth: what the real /ask pipeline SHOULD
// produce for Question, given History and the fixture's own Corpus.
type Gold struct {
	// Answerable is false for the "no-evidence"/"unanswerable" category:
	// the expected outcome is abstention (finish_reason "no_evidence" or a
	// citation_status "abstained" answer), not any claim. Ignored when
	// ExpectedCitationStatus is set (the adversarial-citation category has
	// its own pass rule — see computeMetrics).
	Answerable bool `json:"answerable"`
	// Claims are short, literal Korean substrings the answer text must
	// contain for an answerable fixture to pass (checked verbatim — issue
	// #266 scope explicitly excludes a runtime LLM judge; see llm.go's
	// scriptedCompleter for how the default oracle produces exactly these
	// strings when the evidence was actually shown to it).
	Claims []string `json:"claims,omitempty"`
	// SupportDocs names Corpus[i].Alias values: the documents that back
	// every Claims entry, used for the RetrievalHit detector (metrics.go).
	SupportDocs []string `json:"support_doc_ids,omitempty"`
	// SupportSpans are literal substrings of a SupportDocs entry's Content
	// that must appear in the EXACT text the real pipeline placed in the
	// Stage 3 synthesis prompt for the corresponding claim to be
	// checkable at all — retrieval finding the document is necessary but
	// not sufficient (deep-plan #268 finding F2; metrics.go's ContextHit).
	SupportSpans []string `json:"support_spans,omitempty"`
	// ExpectedCitationStatus, when set, switches computeMetrics into
	// adversarial-detector mode: Pass becomes "did the real, unmodified
	// askCitationStatus verdict (issue #268) equal this value", not
	// "did the answer contain every Claims string". Used by the
	// adversarial-citation fixtures (fake UUID, malformed link,
	// prior-turn-only ID), whose ScriptedAnswer intentionally cites a bad
	// ID — the CORRECT, passing outcome for those fixtures is that the
	// real validator catches it, not that the answer "succeeds".
	ExpectedCitationStatus string `json:"expected_citation_status,omitempty"`
	// ExpectInferredCitation additionally requires the done event's
	// verification.inferred_cited_ids to be non-empty — the
	// "추론을 사실로 인용" regression signal (ask_evidence.go's
	// askEvidenceLayer doc comment) the inferred-as-fact fixture exists to
	// exercise. Only meaningful alongside ExpectedCitationStatus=="valid"
	// (citing an inferred-layer document is ALLOWED, just flagged).
	ExpectInferredCitation bool `json:"expect_inferred_citation,omitempty"`
}

// CorpusDoc is one document a fixture's isolated corpus contains.
type CorpusDoc struct {
	// Alias is this fixture's own stable name for the document — NEVER a
	// UUID. aliasID (corpus.go) derives a deterministic uuid.UUID from it,
	// so the same alias always maps to the same document ID on every run
	// (required for Gold.SupportDocs references and report.go's baseline
	// diff to stay meaningful across runs).
	Alias string `json:"alias"`
	// SourceType is a model.SourceType string value (e.g. "gmail",
	// "call", "calendar"). Kept as a plain string here — this file does
	// not import internal/model — and converted in corpus.go.
	SourceType string `json:"source_type"`
	Title      string `json:"title"`
	Content    string `json:"content"`
	// OccurredAt is RFC3339 ("2026-06-10T09:00:00+09:00"); empty means nil
	// — the "unknown event time", not "same day as now", distinction
	// ask.go's formatOccurredAt exists to preserve. A fixture that needs a
	// window/source-filter test to actually exclude this document MUST set
	// this field: assembleRetrieval's plan.OccurredFrom/To predicate drops
	// every row with a nil OccurredAt once either bound is set
	// (model.SearchQuery.OccurredFrom's doc comment).
	OccurredAt string `json:"occurred_at,omitempty"`
}

// Fixture is one askeval E2E test case.
type Fixture struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	// AsOf is this fixture's "now" (RFC3339), injected into the real
	// pipeline via api.Server.WithClock (ask_eval_hooks.go) so "어제"/
	// "지난주" resolve the same way regardless of when this fixture is
	// actually executed.
	AsOf     string      `json:"as_of"`
	Corpus   []CorpusDoc `json:"corpus"`
	History  []Turn      `json:"history,omitempty"`
	Question string      `json:"question"`
	// StandaloneQuestion is the scripted answer to the query-rewrite call
	// (ask_rewrite.go) when History is non-empty — rewriteStandaloneQuestion
	// never calls the LLM for a first turn, so this is ignored and may be
	// left empty when History is empty.
	StandaloneQuestion string `json:"standalone_question,omitempty"`
	// ScriptedAnswer, when non-empty, is returned verbatim (after
	// "{{doc:<alias>}}"/"{{fake}}" placeholder substitution — see llm.go)
	// as the synthesis answer, overriding the default evidence-echo oracle.
	// Required for every adversarial-citation fixture (Gold.
	// ExpectedCitationStatus set); must be empty for every other category,
	// so the default context-conditional oracle — and therefore the
	// RetrievalHit/ContextHit detectors, which read the REAL prompt the
	// oracle saw — actually runs.
	ScriptedAnswer string `json:"scripted_answer,omitempty"`
	// SemanticAliases declares synonym groups the FAKE VECTOR EMBEDDER
	// (corpus.go's hashedEmbedder/hashEmbed, via semanticAliasFold) treats
	// as interchangeable: every member of a group is folded to that group's
	// first ("canonical") member before the bigram hash runs, for BOTH the
	// query text and every chunk's text — so a question that paraphrases a
	// chunk's actual wording (e.g. asks about "마감일" when the transcript
	// says "완료 기준일") still hashes into overlapping features, the same
	// way a real embedding model would place semantically-close paraphrases
	// near each other in vector space.
	//
	// This package's fake embedder otherwise has NO notion of semantics at
	// all (hashEmbed is a literal character-bigram hash — see its doc
	// comment): SemanticAliases exists precisely to simulate a real vector
	// model's paraphrase recall for fixtures that need to exercise the
	// chunk-vector lane finding a passage the question's own words do not
	// literally contain (deep-plan #267 finding: the chunk-vector lane is
	// what actually recovers a paraphrased mid/late-transcript fact once
	// its evidence is preserved through fusion).
	//
	// Deliberately scoped to hashedEmbedder ONLY — corpus.go's lexical
	// lanes (Search's lexicalScore, chunkLexical) never see a folded text,
	// so a synonym never leaks into the fake FTS/document lane. If it did,
	// a fixture built to show "the vector lane recovers what the lexical
	// lane misses" would recover on BOTH lanes and stop being a
	// discriminating test — see docs/ask-evaluation-protocol.md's
	// "semantic_aliases" section for the full rationale and the control
	// check that verifies this scoping.
	SemanticAliases [][]string `json:"semantic_aliases,omitempty"`
	Gold            Gold       `json:"gold"`
}

// Load reads every *.json file in dir as one Fixture, validates it, and
// returns them sorted by ID (determinism: report.go's fixture-set hash and
// Run's sequential execution order both depend on a stable order that does
// not depend on the host filesystem's directory-listing order).
func Load(dir string) ([]Fixture, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("askeval: read fixtures dir %s: %w", dir, err)
	}
	var out []Fixture
	seenIDs := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("askeval: read %s: %w", path, err)
		}
		var f Fixture
		if err := json.Unmarshal(b, &f); err != nil {
			return nil, fmt.Errorf("askeval: parse %s: %w", path, err)
		}
		if err := validate(f); err != nil {
			return nil, fmt.Errorf("askeval: %s: %w", path, err)
		}
		if prev, dup := seenIDs[f.ID]; dup {
			return nil, fmt.Errorf("askeval: duplicate fixture id %q in %s (first seen in %s)", f.ID, path, prev)
		}
		seenIDs[f.ID] = path
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// validate checks the invariants runner.go/metrics.go rely on without
// re-checking at every call site. It does not validate SourceType against
// model.SourceType's enum — corpus.go's buildCorpus surfaces an unknown
// source type as a run-time error instead, keeping this file free of an
// internal/model import.
func validate(f Fixture) error {
	if f.ID == "" {
		return fmt.Errorf("missing id")
	}
	if f.Category == "" {
		return fmt.Errorf("fixture %s: missing category", f.ID)
	}
	if strings.TrimSpace(f.Question) == "" {
		return fmt.Errorf("fixture %s: missing question", f.ID)
	}
	if f.AsOf == "" {
		return fmt.Errorf("fixture %s: missing as_of", f.ID)
	}
	if _, err := time.Parse(time.RFC3339, f.AsOf); err != nil {
		return fmt.Errorf("fixture %s: as_of: %w", f.ID, err)
	}
	if len(f.Corpus) == 0 {
		return fmt.Errorf("fixture %s: empty corpus", f.ID)
	}
	aliases := map[string]bool{}
	for _, cd := range f.Corpus {
		if cd.Alias == "" {
			return fmt.Errorf("fixture %s: corpus doc missing alias", f.ID)
		}
		if aliases[cd.Alias] {
			return fmt.Errorf("fixture %s: duplicate corpus alias %q", f.ID, cd.Alias)
		}
		aliases[cd.Alias] = true
		if cd.SourceType == "" {
			return fmt.Errorf("fixture %s: corpus doc %q missing source_type", f.ID, cd.Alias)
		}
		if cd.OccurredAt != "" {
			if _, err := time.Parse(time.RFC3339, cd.OccurredAt); err != nil {
				return fmt.Errorf("fixture %s: corpus doc %q: occurred_at: %w", f.ID, cd.Alias, err)
			}
		}
	}
	if len(f.History) > 0 && f.StandaloneQuestion == "" {
		return fmt.Errorf("fixture %s: has history but no standalone_question (rewriteStandaloneQuestion will call the scripted LLM)", f.ID)
	}
	switch {
	case f.Gold.ExpectedCitationStatus != "":
		if f.ScriptedAnswer == "" {
			return fmt.Errorf("fixture %s: expected_citation_status set but scripted_answer is empty", f.ID)
		}
	case f.Gold.Answerable:
		if len(f.Gold.Claims) == 0 || len(f.Gold.SupportDocs) == 0 || len(f.Gold.SupportSpans) == 0 {
			return fmt.Errorf("fixture %s: answerable fixture needs claims, support_doc_ids, and support_spans", f.ID)
		}
		if len(f.Gold.Claims) != len(f.Gold.SupportSpans) {
			return fmt.Errorf("fixture %s: claims and support_spans must be the same length (one span per claim)", f.ID)
		}
	}
	for _, alias := range f.Gold.SupportDocs {
		if !aliases[alias] {
			return fmt.Errorf("fixture %s: gold support_doc_ids references unknown corpus alias %q", f.ID, alias)
		}
	}
	if err := validateSemanticAliases(f); err != nil {
		return err
	}
	return nil
}

// validateSemanticAliases rejects a semantic_aliases declaration this
// fixture cannot mean unambiguously: every group needs at least two members
// to fold anything, and a term appearing in two different groups would fold
// to two different canonicals depending on iteration order — semanticFold
// (corpus.go) does not define which one wins, so this is a fixture-authoring
// error, not a runtime ambiguity to silently resolve.
func validateSemanticAliases(f Fixture) error {
	seen := map[string]int{} // member (trimmed) -> group index
	for gi, group := range f.SemanticAliases {
		if len(group) < 2 {
			return fmt.Errorf("fixture %s: semantic_aliases[%d] needs at least 2 members to fold anything, got %d", f.ID, gi, len(group))
		}
		for _, member := range group {
			m := strings.TrimSpace(member)
			if m == "" {
				return fmt.Errorf("fixture %s: semantic_aliases[%d] has an empty member", f.ID, gi)
			}
			if prev, dup := seen[m]; dup && prev != gi {
				return fmt.Errorf("fixture %s: semantic_aliases member %q appears in both group %d and group %d", f.ID, m, prev, gi)
			}
			seen[m] = gi
		}
	}
	return nil
}

// FixtureSetHash is a short, deterministic digest of the exact fixture
// content Load returned — report.go's Provenance.FixtureSetHash. Two Runs
// with different hashes are NOT diffable (DiffReports refuses): the fixture
// set itself changed, so a pass/fail delta would not mean "the code
// changed", it would mean "the test changed".
func FixtureSetHash(fixtures []Fixture) string {
	h := sha256.New()
	for _, f := range fixtures { // Load already sorts by ID.
		b, _ := json.Marshal(f)
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
