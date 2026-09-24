# /ask E2E evaluation protocol

This protocol covers issue #266's offline end-to-end evaluator for
`POST /api/v1/ask`: the answer/citation/abstention quality
[`docs/evaluation-protocol.md`](evaluation-protocol.md) explicitly excludes
from its own scope (that document measures search ranking only). The
implementation lives in `internal/askeval`, the fixtures in
`eval/ask/fixtures/*.json`, and the CLI entry point in `cmd/askeval`.

## What this evaluator does and does not do

It drives the **real** `/ask` handler (`internal/api`) through `httptest`,
end to end: query rewrite, intent classification, query planning, retrieval
(document store lane + chunk lane), excerpt budgeting, synthesis, and the
deterministic citation validator (issue #268) all run exactly as they do in
production. Only three things are faked, and each fake is itself a real
implementation of the same Go interface production code depends on — not a
mock of `/ask`'s own logic:

| Real dependency | Fake used here | Why it is safe |
|---|---|---|
| Document/chunk store (Postgres) | `internal/askeval`'s `corpus` type, built from one fixture's declared `corpus` field | Implements `search.DocumentSearcher`, `search.ChunkSearcher`, `search.FilteredChunkSearcher`, `search.ChunkLister` — the same interfaces `*store.DocumentStore`/`*store.ChunkStore` satisfy |
| Embedding backend | `hashedEmbedder`: a deterministic bag-of-character-bigrams hash, L2-normalized | Implements `search.EmbeddingEngine`; makes the chunk-vector lane actually run without a network call |
| LLM (rewrite/classify/plan/synthesis) | `scriptedCompleter`: deterministic responses keyed by the request's system-prompt text | Implements `llm.Completer`; see "The scripted LLM" below |

It never touches the network or a real database: `go test ./internal/askeval/...`
and `go run ./cmd/askeval` are CI-safe by construction.

It uses **only synthetic fixture data**. No real personal data (SMS, call
transcripts, emails, etc.) ever appears in `eval/ask/fixtures/`.

## The scripted LLM: a "context-conditional oracle"

`internal/askeval/llm.go`'s `scriptedCompleter` answers each of `/ask`'s
four LLM call sites differently, detected by matching the system prompt's
opening sentence (each prompt is an unexported constant in its own package —
see that file's doc comment for why this package cannot import them
directly):

- **Query rewrite** (`ask_rewrite.go`): returns the fixture's
  `standalone_question` field verbatim. Only called when a fixture has
  `history` — a first-turn question never reaches this call.
- **Intent classification / query planning** (`internal/intent`): always
  returns an error. Both `intent.LLMClassifier` and `intent.LLMPlanner`
  treat an LLM failure as "fall back to the deterministic path" by design
  (their own doc comments), so this is not a degraded test — it is the
  SAME fallback production traffic takes whenever the LLM backend is
  unavailable. Every fixture's retrieval window/source filter is therefore
  decided entirely by `intent.DeterministicWindow`'s regex parser reading
  the fixture's own Korean question text (see the `period_source_filter`
  and `korean_followup` fixtures for examples: "지난주", "오늘", "어제",
  "이번달" all resolve deterministically).
- **Synthesis** (`ask.go`'s Stage 3): this is the "oracle". For each
  `gold.claims[i]` / `gold.support_spans[i]` pair, IF `support_spans[i]` is
  a literal substring of the **exact text `/ask`'s own
  `buildBudgetedAskMessages` placed in front of this call** — not the
  fixture's corpus, not the "sources" SSE event — the oracle emits
  `claims[i]` with a citation to `support_docs[i]`'s real document ID. A
  span that never made it into the prompt yields no claim for it. A
  fixture with zero emittable claims (including every unanswerable
  fixture) gets the fixed abstention phrase used in the exact wording
  `askSystemPromptTemplate` instructs the model to use.

This oracle is explicitly **not** a language model: it has no judgment,
cannot paraphrase, and cannot reason about ambiguity. It exists to answer
exactly one question per fixture — "was the evidence this claim needs
actually shown to a synthesis call, in the pipeline's own real output?" —
deterministically and reproducibly. A fixture whose `scripted_answer` field
is set (the adversarial-citation fixtures) bypasses the oracle entirely and
returns that text verbatim, after `{{doc:<alias>}}`/`{{fake}}` placeholder
substitution.

## Fixture schema

One fixture is one JSON file in `eval/ask/fixtures/`. See
`internal/askeval/fixture.go`'s `Fixture`/`Gold`/`CorpusDoc` doc comments
for the authoritative field-by-field reference; summary:

```jsonc
{
  "id": "st-01-budget",              // unique across the whole directory
  "category": "single_turn",         // grouping label, free-form
  "as_of": "2026-06-10T09:00:00+09:00", // this fixture's injected "now"
  "corpus": [                        // this fixture's ISOLATED document set
    {"alias": "call-1", "source_type": "call", "title": "...",
     "content": "...", "occurred_at": "2026-06-10T08:00:00+09:00"}
  ],
  "history": [                       // optional; prior turns for a follow-up
    {"question": "...", "answer": "..."}
  ],
  "question": "...",
  "standalone_question": "...",      // required iff history is non-empty
  "scripted_answer": "...",          // optional; bypasses the oracle
  "gold": {
    "answerable": true,
    "claims": ["..."], "support_doc_ids": ["call-1"], "support_spans": ["..."],
    "expected_citation_status": "invalid", // adversarial fixtures only
    "expect_inferred_citation": false
  }
}
```

A corpus doc's `alias` (never a UUID) deterministically maps to the same
`uuid.UUID` on every run via `uuid.NewSHA1` over a fixed namespace
(`corpus.go`'s `aliasID`) — a fixture's `gold.support_doc_ids` and
`scripted_answer`'s `{{doc:<alias>}}` placeholders both resolve through the
same function, so a fixture never needs to hardcode a UUID.

## Fixture mix (35 fixtures, ≥30 required)

| Category | Count | What it exercises |
|---|---:|---|
| `single_turn` | 6 | One question, one supporting document, no history |
| `korean_followup` | 5 | 지시어/pronoun resolution ("그 사람", "거기", "그 회의") across a 2-turn conversation, via the real query-rewrite call |
| `period_source_filter` | 5 | `intent.DeterministicWindow` + `explicitRecordSources` narrowing the candidate pool by event-time window and/or source type |
| `call_transcript_mid_late` | 5 | **Known baseline gap** — see below |
| `conflicting_sources` | 3 | Two documents assert different values for the same fact; only the correct (gold) one should be cited |
| `no_evidence` | 3 | Retrieval returns nothing relevant → `finish_reason: "no_evidence"` before synthesis ever runs |
| `irrelevant_evidence` | 3 | Retrieval returns topically-adjacent but non-answering documents → synthesis reaches Stage 3 but must still abstain (`citation_status: "abstained"`), not fabricate |
| `adversarial_citation` | 4 | Fabricated UUID, malformed link, a real-but-unshown document ID, and an allowed-but-flagged inferred-layer citation — see "Adversarial citations" below |
| `document_injection` | 1 | A retrieved document's own content tries to instruct the model to cite a fabricated ID; `validateAskCitations` never trusts document content, only the server-built manifest |

## Metrics (`internal/askeval/metrics.go`)

Every `CaseMetrics` field is a deterministic detector over what the real
pipeline produced — never a semantic judge (see "Judge mode" below):

- **`retrieval_hit`** — every `gold.support_doc_ids` entry's ID appears in
  the `sources` SSE event. Retrieval FOUND the evidence, independent of the
  excerpt budget.
- **`context_hit`** — every `gold.support_spans` string is a literal
  substring of the exact text the oracle actually saw. This is the
  "context assembly succeeded" half; `retrieval_hit=true, context_hit=false`
  is the specific "retrieval succeeded, context assembly failed" case this
  issue's completion criteria ask to distinguish from a plain retrieval
  miss (`retrieval_hit=false`).
- **`answer_correct`** — every `gold.claims` string is a literal substring
  of the produced answer text.
- **`citation_status`** — copied from the `done` SSE event's
  `verification.citation_status` (issue #268's `askCitationStatus`:
  `valid|invalid|missing|abstained|unverified`), or `"no_evidence"` when
  synthesis never ran.
- **`inferred_cited`** — `verification.inferred_cited_ids` is non-empty
  (the "추론을 사실로 인용" regression signal).
- **`abstained_correctly`** / **`false_abstention`** — for an unanswerable
  fixture, did it abstain; for an answerable fixture, did it WRONGLY
  abstain (issue #266 completion criteria: report loss on ordinary
  questions, not just gains on adversarial ones).
- **`answer_bytes`** / **`prompt_bytes`** — byte-size **cost proxies**.
  These are NOT a token count: actual cost depends on the configured
  model's tokenizer, which this offline scripted run never calls. Treat
  them as a relative signal between two runs of the same fixture set, not
  an absolute cost figure.
- **`latency_ms`** — wall-clock milliseconds to the `sources`/first
  `token`/`done` SSE events, measured against `httptest`'s in-process
  request. This measures the harness's own overhead (fake corpus lookup,
  JSON marshalling), NOT production request latency, which depends on a
  real database and a real LLM round-trip this runner never makes.

`Pass` is the single rollup `report.go`'s category counts and
`FailingCaseIDs` are built from — see `metrics.go`'s `computeMetrics` for
the exact rule per fixture shape (adversarial-citation fixtures pass when
the validator reaches the provoked verdict; answerable fixtures pass on
correct-claim-plus-valid-citation; unanswerable fixtures pass on correct
abstention).

## Known baseline: `call_transcript_mid_late` (5/5 fail today)

These five fixtures are a **known, currently-measured, and deliberately
committed** baseline failure — the exact gap issue #267 exists to close,
not a bug in this evaluator. Each fixture's one document has:

1. a **head** whose vocabulary strongly overlaps the question (so
   retrieval finds and ranks it, and `askPassage`'s lexical-window search
   anchors on the head);
2. ~4.2 KB of filler with zero vocabulary overlap with the question;
3. a **tail** carrying the actual fact, in vocabulary the question never
   mentions.

`internal/api/ask_context.go`'s `buildBudgetedAskMessages` caps any single
document's excerpt at `askExcerptBytes` (4096 bytes) **regardless of how
many documents are being retrieved** (`perDoc = min(askExcerptBytes,
(remaining-512)/count)`), so placing the fact past that offset — combined
with a head that wins `askPassage`'s window search — guarantees the fact
never reaches the synthesis prompt. Measured result for all five fixtures:
`retrieval_hit=true`, `context_hit=false`, `citation_status="abstained"`.
This is `/ask` correctly declining to fabricate an answer it cannot
support — a safe failure mode, but a failure mode `#267` (matched-chunk
evidence propagation) is designed to fix by preserving the chunk that
actually matched, rather than re-deriving a window from the whole
document's head.

### Why "omitted-by-budget" is not one of the four adversarial-citation shapes

The original plan called for a fourth adversarial-citation fixture where a
document is selected by retrieval (visible in `sources`) but dropped from
the prompt by the excerpt budget before synthesis
(`askPromptManifest.Omitted`), and a citation of that ID is caught the same
way a fabricated ID is. Deriving `buildBudgetedAskMessages`'s constants by
hand shows this is **structurally unreachable** through fixture corpus
sizing alone:

- `askMessageBytes` (20 KB) is a fixed package constant.
- The question is capped at `askQuestionBytes` (4 KB) and history at
  `askHistoryBytes` (4 KB total), so `remaining` never drops below
  `20480 - 4096 - 4096 = 12288` bytes.
- `selectAskEvidence` caps the candidate count at 12 before the budget loop
  ever runs, so `count <= 12`.
- `perDoc = min(4096, (remaining-512)/count)` is therefore always
  `>= (12288-512)/12 = 981` bytes — and a realistic document header (UUID
  ×2 + source type + a 256-byte-clipped title + a KST timestamp) never
  exceeds ~400 bytes, so the loop's own "header too large for its share"
  break condition (`perDoc <= len(header)+len(excerptMarker)`) cannot fire
  either.

`adv-03-unknown-real-id` substitutes a **topically- and source/window-filtered**
exclusion for the same validator code path instead: a real, resolvable
document ID that this specific question's plan (source type + event-time
window) does not select at all, cited anyway. `validateAskCitations`
cannot distinguish "never selected" from "selected then dropped by
budget" — both reach `askPromptManifest`'s `layerOf` lookup as `ok=false`
— so this fixture exercises the identical detection path
(`UnknownIDs`/`citation_status: "invalid"`) the original scenario would
have.

## Baseline vs. candidate comparison

```sh
go run ./cmd/askeval --out /tmp/baseline.json
# ... make a change to internal/search, internal/api/ask_context.go, etc. ...
go run ./cmd/askeval --out /tmp/candidate.json --baseline /tmp/baseline.json
```

`DiffReports` (`report.go`) refuses to diff two reports whose
`fixture_set_hash` differs — a changed fixture set means the DELTA
describes a test change, not a code change. The diff reports
`improved`/`regressed`/`still_failing`/`still_passing` **per fixture ID**,
never a bare aggregate score: a run where fixture A flips pass→fail while
fixture B flips fail→pass must never read as "no change" (issue #266
completion criteria: "수치 근거 없이 개선을 단정하지 않는다").

## Provenance and judge mode

Every report's `provenance` block records `git_rev`, `fixture_set_hash`,
`cases`, `mode` (`"scripted"` — the only mode this runner implements;
`"configured"` — a real LLM backend — is reserved for future work and
rejected by the CLI today), and `judge_mode` (`"off"` by default). A
semantic judge is **never** implemented in this issue's scope
(`askClaimSupport` stays `askClaimSupportNotEvaluated` at runtime — issue
#268's own doc comment on that type). If a future issue adds one, it must
run in `shadow` mode only: judge disagreement can subtract from a result,
never add one (a judge failure or disagreement must never be read as a
pass).

## Running it

```sh
# Full offline run, text summary to stdout, JSON report to a file:
go run ./cmd/askeval --out /tmp/askeval-report.json

# CI-safe test suite (no network, no database):
go test ./internal/askeval/... -race -count=1
```
