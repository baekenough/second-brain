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
  "semantic_aliases": [              // optional; see "semantic_aliases" below
    ["마감일", "완료 기준일"]
  ],
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

### `semantic_aliases`: simulating a vector model's paraphrase recall

`hashedEmbedder` (see the fake-dependency table above) is a **literal**
character-bigram hash — it has no notion of meaning at all, so two Korean
phrases that mean the same thing but share no characters (e.g. "마감일" and
"완료 기준일") embed to unrelated vectors and never cosine-match. A real
embedding backend recovers exactly this kind of paraphrase; this fake one
cannot, which is a problem for any fixture that specifically needs to
exercise "the chunk-vector lane finds a passage the question's own words do
not literally contain" (the `call_transcript_mid_late` category's whole
point — see below).

`semantic_aliases` closes that gap in a narrow, explicit, deterministic way.
Each entry is a synonym group; every member after the first ("canonical")
member is folded to the canonical member — via `corpus.go`'s
`semanticAliasFold` — before the bigram hash runs, for BOTH the query text
and every chunk's text. Concretely, `[["마감일", "완료 기준일"]]` makes a
chunk containing "완료 기준일" hash as if it said "마감일" instead, so a
query that says "마감일" (and never literally says "완료 기준일") still
cosine-matches it — simulating, crudely but deterministically, what a real
embedding model does for that pair of phrases natively.

**Scope — vector lane only.** `semanticAliasFold`'s output feeds ONLY
`hashedEmbedder.Embed`/`EmbedBatch` and `corpus.chunkVector`'s own
`hashEmbed` call. It is never applied to `corpus.Search`'s `lexicalScore` or
to `chunkLexical` (the fake document-store and chunk-FTS lanes) — see
`Fixture.SemanticAliases`' doc comment (`internal/askeval/fixture.go`) for
the full rationale. This matters for correctness, not just tidiness: if a
synonym leaked into the lexical lanes too, a fixture built to show "the
chunk-vector lane alone recovers this paraphrase" would pass via the
lexical lane as well, and would no longer distinguish anything —
`TestSemanticAliasFold_AffectsOnlyVectorLane` (`internal/askeval/corpus_test.go`)
pins this scoping directly, asserting the fake document lane and chunk-FTS
lane both return zero results for a paraphrase query that the fake vector
lane (after folding) does find.

A naive fixture author's first instinct — just rewrite the question to
literally quote the document's own wording — does not need this mechanism
at all, and also does not test anything: `askPassage`'s pre-#267 lexical
window search scans the WHOLE document for the query's own literal words,
so a question containing the document's exact phrasing would already find
the right passage even without issue #267's chunk-evidence propagation,
making the fixture unable to tell the two states apart. `semantic_aliases`
exists specifically for the fixture shape that DOES discriminate: a
question phrased so its own words appear NOWHERE in the document (so the
pre-#267 lexical fallback provably fails — see
`call_transcript_mid_late`'s current fixtures, each engineered so
`askPassage`'s window search scores `0` everywhere), while still being
answerable because the (simulated) vector lane can bridge the paraphrase.

## Fixture mix (40 fixtures, ≥30 required)

| Category | Count | What it exercises |
|---|---:|---|
| `single_turn` | 6 | One question, one supporting document, no history |
| `korean_followup` | 5 | 지시어/pronoun resolution ("그 사람", "거기", "그 회의") across a 2-turn conversation, via the real query-rewrite call |
| `period_source_filter` | 5 | `intent.DeterministicWindow` + `explicitRecordSources` narrowing the candidate pool by event-time window and/or source type |
| `call_transcript_mid_late` | 6 | Paraphrased/pronoun-follow-up questions whose gold fact sits in a LATE chunk of a long document, including one (`ctm-06`) at the document's exact tail — see below |
| `conflicting_sources` | 3 | Two documents assert different values for the same fact; only the correct (gold) one should be cited |
| `no_evidence` | 5 | 3 abstention fixtures + 2 fabrication self-tests (`ne-04`/`ne-05`) — see "`no_evidence`/`irrelevant_evidence`: two different things being tested" below |
| `irrelevant_evidence` | 5 | 3 abstention fixtures + 2 fabrication self-tests (`ie-04`/`ie-05`) — see below |
| `adversarial_citation` | 4 | Fabricated UUID, malformed link, a real-but-unshown document ID, and an allowed-but-flagged inferred-layer citation — see "Adversarial citations" below |
| `document_injection` | 1 | A retrieved document's own content tries to instruct the model to cite a fabricated ID; `validateAskCitations` never trusts document content, only the server-built manifest |

### `no_evidence`/`irrelevant_evidence`: two different things being tested

Each of these categories carries two DIFFERENT kinds of fixture, and it
matters which kind a reader is looking at:

1. **Abstention fixtures** (`ne-01`–`ne-03`, `ie-01`–`ie-03` — no
   `scripted_answer`). These exercise the REAL pipeline's abstention
   plumbing end to end: retrieval genuinely finds nothing usable (or
   something topically adjacent but non-answering), synthesis is reached (or
   skipped, for `no_evidence`), and the resulting `finish_reason`/
   `citation_status` is checked. **They test the pipeline's actual
   behavior for an unanswerable question.**

2. **Fabrication self-tests** (`ne-04`/`ne-05`, `ie-04`/`ie-05` —
   `scripted_answer` set). These do NOT test the pipeline's own resistance
   to fabrication at all — the "scripted LLM" section above explains why:
   `scriptedCompleter`'s default oracle ALWAYS returns the fixed abstention
   phrase whenever `gold.support_spans` is empty, which is true for every
   `no_evidence`/`irrelevant_evidence` fixture regardless of what the real
   prompt actually contained (reproduced: injecting an answering document
   into `ne-01`'s corpus still PASSes, because the oracle never even looks
   at the prompt for a fixture with no spans to find). Before these four
   fixtures existed, that meant `no_evidence`/`irrelevant_evidence` could
   **never fail**, no matter how badly a real regression broke abstention
   handling — nothing ever exercised the "the pipeline answered anyway"
   path.
   `ne-04`/`ne-05`/`ie-04`/`ie-05` close that gap by setting
   `scripted_answer` to FORCE a fabricated, non-abstaining, cited answer
   past the oracle, so `metrics.go`'s `fabricated_answer` detector (see
   below) has something real to catch — proving the harness's OWN
   fabrication-detection metric still works, the same way
   `adversarial_citation`'s fixtures prove issue #268's validator still
   works. The PASSING outcome for these four is that
   `fabricated_answer=true` fires (see `TestRun_DistinguishesFabricatedAnswers`,
   `internal/askeval/runner_test.go`) — not that the scripted answer
   abstains, which it deliberately does not.
   `ie-04` in particular exercises the sharpest edge: its corpus document IS
   topically close enough to be retrieved and shown (the "irrelevant
   evidence" shape), so citing its own real document ID reaches
   `citation_status: "valid"` even though the specific fact stated is wrong
   (a different project's budget) — issue #268's validator only proves a
   cited ID was actually shown to the model, never that it supports the
   claim next to it. `fabricated_answer` is deliberately independent of
   `citation_status` for exactly this reason.

## Metrics (`internal/askeval/metrics.go`)

Every `CaseMetrics` field is a deterministic detector over what the real
pipeline produced — never a semantic judge (see "Judge mode" below):

- **`retrieval_hit`** — every `gold.support_doc_ids` entry's ID appears in
  the `sources` SSE event. Retrieval FOUND the evidence, independent of the
  excerpt budget. **Omitted from the JSON report** (Go: `*bool`, nil) for
  any fixture shape where it is not evaluated at all (e.g. an unanswerable
  fixture with no `support_doc_ids`) — a bare `false` there would read as a
  real negative result to a report consumer scanning JSON, when in fact
  retrieval was never checked for that fixture.
- **`context_hit`** — every `gold.support_spans` string is a literal
  substring of the exact text the oracle actually saw. This is the
  "context assembly succeeded" half; `retrieval_hit=true, context_hit=false`
  is the specific "retrieval succeeded, context assembly failed" case this
  issue's completion criteria ask to distinguish from a plain retrieval
  miss (`retrieval_hit=false`). Omitted (nil) when not applicable, same as
  `retrieval_hit`.
- **`answer_correct`** — every `gold.claims` string is a literal substring
  of the produced answer text. Omitted (nil) for any fixture shape outside
  the plain answerable case (adversarial-citation and unanswerable
  fixtures never populate this field).
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
- **`fabricated_answer`** — for an unanswerable fixture (no
  `expected_citation_status`), did the pipeline produce a NON-abstaining
  answer that actually cited something anyway. Computed independently of
  `citation_status` (it does not care whether the citation resolved) so a
  future loosening of the abstention-phrase/citation-status logic itself
  cannot silently make this pass by definition. See "`no_evidence`/
  `irrelevant_evidence`: two different things being tested" above for why
  this metric exists and which fixtures exercise it.
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
the exact rule per fixture shape: adversarial-citation fixtures pass when
the validator reaches the provoked verdict; answerable fixtures pass on
correct-claim-plus-valid-citation; unanswerable fixtures pass on correct
abstention — UNLESS `scripted_answer` is set (the fabrication self-tests),
in which case Pass tracks `fabricated_answer` instead, because the
fixture's whole point is that the scripted answer does NOT abstain.

## `call_transcript_mid_late`: resolved by #267 (was a 5/5 known baseline gap)

These five fixtures were originally a **known, currently-measured, and
deliberately committed** baseline failure — the exact gap issue #267
closed, not a bug in this evaluator. Issue #267 (matched-chunk evidence
propagation) fixed the underlying pipeline gap; the fixtures below are the
ones that actually discriminate the before/after behaviour, and all five
now pass. `TestRun_CallTranscriptMidLate_MatchedChunkEvidence`
(`internal/askeval/runner_test.go`) pins this as a regression test.

### Fixture shape

Each fixture's one document has:

1. a **head paragraph** (~1.5 KB): an intro sentence plus filler text with
   zero vocabulary overlap with the question;
2. a **tail paragraph** (~1.5 KB, split into its own chunk by
   `internal/chunker`): more filler, then the fact sentence, then a short
   wrap-up sentence — the wrap-up exists so the fact is never the literal
   last byte of the document (see "Why the tail needs trailing text" below);
3. a **question** phrased so that NONE of its own words are a literal
   substring anywhere in the document — a paraphrase (e.g. "마감일" for the
   document's "완료 기준일") for `ctm-01`–`ctm-03`, or a Korean pronoun
   follow-up ("그거 새로 뽑을 인력이 몇 명이래?") whose `standalone_question`
   rewrite also paraphrases, for `ctm-04`/`ctm-05`. Both shapes make issue
   #267's two independent fixes relevant: chunk-evidence propagation itself,
   and `buildBudgetedAskMessages`'s `excerptQuery` parameter (the
   standalone-rewritten question, not the user's literal follow-up wording,
   drives the lexical FALLBACK window when evidence is absent).

### Why the paraphrase is necessary, not decorative

A naive fixture author's first instinct — rewrite the question to literally
contain the document's own wording (e.g. ask "완료 기준일이 언제야?" instead
of paraphrasing) — does not test anything: `internal/api/ask_context.go`'s
`askPassage` scans the WHOLE document for literal word matches, so a
question containing the document's exact phrasing finds the right window
via plain lexical search alone, with or without issue #267. The fixtures
here instead use `semantic_aliases` (see above) so the question's own words
never appear anywhere in the document at all — confirmed by construction:
a faithful port of `askPassage`'s window-scoring loop against each
fixture's exact content/question pair scores `0` at every window position,
meaning `askPassage` cannot distinguish the head from anywhere else and
falls back to its initial value (the document's start).

### Why the tail needs trailing text (historical — fixed, see ctm-06)

`ctm-01` through `ctm-05` each carry a short wrap-up sentence after their
gold fact, so the fact is never the literal last byte of the document. That
was originally a fixture-side workaround for a genuine product bug: without
a trailing sentence, `internal/api/ask_context.go`'s `windowAround` — which
centres the excerpt window on the located evidence span — clamped its
window's right edge to `len(content)` (there is nothing further to
include), then prepended a `"[앞부분 생략] "` marker; the combined text now
exceeded the excerpt budget by the marker's byte length, and
`clipAskText`'s safety-net re-clip trimmed that many bytes off the END —
which, with no trailing buffer, was the fact itself. Confirmed by
reproducing it against an earlier draft of these fixtures (see the fixing
commit's message for the byte-offset evidence).

**This bug is now fixed for real, not worked around.** `windowAround`
reserves the prefix/suffix marker bytes out of budget BEFORE the
surrounding window is sized (`evidencePrefixMarker`/`evidenceSuffixMarker`
in `internal/api/ask_context.go`), so the window itself — plus whichever
markers end up attached — is guaranteed to fit budget without ever needing
to trim into the evidence afterward. When the located span sits at the
window's tail, the window's LEFT edge simply gives up more room instead;
nothing is trimmed off the fact. This holds regardless of where the
evidence sits (head / middle / exact tail) as long as the span itself fits
within budget minus both markers.

`ctm-01`–`ctm-05` keep their trailing wrap-up sentences unchanged — they
still read as realistic transcript endings, and removing them now would
just be churn. **`ctm-06-tail-conclusion`** is the new, dedicated regression
fixture: its gold fact IS the literal last byte of the document (no
wrap-up sentence follows it at all), specifically to catch this exact bug
if `windowAround`'s reserve-before-sizing invariant ever regresses.
`TestRun_CallTranscriptMidLate_MatchedChunkEvidence`
(`internal/askeval/runner_test.go`) covers all six `call_transcript_mid_late`
fixtures, `ctm-06` included.

### `ctm-06`'s fake-vector ranking margin

`ctm-06`'s content was originally authored the same way as `ctm-01`–`ctm-05`
(one long, mostly-identical filler sentence repeated across both the head
and tail halves) and its gold chunk's `hashedEmbedder` cosine score beat the
runner-up chunk by only ~0.007 (0.3818 vs 0.3748) — fragile enough that an
unrelated `hashEmbed`/`semanticAliasFold` change, or even a single extra
hash collision in the 48-dimension fake embedding space, could silently
flip which chunk the fake vector lane ranks first without any test failing
to say so. It has since been re-authored: the head is longer and uses
topic-neutral filler with no vocabulary overlap with the gold fact, and the
tail is short and dense with the gold fact's own vocabulary (repeating the
`semantic_aliases`-folded "승인 기한" phrase once more before the final
sentence), pushing the margin to ~0.28. `TestRun_CTMFakeVectorMargin`
(`internal/askeval/runner_test.go`) asserts a per-fixture floor for every
`call_transcript_mid_late` fixture's own margin (`ctm-06`'s floor is 0.02;
`ctm-01`–`ctm-05` keep their original, more fragile margins and floors set
with headroom under their currently-measured values — re-authoring them is
a separate, not-yet-scheduled follow-up), so a future regression that
erodes or flips any of these margins fails loudly with the fixture ID and
both scores in the message, instead of silently relying on the fixture
still happening to rank correctly.

### Control check (pre-#267 vs. HEAD)

Re-running these fixtures against the pre-#267 code (`git revert` of #267's
commit in a scratch worktree, current fixture files copied in) reproduces
the original failure mode exactly: all five `retrieval_hit=true,
context_hit=false, citation_status="abstained"` — the document IS found,
but the excerpt never reaches the fact. At HEAD (#267 applied), all five
`retrieval_hit=true, context_hit=true, answer_correct=true,
citation_status="valid"`. Both runs share the same
`fixture_set_hash`, so `cmd/askeval --baseline` diffs them directly:
`improved=5 regressed=0 still_failing=0 still_passing=30`. See the fixing
commit's message for the exact command transcript.

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
