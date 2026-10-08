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
  created by evaluation. `--split` filters feedback-derived pairs only; it does
  not partition the user-judged golden set. Use `--limit` for a deterministic
  bounded golden sample. Reusing these labels during development is not held-out
  validation.
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
`[from, to)` to `OccurredFrom`/`OccurredTo`. Questions with no period phrase stay
unwindowed. Only the window is reproduced: the screen's relevance/recency
two-stream merge and its `IncludeRetention` opt-out are not, because the first
is review ergonomics rather than ranking and the second would measure a wider
corpus than `/ask` retrieves from.

### Anchor: which "now" a relative phrase means

"내일", "이번 주", "지난달" mean nothing without a reference instant. Golden
questions were asked and judged weeks before the run, so resolving all of them
against one global anchor (the run time) puts the labeled documents outside the
window for every relative phrase, and those queries score 0 regardless of
retrieval quality. The anchor is therefore chosen **per query**:

| Selection | Anchor per query | Notes |
|---|---|---|
| `--window=plan` (default, `--window-anchor=judged_at`) | the query's first judgment time (`MIN(judged_at)`), in KST | Reproduces the window the review screen resolved. See below. Pairs without a judgment time fall back to the run time. |
| `--window-anchor=asked_at` | `golden_queries.asked_at` (migration 032), in KST | Semantically the phrase's intended meaning; feedback-derived pairs have no `asked_at` and fall back to their label time. |
| `--as-of=<RFC3339>` | that single instant for every query | Explicit global override. Cannot be combined with `--window-anchor`. |

Both `--as-of` and `--window-anchor` are rejected without `--window=plan`.

**Which anchor were the labels judged under?** The review screen
(`goldenResolveWindow` in `internal/api/golden.go`) anchors at *review time*,
`s.nowFunc()` of the request, deliberately **not** at `asked_at` (its doc comment
explains that anchoring at `asked_at` surfaced stale candidates). So the window
a reviewer actually saw is the one resolved at judgment time. `asked_at` equals
that only when a question was judged shortly after it was asked; for a question
asked on one day and judged weeks later the two differ. `judged_at` is the
faithful replay of the screen and is the default; `asked_at` is the semantically
intended meaning of the phrase. Measured on the golden set, `judged_at` scored
ndcg@10 0.640 against 0.550 for `asked_at` (baseline run, same labels);
`window_applied` and `window_anchor_date` in the dump show every miss.

### Source include set (`--plan-sources`)

`--window=plan` alone reproduces only the window. Production `/ask` also applies
the source include set of the deterministic plan (`intent.DeterministicPlan`,
shared with `LLMPlanner`): an explicit record word (메일/문자/통화/슬랙/노션/노트)
selects that source, a calendar keyword (일정/스케줄/캘린더/약속) or a window that
starts after the anchor's today selects `calendar` only. Without it, calendar
questions compete with sms/call documents for the 20-slot candidate pool and
miss in-window calendar documents.

`--plan-sources` (requires `--window=plan`, default off) applies that set per
query, using the same per-query anchor as the window. No LLM is called. When the
deterministic plan declines a question (e.g. a record question that also names a
calendar topic, such as "내일 일정에 관한 메일") or the question has no period
phrase, the query keeps today's behaviour: the window from
`DeterministicWindow` alone and no source constraint. Enabling it adds
`plan_sources: true` to the config hash (a separate baseline family; the key is
absent when off), and each dump row carries `plan_sources` — the include set that
was actually applied (omitted when none).

### Config hash

`--window=plan` adds `window_mode` and `window_resolver`, plus the anchor:

- `--as-of`: `window_as_of_kst_date` only, exactly as before this change, so
  existing `--as-of` baselines keep matching. The date enters the hash as a KST
  calendar date because every parser branch lands on KST day boundaries.
- `judged_at` / `asked_at`: `window_anchor` (`judged_at` or `asked_at`) and **no**
  date, because the anchors come from the labels, not the run date; the same
  labels give the same series on any day.

Each anchor mode is its own baseline family, and all of them differ from `none`:
do not report a plan-mode score as an improvement over a `none`-mode baseline.
`--window=none` adds no keys at all, leaving existing baselines comparable.

## Per-query diagnostics (`--dump`)

`--dump=<path>` writes a dump v2 file (JSON Lines, file mode 0600). It answers
what an aggregate score cannot: for each relevant label, whether it was never
retrieved (`in_overfetch_pool` false), retrieved but below the page
(`in_overfetch_pool` true, `final_rank` null), or demoted by reranking
(`pre_rerank_rank` above `final_rank`). `lanes_hit` names the lanes that
surfaced it (`document_store`, `chunk_vector`, `opensearch`, `chunk_fts`).
Rows are written for failed searches too. With `--window=plan` every row also
carries `window_anchor_date` (KST date, `YYYY-MM-DD`) — the anchor the query's
relative phrase was resolved against — so a `window_applied` that excludes the
label can be audited without the question text.

The file carries **no question text, document title or body**. Queries are
identified by `golden_queries.id`, or by a `sha256:` prefix of the question for
feedback-derived pairs, plus `query_len`. Label timestamps are truncated to a
date. Keep the file outside the repository, in a directory with mode 0700.

```sh
go run ./cmd/eval --golden --no-persist --window=plan \
  --as-of=2026-09-21T09:00:00+09:00 --dump=/secure/eval-diag.jsonl
```

### Dump v2 schema (`internal/evaldump`, issue #269)

Line 1 is a header object (`"kind":"header"`) carrying the run's provenance —
the same `label_hash`/`config_hash`/`code_revision` values written to
`eval_metrics` — plus `attempted`, `failed`, `label_source`
(`golden-user`/`feedback`), `split`, `window_mode` and `created_at`. Every
following line is a query row (`"kind":"query"`) that keeps every v1 field
and adds:

| Field | Meaning |
|---|---|
| `latency_ms` | This query's read-path latency, the same value `evaluatePairs` measures per query. |
| `ndcg10` | `search.NDCGK(top10, positives, 10)` — the identical function `Aggregate` uses for the macro-average, not a separate re-implementation. `null` when the query has no positive labels (0 and "not scoreable" are different facts). |
| `recall10` | `|top10 ∩ relevant| / |relevant|`. `null` under the same condition as `ndcg10`. |
| `fp10` | The existing `negative_ids_in_top10` count, under the dump-v2 metric name. |

A file with no header line is read as v1: `internal/evaldump.Dump.Header` is
`nil` and `Version` is `1` — provenance is unknown, never assumed to match.
`internal/evaldump.Read`/`Decode` parse both shapes; the out-of-repo
`~/.claude/skills/golden-eval/scripts/compare-dumps.py` still reads v2 rows
fine (it only reads fields it knows and ignores the header line's shape).

## Paired comparison across slices (`cmd/evalcompare`, issue #269)

A macro-average alone hides a regression in one slice while the overall
number improves. `cmd/evalcompare` pairs a baseline and a candidate dump by
`query_id`, groups them by `answer_source:<source_type>` (from the label
document's own source — never from which documents a run happened to
return), `query_source:<...>`, and, optionally, manually reviewed tags from a
slices file, and reports a fixed-seed paired bootstrap confidence interval
per group per metric. It opens no network connection and no database; the
two dump files (and the optional slices file) are its only input.

```sh
go run ./cmd/eval --golden --no-persist --window=plan --dump=/secure/before.jsonl
go run ./cmd/eval --golden --no-persist --window=plan --rerank-input=best_chunk \
  --dump=/secure/after.jsonl
go run ./cmd/evalcompare --baseline=/secure/before.jsonl --candidate=/secure/after.jsonl
```

The binary is named `evalcompare`, not `eval`, so `go build ./cmd/evalcompare`
does not collide with the root `eval/` directory that shadows `go build
./cmd/eval`'s default output name (issue #274).

Slice labels live in a small versioned JSON file, `{"schema":1,"version":
"...","tags":{"<golden_query_id>":["person","time","exact_token", ...]}}`,
keyed by `golden_queries.id` — never by question text. It is meant to live
outside this repository, next to the dumps it labels; pass its path with
`--slices`. The report cites the file's `sha256` so a reader can tell which
labelling version produced a given grouping.

A group needs at least `--min-n` (default 20) paired queries before its
verdict can be anything other than `inconclusive` — the expected, normal
outcome for most slices of a small golden set. `--seed` (default 1) and
`--iterations` (default 10000) fix the bootstrap draws deterministically:
the same two dumps, the same seed, and the same slices file always produce
byte-identical output, and query row order never affects it (pairing is by
`query_id`, sorted before resampling).

**Minimum recommended iterations.** The CI bounds are the 2.5th/97.5th
percentile of the resampled distribution using linear interpolation between
order statistics, so they are never truncated toward zero the way a
nearest-rank index would be — but low `--iterations` still means each
percentile is estimated from fewer resamples and is noisier run to run for a
*different* seed (a fixed seed always reproduces the same output). Keep the
default 10000 for a reported comparison; only drop `--iterations` below
~1000 for fast local iteration while tuning a knob, not for a number you
intend to cite.

**Only the `overall` group gates exit code 1.** Compare runs an independent
95% bootstrap test per group per metric; gating the exit code on "any group
regressed" compounds their false-alarm rates instead of holding to the
nominal 5%. deep-verify's #269 review measured this directly: with 8 slices
and no true difference between baseline and candidate, at least one slice
falsely showed `regressed` in about 20% of 60 null trials. Every group's
`ndcg10`/`recall10`/`fp10` verdict is still computed and printed — each
`Group` in the JSON output carries a `gating` field (`true` only for
`overall`) — but only `overall`'s `ndcg10` verdict can set `Report.Regressed`
/ exit code 1. Non-`overall` groups whose CI has a value additionally carry
an informational `bonferroni_significant` boolean: whether that same
bootstrap distribution would still exclude zero at a Bonferroni-corrected
alpha (`0.05 / <number of non-overall groups>`) instead of the uncorrected
0.05 used for `verdict`. It never changes `verdict` and never feeds the
gate — read it as "would this slice's regression survive strict multiple-
comparison correction", nothing more.

Exit codes:

| Code | Meaning |
|---|---|
| 0 | Compared; the `overall` group's `ndcg10` verdict is not `regressed`. |
| 1 | The `overall` group's `ndcg10` verdict is `regressed` (its 95% CI for `candidate - baseline` falls entirely below zero). A non-`overall` group showing `regressed` never sets this on its own — see "Only the `overall` group gates exit code 1" above. `recall10`/`fp10`/latency never gate this either way. |
| 2 | The two dumps cannot be compared at all: `label_hash` mismatch, a query present on only one side, a duplicate `query_id` within one dump, any `search_failed` row, a header reporting `failed > 0`, a v1 dump without `--allow-v1`, a slices file naming an unknown query id, or a row whose stored `ndcg10` disagrees with the value re-derived from its own `relevant_docs[].final_rank` by more than `1e-9` (a self-consistency check, independent of however the writer computed the field). |
| 3 | Usage error (missing/invalid flags, unreadable file). |

`--allow-v1` downgrades a v1 dump from exit 2 to a compare that still runs,
but every group's verdict is forced to `inconclusive` regardless of what the
deltas show — a v2 header is what lets the tool trust a verdict at all.
Because any `search_failed` row already invalidates the comparison (exit 2)
before any group verdict is computed, a rising failure rate cannot manifest
inside a valid (exit 0/1) comparison; it is always caught earlier as
"invalid", not scored as a regression.

`config_hash` is expected to differ between baseline and candidate — that is
the thing under comparison — and both values are printed in the report.
`--format=json` emits the same data machine-readable; CI can parse
`.groups[].ndcg10.verdict` without re-deriving anything from the text
report.

## Retrieval tuning knobs

The search service exposes several experimental knobs. Every knob defaults to the
current production behaviour, so an unset knob changes nothing. Each can be set
per process through the environment and overridden per evaluation run through
the matching `cmd/eval` flag.

| Environment | eval flag | Default | Meaning |
|---|---|---|---|
| `SEARCH_RERANK_OVERFETCH` | `--rerank-overfetch=N` | `0` (off) | Lower bound of the candidate pool sent to the reranker; the pool becomes `max(min(limit*2,200), min(N,200))`. |
| `SEARCH_MERGE_MODE` | `--merge=asymmetric\|symmetric` | `asymmetric` | `symmetric` lets chunk/OpenSearch-only hits compete on RRF score instead of only filling slots the document store left open. |
| `SEARCH_RERANK_BLEND` | `--rerank-blend=replace\|rrf` | `replace` | `rrf` orders results by `1/(60+fused_rank) + w*1/(60+rerank_rank)` instead of replacing the fused order with the reranker's. |
| `SEARCH_RERANK_BLEND_WEIGHT` | `--rerank-blend-weight=W` | `1.0` | Weight `w` of the reranker term in `rrf` blending. |
| `SEARCH_RERANK_INPUT` | `--rerank-input=head\|best_chunk\|yaml` | `head` | `best_chunk` sends `[source · date · title]` plus the chunk closest to the query instead of the document head. `yaml` sends the same body as YAML fields — see the post-fusion section below. |
| `SEARCH_RECENCY_HALFLIFE_DAYS` | `--recency-halflife-days=D` | `0` (off) | Multiplies fused scores by `(1-α)+α*2^(-age/D)` on queries that carry no event-time window. |
| `SEARCH_RECENCY_ALPHA` | `--recency-alpha=A` | `0.3` | Maximum strength `α` of the recency decay. |
| `SEARCH_CHUNK_SPARSE` | `--chunk-sparse=fallback\|fuse\|fuse_ctx` | `fallback` | 청크 FTS/bigm 레인을 RRF 융합에 상시 참여시킨다(#270). 자세한 내용은 `docs/chunk-sparse-context.md`. |
| `SEARCH_SPARSE_QUERY` | `--sparse-query=raw\|chunk\|chunk_doc` | `raw` | 희소 레인이 질문 원문 대신 `internal/sparseq` 추출 키워드를 쓴다(#276). 아래 절 참고. |
| `SEARCH_ENTITY_QUERY_CONTAINS_NAME` | `--entity-query-contains-name` | `false` | 두 글자 이상 엔티티 이름이 질의에 포함되는지 찾는다. 엔티티 레인의 기존 활성화 조건·필터는 그대로 적용한다. |
| `SEARCH_ENTITY_KEYWORDS` | `--entity-keywords=sparse\|llm` | off (빈 값) | 엔티티 레인이 질문 원문 대신 질문에서 뽑은 저수준 키워드로 엔티티를 찾는다(LightRAG 이중 키워드). 아래 절 참고. |
| `SEARCH_HIGH_LEVEL_KEYWORDS_TO_SPARSE` | `--high-level-keywords-to-sparse` | `false` | `llm` 모드의 고수준(주제) 키워드를 희소 레인(fts·bigm) 키워드에 덧붙인다. `SEARCH_SPARSE_QUERY=chunk\|chunk_doc` 필요. |
| `SEARCH_GRAPH_WEIGHT` | `--graph-weight=W` | `0` (off) | 여섯 번째 RRF 레인: 키워드로 찾은 엔티티와 `entity_relations` 로 1-hop 이웃인 근거 문서. `SEARCH_ENTITY_KEYWORDS` 필요. |
| `SEARCH_GRAPH_HUB_DAMPING` | `--graph-hub-damping` | `false` | 그래프 레인 순위를 `SUM(confidence × 1/ln(e + 시드 엔티티 문서 언급 수))` 로 바꿔 허브 엔티티를 누른다(HippoRAG). `SEARCH_GRAPH_WEIGHT>0` 필요. |
| `SEARCH_GRAPH_EXPAND_BOOST` | `--graph-expand-boost=B` | `0` (off) | 융합 후·리랭크 전, 상위 10건의 엔티티 ∪ 키워드 엔티티와 관계로 이어진 기존 후보만 `score × (1 + B·s)` 로 올린다(재정렬만, 0≤B≤1). |
| `SEARCH_RRF_MISSING_RANK` | `--rrf-missing-rank=cutoff` | off (빈 값) | 결과가 있는 레인에 없는 문서가 0 대신 `w/(k + 레인 상한 + 1)` 을 받는다(R2R 가중 RRF). |
| `SEARCH_SOURCE_STRATIFY_K` | `--source-stratify-k=K` | `0` (off) | 소스별 보조 검색(최대 10). 아래 "융합 이후 노브" 절 참고. |
| `SEARCH_COLLAPSE_CONTACT_DAY` | `--collapse-contact-day` | `false` | 통화·문자 상대-날짜 접기/펼치기. |
| `SEARCH_COLLAPSE_EXPAND_MAX` | `--collapse-expand-max=N` | `3` | 대표 뒤에 펼칠 같은 그룹 문서 수(1~20). `--collapse-contact-day` 필요. |
| `SEARCH_WINDOW_BUCKET_DIVERSIFY` | `--window-bucket-diversify` | `false` | 하루를 넘는 시간창에서 날짜별 최상위 문서를 먼저 세운다. |
| `SEARCH_MMR_LAMBDA` | `--mmr-lambda=λ` | `0` (off) | 리랭크 뒤 상위 30건 MMR, λ∈(0,1]. |
| `SEARCH_RERANK_CALL_CONTEXT` | `--rerank-call-context` | `false` | 통화 리랭커 입력에 `contact_name`을 추가한다. `best_chunk`의 문서 결과에는 본문 앞 250자도 보탠다. 청크 결과는 참여자만 추가하며 전체 1,000자 예산을 유지한다. |

Only non-default knob values are written into the config-hash profile, so a run
with every knob at its default keeps matching existing baselines, while any
enabled knob establishes a separate baseline exactly like `--window=plan`.

### 엔티티 이중 키워드와 그래프 1-hop 레인 (`--entity-keywords`, `--graph-weight`)

LightRAG 의 두 아이디어를 Postgres 안에서 실험 노브로 옮긴 것이다. 기본은 모두
꺼져 있고, 꺼진 상태의 SQL·인자는 바이트 단위로 기존과 같다
(`internal/store/testdata/sparse_query_raw.golden` 가 고정).

- **저수준 키워드**: 현행 엔티티 레인은 `normalized_name LIKE '%질문 전체%'` 라
  문장형 질문에서는 거의 맞지 않는다. `--entity-keywords=sparse` 는
  `internal/sparseq.Extract` 키워드를, `llm` 은 LLM 한 번으로 뽑은
  `{"low_level": 고유명사, "high_level": 주제}` 의 low_level 을 쓴다. 키워드는
  소문자·trim·중복 제거·2~40자·최대 8개로 정규화하고(`store.NormalizeEntityKeywords`),
  `normalized_name = ANY(키워드)` 또는 `LIKE ANY(키워드%)`(LIKE 메타문자 이스케이프)로
  맞춘다. 문서 순위는 맞은 서로 다른 엔티티 수 내림차순, 문서 id 오름차순이다.
- **LLM 폴백**: LLM 이 없거나(`llm.Completer` 비활성)·8초 안에 못 답하거나·JSON 이
  깨졌거나·고유명사가 하나도 없으면 sparse 키워드로 되돌아간다. 검색은 실패하지
  않는다. 키워드 내용·LLM 응답은 로그에 남기지 않고 개수와 실패 사유(고정 문자열)만
  남긴다.
- **고수준 키워드**: `--high-level-keywords-to-sparse` 는 `llm` 모드의 high_level 을
  `sparseq.Extract` 로 다시 풀어 희소 레인(fts·bigm) 키워드 뒤에 붙인다(질문에서 직접
  뽑은 키워드가 먼저, 전체 `sparseq.MaxTerms` 안에서 남는 자리만). vec·summvec 레인은
  건드리지 않는다.
- **그래프 레인**: `--graph-weight>0` 이면 저수준 키워드로 찾은 엔티티를 시드로,
  `entity_relations` 의 from/to 가 시드인 관계의 `evidence_document_id` 를
  `SUM(confidence)` 내림차순으로 순위 매긴 레인이 RRF 에 합류한다. 상태·소스 포함/제외·
  retention·occurred 필터는 엔티티 레인과 똑같이 레인 안에서 `d.` 한정형으로 건다.
  키워드가 없는 질의는 레인이 생기지 않는다. 엔티티 레인(`EntityWeight`)과 독립이다.
- 키워드가 하나도 안 나온 질의의 엔티티 레인은 현행 SQL 을 그대로 탄다.
- 엔티티 레인 자체가 켜져 있어야(`ENTITY_EXTRACTION_ENABLED=true` 또는 명시 가중치)
  `--entity-keywords` 가 엔티티 레인에 효과가 있다. 이 환경변수는 실행 프로필에 들어가지
  않으므로 baseline 과 후보를 같은 환경에서 돌려야 한다.
- 프로필에는 `entity_keyword_mode`, `llm` 이면 `entity_keyword_prompt_version`
  (`search.EntityKeywordPromptVersion`), 켠 경우 `high_level_keywords_to_sparse`,
  `graph_weight`, `sparse_terms_version` 이 들어간다. LLM 모델 이름은 들어가지 않는다.
  LLM 출력은 비결정적일 수 있어 `llm` 실행은 반복해 편차를 확인한다.

```sh
# baseline(노브 off) 대 후보. 두 실행 모두 같은 ENTITY_EXTRACTION_ENABLED 환경에서 돌린다.
export ENTITY_EXTRACTION_ENABLED=true
go run ./cmd/eval --golden --no-persist --window=plan --dump=/tmp/base.jsonl
go run ./cmd/eval --golden --no-persist --window=plan --entity-keywords=sparse --dump=/tmp/kw-sparse.jsonl
go run ./cmd/eval --golden --no-persist --window=plan --entity-keywords=sparse --graph-weight=0.5 --dump=/tmp/kw-sparse-graph.jsonl
go run ./cmd/eval --golden --no-persist --window=plan --entity-keywords=llm --graph-weight=0.5 --dump=/tmp/kw-llm-graph.jsonl
go run ./cmd/eval --golden --no-persist --window=plan --sparse-query=chunk_doc --entity-keywords=llm --high-level-keywords-to-sparse --graph-weight=0.5 --dump=/tmp/kw-llm-full.jsonl
go run ./cmd/evalcompare --baseline=/tmp/base.jsonl --candidate=/tmp/kw-sparse.jsonl
```

### 그래프 허브 감쇠·결과 시드 확장·누락 레인 순위 (`--graph-hub-damping`, `--graph-expand-boost`, `--rrf-missing-rank`)

세 노브 모두 기본 꺼짐이고, 꺼진 상태의 저장소 SQL·인자는 바이트 단위로 같다
(`sparse_query_raw.golden`, `internal/store/graph_expand_test.go`).

- **허브 감쇠**(HippoRAG 노드 특이성): 시드 가중치 `w(e) = 1/ln(e + n(e))`, `n(e)` 는 그
  엔티티의 `document_entities` 행 수다. 시드 집합에 대해서만 `LATERAL` 로 센다
  (`idx_document_entities_entity_id`). 그래프 레인은 근거 문서를
  `SUM(confidence × w(시드 쪽 끝점))` 로 순위 매기고, 양 끝이 모두 시드면 큰 쪽 가중치를
  쓴다. 언급 0 이면 `w=1` 이라 순위가 감쇠 없는 레인과 같고, 1000 건이면 약 0.145 다.
  WHERE·필터·LIMIT 은 감쇠 없는 레인과 같다. `--graph-weight>0` 없이 켜면 eval 이 거부한다.
- **결과 시드 확장**(Graphiti edge_search / Hindsight): 문서 저장소·청크·OpenSearch 융합이
  끝난 후보 풀(소스별 보조 검색 합류 뒤, 보존 페널티·최신성 감쇠·리랭크 전)에서 상위 10건의 엔티티 ∪ 키워드로 찾은
  엔티티를 시드로 삼는다. 저장소(`DocumentStore.GraphSupportCounts`)가 후보마다 시드와
  이어진 서로 다른 관계 수 `n` 을 센다 — 관계의 `evidence_document_id` 가 후보이고 from/to 중
  한쪽이 시드인 관계다. 후보 자신의 엔티티만으로 이어진 관계는 세지 않는다(상위 문서가 자기
  관계로 자기를 올리는 순환 방지). 상태·소스·retention·occurred 필터를 다시 걸고, 후보(최대
  200건)만 대상으로 한다. 승수는 `1 + B·s`, `s = ln(1+n)/ln(1+max n)` ∈ [0,1] 이며 후보를
  더하지 않는다. 최신순 정렬 질의, 저장소 실패, 엔티티가 없는 코퍼스에서는 아무것도 바꾸지
  않는다. 로그에는 개수만 남긴다.
- **누락 레인 순위 cutoff**(R2R): 레인 항이 `COALESCE(w/(k+rank), CASE WHEN COUNT(lane.id)
  OVER () > 0 THEN w/(k + $3 + 1) ELSE 0 END)` 가 된다. `$3` 은 레인 상한(LIMIT)이다.
  결과가 하나도 없는 레인(맞는 엔티티가 없는 엔티티·그래프 레인 등)과 가중치 0 레인은 여전히
  0 이다. 레인 CTE 를 다시 참조하지 않으므로 계획은 바뀌지 않는다.
- 프로필 키: `graph_hub_damping`(그래프 레인이 있을 때만), `graph_expand_boost`,
  `rrf_missing_rank`.

```sh
export ENTITY_EXTRACTION_ENABLED=true
go run ./cmd/eval --golden --no-persist --window=plan --entity-keywords=sparse --graph-weight=0.5 --dump=/tmp/graph.jsonl
go run ./cmd/eval --golden --no-persist --window=plan --entity-keywords=sparse --graph-weight=0.5 --graph-hub-damping --dump=/tmp/graph-damped.jsonl
go run ./cmd/eval --golden --no-persist --window=plan --entity-keywords=sparse --graph-weight=0.5 --graph-expand-boost=0.3 --dump=/tmp/graph-expand.jsonl
go run ./cmd/eval --golden --no-persist --window=plan --rrf-missing-rank=cutoff --dump=/tmp/rrf-cutoff.jsonl
go run ./cmd/evalcompare --baseline=/tmp/graph.jsonl --candidate=/tmp/graph-damped.jsonl
```

### 희소 레인 질의 키워드 (`--sparse-query`, #276)

문서·청크의 tsvector 는 `simple` 설정이라 한국어 어절이 조사까지 붙은 채
하나의 렉심으로 남는다("회의를"). 그래서 "이번 주 회의 일정 알려줘" 같은
문장형 질의는 기존 `plainto_tsquery`(모든 단어 AND)와 `LIKE '%질문 전체%'`
로는 희소 레인에서 0건이 된다. 이 노브를 켜면 `internal/sparseq` 가 질문에서
시간 표현·요청 동사·의문사·서술어 어미·끝 조사를 걷어 낸 키워드(최대 8개)를
뽑고, 희소 레인은 접두 OR tsquery(`'회의':* | '일정':*`, 파라미터 하나로
바인딩)와 키워드별 `LIKE` OR 로 매칭한다. LLM 호출이나 외부 의존성은 없고
같은 질문에는 항상 같은 키워드가 나온다.

| 값 | 키워드를 쓰는 레인 |
|---|---|
| `raw`(기본) | 없음. 생성 SQL 은 #276 이전과 바이트 단위로 같다(`internal/store/testdata/sparse_query_raw.golden` 가 고정). |
| `chunk` | 청크 FTS/bigm 레인(폴백·`fuse`·`fuse_ctx` 의 두 CTE). |
| `chunk_doc` | `chunk` + 문서 하이브리드의 fts·bigm 레인 + 임베딩 없는 fulltext 경로. |

- 어느 값이든 임베딩·리랭커·엔티티 레인·OpenSearch 는 질문 원문을 받는다.
  엔티티 레인의 매칭 방향 문제는 이 노브의 범위가 아니다.
- 살아남은 키워드가 없으면("뭐 있었지?") 그 질의는 `raw` 와 같은 SQL 을 탄다.
- 기본 `--chunk-sparse=fallback` 에서는 청크 FTS 레인이 1차 경로 0건일
  때만 돌기 때문에 `chunk` 범위의 효과를 재려면 `--chunk-sparse=fuse` 또는
  `fuse_ctx` 와 함께 돌린다.
- `raw` 가 아니면 실행 프로필에 `sparse_query` 와 `sparse_terms_version`
  (`sparseq.Version`, 현재 `v3`)이 함께 들어가 별도 baseline 계열이 된다.
  불용어·조사·시간 표현 목록을 바꾸면 `sparseq.Version` 을 올려야 한다 —
  어휘가 다른 실행이 같은 계열로 섞이지 않게 하기 위해서다.
- 추출 키워드는 질문에서 파생된 개인 데이터라 로그·trace·덤프에 싣지 않고
  개수만 남긴다.

```sh
go run ./cmd/eval --golden --no-persist --window=plan --chunk-sparse=fuse --dump=/tmp/c1.jsonl
go run ./cmd/eval --golden --no-persist --window=plan --chunk-sparse=fuse --sparse-query=chunk --dump=/tmp/t1.jsonl
go run ./cmd/evalcompare --baseline=/tmp/c1.jsonl --candidate=/tmp/t1.jsonl
```

### 융합 이후 노브 (`--source-stratify-k`, `--collapse-contact-day`, `--window-bucket-diversify`, `--rerank-input=yaml`, `--mmr-lambda`)

골든셋 실패 유형 넷을 겨냥한 실험 노브다. 전부 기본 꺼짐이며, 꺼진 상태에서는
저장소 SQL·호출 횟수·결과 순서가 노브 도입 전과 같다(`TestPostFusion_KnobsOffIdentity`).
구현은 `internal/search/postfusion.go`·`stratify.go`·`rerank_yaml.go`.

파이프라인 안의 적용 순서:

```
레인 융합 → [보조 검색 합류] → [그래프 지지 승수(--graph-expand-boost)] → 보존 페널티 → 최신성 감쇠 → recent 재정렬 → (FusedIDs)
→ [상대-날짜 접기] → 리랭크/합산 → [MMR] → [날짜 버킷] → [접힌 문서 펼치기] → (PoolIDs) → limit 절단
```

- **소스별 보조 검색** (`--source-stratify-k=K`, 실패 유형: 다른 소스 문서에 밀려
  후보 풀에 못 든 정답, 일정 질문에서 빠진 캘린더). 질의에 소스 포함 집합이 없을 때만,
  일정·통화·문자·메일·노트 중 질의가 제외하지 않은 소스마다 본 저장소 검색과 같은 질의
  (시간창·제외 소스·retention 제외·가중치·키워드 동일)를 소스 하나로 제한해 `Limit=K` 로
  동시에 돌린다. 본 검색과 병렬이며 같은 ctx 를 쓴다. 실패한 소스는 경고 로그만 남기고
  빠진다. 각 소스 상위 K건은 전역 목록과 `Σ 1/(60+rank)` RRF 로 합쳐지고(두 목록에 다
  있으면 두 항 모두), 결과는 합집합이라 후보 풀이 최대 `소스 수 × K` 건 커진다. 리랭크를
  켜면 리랭커 입력도 그만큼 늘어난다. 저장소 질의가 최대 5회 더 나가므로 지연시간은
  `slog.Debug("search: source stratify arms done", elapsed_ms)` 와 eval 의 p50/p95 로 본다.
  보조 검색 후보는 `--dump` 의 레인 이름 `source_stratify` 로 남는다.
- **상대-날짜 접기** (`--collapse-contact-day`, `--collapse-expand-max=N`, 실패 유형: 월
  단위 창에서 같은 상대 통화·문자가 상위를 독식해 정답이 11~14위). 문자·통화 문서를
  (소스, 상대, KST 날짜)로 묶는다. 상대는 `metadata.number_hash` → `metadata.thread_id` →
  SourceID 의 번호 해시 구간(`sms:{ms}:{hash}:…`, `call-log:{ms}:{hash}:…`) 순으로 찾고,
  없거나 occurred_at 이 없으면 묶지 않는다. 리랭크 전에 그룹마다 점수 최상위 대표만 순위에
  남겨 리랭커 예산과 상위 자리를 연다. 최종 순서가 정해진 뒤 대표 바로 뒤에 같은 그룹을 점수
  순으로 N건(기본 3) 다시 펴고, 나머지는 목록 맨 뒤에 점수 순으로 붙인다. 후보는 하나도
  버리지 않는다(PoolIDs 에 모두 남는다). 그룹 키는 로그·덤프에 싣지 않는다.
- **날짜 버킷 다변화** (`--window-bucket-diversify`). occurred 창이 KST 로 하루를 넘을 때만
  (한쪽 경계만 있는 열린 창 포함), 후보 풀 전체에서 날짜마다 순위 최상위 문서 하나씩을 현재
  순위 순으로 먼저 세우고 나머지를 그 뒤에 둔다. occurred_at 없는 문서는 첫 바퀴에서 빠진다.
- **리랭커 YAML 입력** (`--rerank-input=yaml`, 실패 유형: 리랭커가 맥락 없이 정답을 내림).
  best_chunk 와 같은 본문을 `source`/`date`(KST 일 단위)/`title`/`counterpart`(연락처 표시
  이름만, 번호처럼 보이면 생략)/`text`(literal block) YAML 로 보낸다. 문서당 1,000 rune
  예산은 같다. 전화번호·number_hash·SourceID 는 싣지 않는다. `--rerank=true` 가 필요하고
  `--rerank-call-context` 와는 같이 쓸 수 없다. 프로필에 `rerank_yaml_version` 이 들어간다.
- **MMR** (`--mmr-lambda=λ`). 리랭크·합산 뒤 상위 30건에서
  `λ·rel − (1−λ)·max cos(같은 소스로 이미 뽑힌 문서)` 를 탐욕적으로 고른다. rel 은 원점수가
  아니라 창 안 순위로 정한다: `rel = 11/(10+rank)`(1위 1.0, k=10). 리랭커가 상위 N건만
  돌려주면 나머지는 점수 0 으로 채워지는데, 리랭커 점수가 음수일 때 점수 정규화를 쓰면 그
  꼬리가 위로 올라오기 때문이다. k 는 RRF 의 60 대신 10 — 60 이면 30건 창 안 관련도가
  1.0→0.68 로 거의 평평해 유사도 벌점만 남는다. 문서 임베딩이 없는 후보는 자기 자리를 지키고, 재배치는 임베딩이
  있는 후보들의 자리 안에서만 일어난다. 점수는 바꾸지 않는다.
- `Sort=recent` 질의에는 접기·날짜 버킷·MMR 이 적용되지 않는다(순서는 시간이 정한다).
- 효과 없는 조합은 eval 이 거부한다: `--collapse-expand-max` 만 단독, `--rerank-input=yaml`
  인데 리랭크 꺼짐, 범위 밖 `--source-stratify-k`/`--mmr-lambda`.
- 프로필에는 켠 노브만 들어간다: `source_stratify_k`, `collapse_contact_day`+`collapse_expand_max`,
  `window_bucket_diversify`, `rerank_input`+`rerank_yaml_version`, `mmr_lambda`+`mmr_window`+`mmr_rank_k`.

```sh
go run ./cmd/eval --golden --no-persist --window=plan --rerank=true --dump=/tmp/base.jsonl
go run ./cmd/eval --golden --no-persist --window=plan --rerank=true --source-stratify-k=5 --dump=/tmp/strat.jsonl
go run ./cmd/eval --golden --no-persist --window=plan --rerank=true --collapse-contact-day --window-bucket-diversify --dump=/tmp/collapse.jsonl
go run ./cmd/eval --golden --no-persist --window=plan --rerank=true --rerank-input=yaml --rerank-blend=rrf --dump=/tmp/yaml.jsonl
go run ./cmd/eval --golden --no-persist --window=plan --rerank=true --mmr-lambda=0.7 --dump=/tmp/mmr.jsonl
go run ./cmd/evalcompare --baseline=/tmp/base.jsonl --candidate=/tmp/strat.jsonl
```

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
Feedback-derived development comparisons should explicitly use `--split=train`,
not repeatedly inspect holdout results. `--golden` always uses all eligible
user-judged labels (or its deterministic `--limit` sample); it currently has no
train/holdout split. Evaluating that same golden set after development does not
provide independent held-out evidence. Sampling is
hash-ordered and deterministic. Keep any private query/result snapshots outside
the repository with directory mode 0700 and file mode 0600.

For before/after production HTTP comparisons, freeze the same eligible training
query subset and label snapshot before deployment. Call only `/api/v1/search`
with the same limit, rerank and filter settings, then compute aggregate metrics
using the same scoring protocol. Record server revisions and failure counts.
Do not call `/ask`, generation, or judgment endpoints to manufacture an evaluation
set, and do not report answer-quality gains from retrieval-only scores.

검색 후속 실험(#264, #281, #283, #284): `raw` 기본값은 유지한다. 키워드
모드에서는 LIKE 와일드카드를 이스케이프하고 문서 본문·연락처 후보를 별도로
찾으며, 원문 전체의 `bigm_similarity` 동점 계산을 생략한다. sparseq v3는
세 글자 `-한` 이름을 보존한다(`통화한` 등 명시한 동사 제외). 새 엔티티·통화
노브는 기본값이 꺼져 있다. 폐기용 DB의 가상 데이터·EXPLAIN 검사는 정확성과
인덱스 사용 가능성을 검증하지만 실제 검색 품질이나 운영 지연 개선을 입증하지
않는다. 기본값을 바꾸기 전 같은 날·같은 `--as-of`로 골든 평가를 다시 실행하고,
통화·person slice와 `92a9dba4`, `60ab999e` 덤프를 비교해야 한다.

두 노브의 영향은 각각 기준 실행과 비교한다. 아래 예시는 기준 시각과
`best_chunk` 설정을 고정하고 한 번에 노브 하나만 켠다. `/secure`는 앞서
설명한 권한으로 준비한 로컬 평가 디렉터리로 바꾼다. 엔티티 실험은 기존
엔티티 추출 활성화 조건과 엔티티 레인 가중치가 충족되어야 효과가 있다.

```sh
go run ./cmd/eval --golden --no-persist --window=plan \
  --as-of=2026-09-27T09:00:00+09:00 --rerank=true --rerank-input=best_chunk --dump=/secure/base.jsonl
go run ./cmd/eval --golden --no-persist --window=plan \
  --as-of=2026-09-27T09:00:00+09:00 --rerank=true --rerank-input=best_chunk \
  --entity-query-contains-name --dump=/secure/entity.jsonl
go run ./cmd/eval --golden --no-persist --window=plan \
  --as-of=2026-09-27T09:00:00+09:00 --rerank=true --rerank-input=best_chunk \
  --rerank-call-context --dump=/secure/call.jsonl
go run ./cmd/evalcompare --baseline=/secure/base.jsonl --candidate=/secure/entity.jsonl
go run ./cmd/evalcompare --baseline=/secure/base.jsonl --candidate=/secure/call.jsonl
```

FP@10의 차이도 `candidate - baseline`으로 표시한다. NDCG·Recall과 달리
FP가 늘면 악화(`regressed`), 줄면 개선(`improved_candidate`)이다. 전체
회귀 종료 코드는 기존대로 NDCG만 기준으로 삼으며 FP 판정은 진단용이다.
