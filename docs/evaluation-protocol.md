# Retrieval evaluation protocol

The evaluator measures **search ranking**, not `/ask` temporal planning, context
assembly, citation faithfulness, abstention, or answer accuracy. It does not
reinterpret historical relative-date questions as a validated current-time test.
Those behaviors require separate tests and human judgments.

HTTP, MCP, and eval use `search.AssembleService`: document/chunk retrieval,
optional OpenSearch, entity surfacing, active weights, reranker, and the LLM client
are wired consistently. Evaluation freezes effective weights per run, requests
top 10 results, and leaves HyDE off. Reranking defaults to
`SEARCH_RERANK_DEFAULT`; `--rerank=false` explicitly disables it. Configured and
requested reranking are recorded; **neither proves remote reranking succeeded**.
The current report marks execution outcome `not_instrumented` because the service
may fall back on remote errors.

## Labels and scores

- Feedback votes retain their sign, including manual votes. A conflicting
  historical positive/negative document label resolves to negative.
- `--golden` uses only `judge=user`; relevant, irrelevant, and noise judgments are
  exported, including negative-only questions. No questions or judgments are
  created by evaluation.
- Labels outside the default corpus (missing, deleted, disposable, insight) are
  excluded from the evaluation snapshot and counted in `excluded_labels`.
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
