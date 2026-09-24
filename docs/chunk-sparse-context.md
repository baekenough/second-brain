# Chunk sparse context (#270)

Deterministic, non-LLM sparse-matching context for the chunk FTS/bigm lane.
Two related experiments:

- **Phase A** — `SEARCH_CHUNK_SPARSE=fuse`: promote the existing chunk
  FTS/bigm lane (`internal/search/search.go`'s `searchChunksFTS`) from a
  "runs only when the primary path found nothing" fallback into the same RRF
  fusion the chunk-vector and OpenSearch lanes already use. No schema
  change — matches raw `chunks.content`.
- **Phase B** — `SEARCH_CHUNK_SPARSE=fuse_ctx` + `SEARCH_CHUNK_SPARSE_CTX`:
  match against a derived table, `chunk_sparse_context` (migration 040),
  that pairs a deterministic title/participants header with each chunk's
  body, so a name mentioned only in the document's metadata (not the chunk
  text itself) can still be found.

Both default OFF (`fallback`). Turning either on changes retrieval, so it
also changes `cmd/eval`'s `config_hash` — see `--chunk-sparse` and
`--chunk-sparse-ctx-version` in `cmd/eval/main.go`.

No LLM involved anywhere in this feature. Every header is built from
document metadata already in Postgres (`internal/chunkctx.BuildSparseText`);
nothing is generated, summarized, or sent to an external API.

## Why phase A has to happen before phase B

`internal/search/search.go`'s chunk FTS/bigm lane historically ran only when
`len(results) == 0` — i.e. after the primary hybrid path (document FTS +
vector + entity lanes) already came back empty. For the overwhelming
majority of production queries the primary path finds *something*, so this
lane almost never executed. Building a derived index (phase B) on top of a
lane that rarely runs would not be measurable — the ablation needs phase A's
raw-content fusion as a baseline to separate "does fusing the lane in at all
help" from "does the derived context help beyond raw content".

## Recipes

Both live in `internal/chunkctx` (shared with the embedding pipeline's
`BuildChunkContextHeader`, extracted from `internal/scheduler` in the P0
refactor of this same release):

| `context_version` | Recipe | Contents |
|---|---|---|
| `v1-tp` | `chunkctx.RecipeTP` | title + participants only. No date, no `[소스]` label. |
| `v1-full` | `chunkctx.RecipeFull` | identical to the embedding header (`BuildChunkContextHeader`) — date, `[소스]` label, title, participants. |

`v1-tp` exists to isolate a specific false-positive risk: a source label like
`[메일]`/`[통화]` can itself match a query containing the word "메일" or
"통화", inflating recall for reasons unrelated to WHO the message is about.
`v1-full` reuses the embedding header exactly, so its ablation result is
directly comparable to what the dense (embedding) lane already sees.

`sparse_text` is always **header + `"\n\n"` + chunk body**, never the header
alone — `plainto_tsquery` ANDs its tokens, so a query like "<name> 예산" (name
in the header, "예산" in the body) needs both halves in ONE `tsvector` to
match at all. Two separate GIN indexes ORed together would not use either
index efficiently.

Returned chunk content is **always `chunks.content`** (the raw chunk), never
`sparse_text` — see `internal/store/chunk_sparse_search.go`'s
`SearchSparseContextFiltered`. `sparse_text` exists purely to be matched
against; it never reaches `SearchResult.Content`, citations, `support_spans`,
or the `/ask` synthesis prompt.

## Freshness (why a stale header can never leak)

`chunk_sparse_context.fingerprint` is `md5(source_type|title|occurred_at|metadata::text)`
of the chunk's parent document, computed by the SAME SQL expression
(`chunkSparseFingerprintSQL`, `internal/store/chunk_sparse_context.go`) used
in both the backfill writer's SELECT and the lane's WHERE clause — neither
side ever computes this value in Go independently, so the two can never
disagree.

`SearchSparseContextFiltered`'s query is a `UNION ALL` of two branches:

1. **fresh** — `chunk_sparse_context` rows whose `fingerprint` still equals
   the document's CURRENT fingerprint (re-evaluated fresh in the same
   query).
2. **raw** — ordinary `chunks.content` matching, for any chunk that has
   `NOT EXISTS` a fresh row in branch 1.

Each branch applies its own `ORDER BY rank DESC LIMIT` BEFORE the
`UNION ALL` (#270 deep-verify MEDIUM), not once afterward on the combined
set — `fresh.rank` (over the header+body `sparse_tsv`) and `raw.rank` (over
plain `content_tsv`) are not on a comparable scale, so a single post-union
limit let a flood of ordinary raw matches crowd every fresh (header) match
out of the result entirely. Limiting per branch guarantees up to `limit`
matches of EACH kind survive; the caller
(`fuseChunkSparseCtx`, `internal/search/chunk_sparse_lane.go`) already
over-fetches and re-aggregates/truncates in Go, so returning up to
`2*limit` rows here is intentional.

A title edit, a metadata merge, or a PII-redaction rewrite (the
`pii_name_redacted` branch of `DocumentStore.AttachTranscript`) changes the
fingerprint on the very next read — before any backfill worker re-runs — so
the chunk falls back to raw content matching immediately. A pre-redaction
name baked into an old `sparse_text` row can therefore never be the reason a
document is found: the row simply stops being "fresh" the instant the
document changes.

This is intentionally over-eager: a metadata edit that does not touch the
header still invalidates the fingerprint. That costs recall (the chunk
falls back to raw matching until the backfill worker catches up), never
staleness (a wrong/stale header is never used).

Verified against a real Postgres database in
`internal/store/chunk_sparse_context_db_test.go`:
`TestSearchSparseContextFiltered_FreshNotCrowdedOutByRawVolume` (per-branch
LIMIT), `TestSearchSparseContextFiltered_ExcludesSoftDeletedDocument`
(neither branch returns a `status='deleted'` document, even with an
existing context row), `TestChunkSparseContext_StaleAfterTitleOnlyUpdate` and
`TestChunkSparseContext_PIIRedaction_OldNameStopsMatching`.

## Storage: why a separate table, not a `chunks` column

`chunks` already carries `content_tsv` (`GENERATED ALWAYS AS ... STORED`) +
a GIN index, and `embedding vector(1536)` + an HNSW index. Adding a column
to `chunks` — generated or not — forces PostgreSQL to rewrite the entire
table (`ACCESS EXCLUSIVE` lock) and rebuild every index on it, HNSW
included. At ~83k rows that is not a cost worth paying for an experimental
feature that may never become the default.

`chunk_sparse_context` (migration 040) is a separate table with its own
`ON DELETE CASCADE` FK to `chunks.id`. `chunks` and `documents` are never
touched by this feature — rollback is deleting rows from (or dropping) this
one table and flipping the knob back.

The FK itself does briefly touch `chunks`: PostgreSQL takes a
`ShareRowExclusiveLock` on the referenced table (`chunks`) for the duration
of the `CREATE TABLE ... REFERENCES chunks(id)` statement, to validate the
constraint is satisfiable — on an empty `chunk_sparse_context` table this is
milliseconds, not a concern, but it means the FIRST application of this
migration briefly blocks concurrent DDL on `chunks` (not normal reads/writes,
which `ShareRowExclusiveLock` does not conflict with).

```sql
CREATE TABLE IF NOT EXISTS chunk_sparse_context (
    chunk_id        BIGINT      NOT NULL REFERENCES chunks(id) ON DELETE CASCADE,
    context_version TEXT        NOT NULL,
    fingerprint     TEXT        NOT NULL,
    sparse_text     TEXT        NOT NULL,
    sparse_tsv      tsvector    GENERATED ALWAYS AS (to_tsvector('simple', sparse_text)) STORED,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (chunk_id, context_version)
);
```

`RunMigrations` (`internal/store/postgres.go`) re-executes every `*.sql`
file on every process boot and has no down-migrations, so 040 is fully
`IF NOT EXISTS` and does **no data movement** — backfilling rows is a
separate, explicit step (`cmd/sparsectx`), never part of the migration
itself.

## Backfill (`cmd/sparsectx`)

```
sparsectx --recipe=v1-tp [--batch 500] [--sleep 200ms] [--dry-run=true|false] [--sweep] [--limit N]
```

| Flag | Meaning |
|---|---|
| `--recipe` | `v1-tp` or `v1-full` — also becomes the row's `context_version`. Required. |
| `--batch` | Chunks per transaction (fetch + upsert, in ONE transaction — see `store.ChunkStore.UpsertSparseContextBatch`). Default 500. |
| `--sleep` | Pause between batches, to throttle load against a database serving live traffic. Default 200ms. |
| `--dry-run` | Default **true** (matches `cmd/recordingbackfill`'s dry-run-by-default convention for scripts that mutate production data). Pass `--dry-run=false` to actually write. |
| `--sweep` | Also re-examine chunks whose row EXISTS but has gone stale (fingerprint no longer matches the document's current one). A plain run only picks up chunks with NO row yet — see idempotency below. |
| `--limit` | Caps the total chunks processed THIS invocation (converted to `ceil(limit/batch)` batches internally). For staged rollout. |

**Idempotent and resumable, with no coordination needed between runs.** Each
batch is one transaction: fetch candidates, compute `sparse_text` in Go
(`internal/chunkctx.BuildSparseText`), and `INSERT ... ON CONFLICT (chunk_id,
context_version) DO UPDATE ... WHERE fingerprint/sparse_text differs`.

Unlike an early version of this feature, candidates are selected by an
**anti-join against `chunk_sparse_context`** — "does this chunk already have
a row (fresh row, for `--sweep`) for this `context_version`" — not by a
`chunk_id > checkpoint` cursor. A cursor can permanently skip a chunk: `id`
values are assigned in statement-call order (`BIGSERIAL`), not commit order,
so two concurrent inserts can commit out of id order; a cursor that has
already advanced past the higher id never looks at the lower one again once
it finally commits. The anti-join has no such blind spot — it asks about
each chunk directly, every call, so a chunk that becomes visible late is
picked up by whichever run happens next. `chunk_sparse_backfill_state.last_chunk_id`
still gets written each batch, but purely as an operator-facing progress
marker; nothing reads it to decide what to fetch.

The one exception is `--dry-run` (never writes anything, so nothing else can
advance the anti-join between ITS OWN batches): `internal/sparsectx.Run`
keeps a small, purely local, in-memory cursor for that single invocation, so
a multi-batch preview pages forward instead of re-fetching batch 1 forever.
This is safe specifically because dry-run writes nothing — the very next
invocation (dry-run or real) recomputes candidates from scratch via the
anti-join, so nothing is ever permanently skipped. The real (`--dry-run=false`)
path never uses this cursor, even across many batches within one long-running
process — every write batch queries the anti-join alone.

One consequence: because there is no cursor to resume from, plain
(non-`--sweep`) runs only ever discover chunks with **no row yet** — new
documents the collector chunked since the last pass. `--sweep` is what picks
up **edited** documents (title rename, metadata merge, PII redaction) whose
existing row went stale. Run the backfill **periodically** (e.g. a
recurring off-peak job), not just once during the initial rollout — a
one-time backfill plus an occasional manual `--sweep` is not enough to keep
a live, continuously-collecting corpus caught up.

Verified in `internal/sparsectx/sparsectx_test.go` (in-memory fake store:
full pass, `--max-batches` early stop, second pass only picks up
still-missing chunks, plain pass finds nothing once caught up, `--sweep`
re-examines only stale rows and leaves fresh ones alone, dry-run writes
nothing AND correctly paginates across multiple batches instead of hanging
(`TestRun_DryRun_PaginatesAcrossBatches_WithoutHanging`), fetch-error
propagation) and against a real Postgres database in
`internal/store/chunk_sparse_context_db_test.go`
(`TestChunkSparseContext_BackfillRoundTrip_Idempotent`,
`TestChunkSparseContext_Backfill_NoSkipOnOutOfOrderCommit`,
`TestChunkSparseContext_BackfillResume_AfterSimulatedMidPassStop`,
`TestChunkSparseContext_CascadeOnChunkReplace`).

### Running the backfill on production safely

1. **Pre-flight (read-only).** Check chunk count and current disk headroom:
   `SELECT count(*) FROM chunks;`, `SELECT pg_total_relation_size('chunks');`,
   and free disk on the Postgres volume. `chunk_sparse_context` grows
   roughly proportional to chunk text size (a header is small; the table
   holds header+body once per `context_version` tried).
2. Follow the standard `ubuntu1-deploy` checklist (backup, image ID check)
   before deploying the release that includes migration 040 — it runs at
   boot on an empty table in milliseconds (no data movement, `IF NOT EXISTS`
   throughout).
3. `sparsectx --recipe=v1-tp --dry-run=true` — confirm the candidate count
   and projected checkpoint progression look sane, with zero writes.
4. `sparsectx --recipe=v1-tp --dry-run=false --limit 1000` (2 small batches)
   — spot-check `chunk_sparse_context` row count and that
   `SearchSparseContextFiltered` still returns raw `chunks.content` (no PII
   printed in any check — count/size queries only).
5. Run the full pass off-peak: `sparsectx --recipe=v1-tp --dry-run=false
   --sleep=200ms` (no `--limit`). Watch `pg_stat_activity` and search p95.
   Interrupting and re-running continues where it left off — no cursor to
   restore, see idempotency above.
6. Run `--sweep` once after the full pass to pick up any document that
   changed mid-pass. Keep running the backfill (with or without `--sweep`,
   depending on whether you only need new chunks or also edited ones)
   periodically afterward — the collector never writes
   `chunk_sparse_context` rows itself.
7. Repeat 3–6 for `v1-full` if that recipe is also being measured.
8. Record index size (`pg_total_relation_size('chunk_sparse_context')`),
   backfill wall time, and search p95 on issue #270.

### Rollback (each step independent)

| Layer | Action |
|---|---|
| Behaviour | Set `SEARCH_CHUNK_SPARSE=fallback` (the default) and restart the server. The context lane (and the phase A fuse lane) are then never queried — byte-identical to pre-#270 behaviour. |
| Data | `DELETE FROM chunk_sparse_context WHERE context_version = 'v1-tp';` (or `TRUNCATE chunk_sparse_context;` for all recipes). |
| Schema | A later migration `04x_drop_chunk_sparse_context.sql` (`DROP TABLE IF EXISTS chunk_sparse_context, chunk_sparse_backfill_state;`) — only after the experiment concludes negative, and only after also editing 040 itself so it does not recreate the table on the next boot. Leaving the (now-empty) tables in place is also a valid, lower-risk option. |

`chunks` and `documents` are never modified by any part of this feature —
no data restore is ever needed for either rollback path.

## Ablation matrix

Run against the golden set (`--window=plan`, prod rerank config, `--dump`),
compare each condition to `C0` with `evalcompare`:

| ID | `--chunk-sparse` | `--chunk-sparse-ctx-version` |
|---|---|---|
| C0 | `fallback` (current prod) | — |
| C1 | `fuse` | — |
| C2 | `fuse_ctx` | `v1-tp` |
| C3 | `fuse_ctx` | `v1-full` |

```
go run ./cmd/eval --golden --window=plan --dump=/tmp/c0.jsonl
go run ./cmd/eval --golden --window=plan --chunk-sparse=fuse --dump=/tmp/c1.jsonl
go run ./cmd/eval --golden --window=plan --chunk-sparse=fuse_ctx --chunk-sparse-ctx-version=v1-tp --dump=/tmp/c2.jsonl
go run ./cmd/eval --golden --window=plan --chunk-sparse=fuse_ctx --chunk-sparse-ctx-version=v1-full --dump=/tmp/c3.jsonl

go run ./cmd/evalcompare --baseline /tmp/c0.jsonl --candidate /tmp/c1.jsonl
go run ./cmd/evalcompare --baseline /tmp/c0.jsonl --candidate /tmp/c2.jsonl
go run ./cmd/evalcompare --baseline /tmp/c0.jsonl --candidate /tmp/c3.jsonl
```

Report NDCG@10, Recall@10, FP@10, p95 latency, `chunk_sparse_context` index
size, and backfill wall time on #270. "Inference cost" is 0 by construction
for every condition — no LLM call anywhere in this feature.

**Decision rule:** only change a default when no measured group is
`regressed` (per `evalcompare`) AND the overall CI lower bound is above 0.
Otherwise keep `fallback` and record the (negative or inconclusive) result
on #270 — a shipped-but-off knob is still a useful artifact for the next
retrieval experiment.

LLM-generated (as opposed to deterministic metadata-derived) sparse context
is explicitly out of scope for this feature — see the professor-triage note
on issue #270: document-level generated summaries would send personal-data
content (mail/call transcripts) to an external API at ~83k-chunk scale and
risk promoting a hallucinated fact to a lexical-match "reason" a document was
retrieved. Any future work here is a separate issue.
