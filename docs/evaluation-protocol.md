# Retrieval evaluation protocol

The evaluator measures **search ranking**, not `/ask` temporal planning, context
assembly, citation faithfulness, abstention, or answer accuracy. It does not
reinterpret historical relative-date questions as a validated current-time test.
Those behaviors are covered by a separate offline `/ask` E2E evaluator — see
[`docs/ask-evaluation-protocol.md`](ask-evaluation-protocol.md).

HTTP, MCP, and eval use `search.AssembleService`: document/chunk retrieval,
optional OpenSearch, entity surfacing, active weights, reranker, and the LLM client
are wired consistently. Evaluation freezes effective weights per run, requests
top 10 results, and leaves HyDE off. Reranking defaults to
`SEARCH_RERANK_DEFAULT`; `--rerank=false` explicitly disables it. Configured and
requested reranking are recorded; **neither proves remote reranking succeeded**.
`current.rerank_attempts`/`rerank_failures`/`rerank_succeeded` are the measured
counts: attempts counts queries that actually called the remote reranker, and
failures counts the calls that fell back to the pre-rerank order. `rerank_requested`
true with zero attempts means no reranker was configured; failures equal to
attempts means the reported score is not a reranked score. `run_config`'s
`rerank_outcome` key keeps its historical `not_instrumented` token so config
hashes stay comparable — it is a frozen hash input, not a live claim.

## Labels and scores

- Feedback votes retain their sign, including manual votes. A conflicting
  historical positive/negative document label resolves to negative.
- `--golden` uses only `judge=user`; relevant, irrelevant, and noise judgments are
  exported, including negative-only questions. No questions or judgments are
  created by evaluation.
- Labels outside the default corpus (missing, deleted, disposable, insight) are
  excluded from the evaluation snapshot and counted in `excluded_labels`.
  `excluded_labels.reasons` splits that count by cause — `missing`,
  `status_not_active`, `deleted_at`, `insight`, `disposable` — so a vanished
  document is not read as a deliberate corpus-policy exclusion. A label with
  several causes is counted once, under the first matching cause in that order;
  the reasons always sum to `relevant_labels + irrelevant_labels`.
  This is a conservative eligibility policy: historical rows with active status
  but a non-null deletion timestamp are excluded even if a legacy retrieval lane
  still admits them. Such corpus-policy differences are not a ranking improvement.
  Stored judgments are never rewritten. Queries with no eligible labels are
  excluded. Filtering alone is not evidence of improved quality.
- NDCG's ideal denominator is `min(K, number of relevant labels)`, independent
  of how many results retrieval returned. Missing results cannot inflate scores.
- Search failures remain in the attempted count and positive-query denominator
  as empty results. Any failed query causes a nonzero exit, even if every query
  fails. Failed runs cannot become baselines.
- Positive queries contribute to NDCG/MRR. Negative-only queries contribute to
  FP@10, not undefined relevance scores. Their counts are reported separately.
- An empty label set is an error, not a successful zero-score evaluation.

## Comparable baselines

Migration 034 retains all old rows and adds resolved configuration, code identity,
config/label hashes, attempted/failed counts and FP@10. Config hashes include the
scoring protocol, label source/split, corpus eligibility, lane/model settings,
endpoint hashes (never keys/URLs), weights, HNSW settings and relevant gates.
Label hashes cover sorted queries and positive/negative IDs without exposing
those inputs in the report.

Only complete runs with identical config and label hashes are compared. Old rows
without provenance never match. Code identity is recorded separately so different
revisions of the same protocol can be compared. VCS-free or dirty builds use a
binary digest when no `-ldflags '-X main.codeRevision=<revision>'` is supplied.
Changing model/labels/protocol establishes a new baseline; do not compare its
score to a legacy nightly score as a quality gain. Corpus content may still drift
between runs: freeze the corpus for strong experiments and record the deployment
revisions and collection interval for operational comparisons.

## Event-time windows (`--window`)

The evaluator searches the whole corpus by default. The golden-set candidate
screen does not: it resolves a period phrase in the question text
("지난주", "오늘", ...) to an `occurred_at` window anchored at review time, so a
reviewer judging "yesterday's call" sees yesterday's documents. A label judged
relevant on that screen can therefore rank far below 10 in an unwindowed
evaluation without either measurement being wrong — they retrieved from
different candidate pools.

`--window=plan` applies the same deterministic parser
(`intent.DeterministicWindow`, no LLM call) and passes the resolved
`[from, to)` to `OccurredFrom`/`OccurredTo`. `--as-of=<RFC3339>` anchors the
resolution; it defaults to the run time and is rejected without
`--window=plan`. Questions with no period phrase stay unwindowed. Only the
window is reproduced: the screen's relevance/recency two-stream merge and its
`IncludeRetention` opt-out are not, because the first is review ergonomics
rather than ranking and the second would measure a wider corpus than `/ask`
retrieves from.

`--window=plan` adds `window_mode`, `window_resolver` and `window_as_of_kst_date`
to the config hash and is therefore a separate baseline family; do not report a
plan-mode score as an improvement over a `none`-mode baseline. The as-of anchor
enters the hash as a KST calendar date because every branch of the parser lands
on KST day boundaries, so two runs on the same day are comparable and two runs
on different days are not. `--window=none` adds no keys at all, leaving existing
baselines comparable.

## Per-query diagnostics (`--dump`)

`--dump=<path>` writes one JSON object per query (JSON Lines, file mode 0600).
It answers what an aggregate score cannot: for each relevant label, whether it
was never retrieved (`in_overfetch_pool` false), retrieved but below the page
(`in_overfetch_pool` true, `final_rank` null), or demoted by reranking
(`pre_rerank_rank` above `final_rank`). `lanes_hit` names the lanes that
surfaced it (`document_store`, `chunk_vector`, `opensearch`, `chunk_fts`).
Rows are written for failed searches too.

The file carries **no question text, document title or body**. Queries are
identified by `golden_queries.id`, or by a `sha256:` prefix of the question for
feedback-derived pairs, plus `query_len`. Label timestamps are truncated to a
date. Keep the file outside the repository, in a directory with mode 0700.

```sh
go run ./cmd/eval --golden --no-persist --window=plan \
  --as-of=2026-09-21T09:00:00+09:00 --dump=/secure/eval-diag.jsonl
```

## Retrieval tuning knobs

The search service exposes five experimental knobs. Every knob defaults to the
current production behaviour, so an unset knob changes nothing. Each can be set
per process through the environment and overridden per evaluation run through
the matching `cmd/eval` flag.

| Environment | eval flag | Default | Meaning |
|---|---|---|---|
| `SEARCH_RERANK_OVERFETCH` | `--rerank-overfetch=N` | `0` (off) | Lower bound of the candidate pool sent to the reranker; the pool becomes `max(min(limit*2,200), min(N,200))`. |
| `SEARCH_MERGE_MODE` | `--merge=asymmetric\|symmetric` | `asymmetric` | `symmetric` lets chunk/OpenSearch-only hits compete on RRF score instead of only filling slots the document store left open. |
| `SEARCH_RERANK_BLEND` | `--rerank-blend=replace\|rrf` | `replace` | `rrf` orders results by `1/(60+fused_rank) + w*1/(60+rerank_rank)` instead of replacing the fused order with the reranker's. |
| `SEARCH_RERANK_BLEND_WEIGHT` | `--rerank-blend-weight=W` | `1.0` | Weight `w` of the reranker term in `rrf` blending. |
| `SEARCH_RERANK_INPUT` | `--rerank-input=head\|best_chunk` | `head` | `best_chunk` sends `[source · date · title]` plus the chunk closest to the query instead of the document head. |
| `SEARCH_RECENCY_HALFLIFE_DAYS` | `--recency-halflife-days=D` | `0` (off) | Multiplies fused scores by `(1-α)+α*2^(-age/D)` on queries that carry no event-time window. |
| `SEARCH_RECENCY_ALPHA` | `--recency-alpha=A` | `0.3` | Maximum strength `α` of the recency decay. |

Only non-default knob values are written into the config-hash profile, so a run
with every knob at its default keeps matching existing baselines, while any
enabled knob establishes a separate baseline exactly like `--window=plan`.

## Read-only comparisons

```sh
# Use an existing database and existing labels; no migrations or metric writes.
go run ./cmd/eval --no-persist --split=train --limit=50
# A rerank-off comparison has a different profile and baseline.
go run ./cmd/eval --no-persist --split=train --limit=50 --rerank=false
```

`--no-persist` enforces PostgreSQL `default_transaction_read_only=on`, skips
extension creation/migrations, metric persistence, telemetry and webhook alerts.
It rejects `--check-reindex`, which can write state. An old schema has no compatible
baseline and is read without upgrading it. Embedding/reranking requests still
consume configured remote services; bound the diagnostic sample before running.
The default remains all eligible labels; development comparisons should explicitly
use `--split=train`, not repeatedly inspect holdout results. Sampling is
hash-ordered and deterministic. Keep any private query/result snapshots outside
the repository with directory mode 0700 and file mode 0600.

For before/after production HTTP comparisons, freeze the same eligible training
query subset and label snapshot before deployment. Call only `/api/v1/search`
with the same limit, rerank and filter settings, then compute aggregate metrics
using the same scoring protocol. Record server revisions and failure counts.
Do not call `/ask`, generation, or judgment endpoints to manufacture an evaluation
set, and do not report answer-quality gains from retrieval-only scores.
