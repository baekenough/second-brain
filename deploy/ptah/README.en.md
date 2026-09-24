# Switching the embedding model with Ptah

[한국어](README.md)

This directory switches second-brain's vectors to a new model (here
OpenAI `text-embedding-3-small`, 1536 dimensions, to a local `bge-m3`,
1024 dimensions) without draining the tables and without mixing two models
in one index. [Ptah](https://ptah.run) builds the new vectors beside the
live ones; the application switches to them only after they are complete
and verified.

## Why

Today a model change has three paths, and each has a gap:

- `EMBEDDING_REEMBED_ENABLED=true` re-embeds in place. Until it finishes,
  one HNSW index holds vectors from two models — the state migration 037
  was written to make visible.
- Migrations 011 and 015 change the vector dimension only on an empty
  table. With data they skip, and the `docs/embedding-dimension.md` they
  point to does not exist.
- The full re-embed in `TODO.md` resets `collected_at` and waits for the
  scheduler. Nothing reports when it is done.

## What changes

Ptah keeps one generation per vector family, in a table it creates. Your
tables gain no columns; Ptah adds a trigger and an outbox table on the two
source tables, so every write made during the build is caught up.

| Family | Source | Text sent (same recipe as the Go code) | Ptah table |
| --- | --- | --- | --- |
| Documents (`vec` lane) | `documents` | `title + "\n\n" + content` (doc-v1) | `document_vectors` |
| Summaries (`summvec` lane) | `documents` | `title_summary + "\n\n" + bullet_summary` | `document_summary_vectors` |
| Chunks (chunk search, /ask) | `chunk_sparse_context` | `sparse_text` with `context_version = 'v1-full'` | `chunk_context_vectors` |

The chunk row is the part worth a look. The chunk-ctx-v1 embedding input is
built in Go from the parent document's date, title and participants, and
`cmd/sparsectx --recipe=v1-full` already stores exactly that string in
`chunk_sparse_context.sparse_text`. So Ptah reads that table and repeats no
header logic. `TestChunkEmbeddingInputIsTheV1FullSparseText` holds the two
constructions equal.

On the application side, `VECTOR_SOURCE=ptah`:

- search reads which generation is active from Ptah's pointer
  (`ptah_embedding_pointer`), rechecked every 30 seconds, so a cutover or a
  rollback reaches search without a deploy;
- a lane with no generation, or one whose dimension differs from
  `EMBEDDING_DIM`, is turned off instead of failing inside pgvector;
- the chunk lane applies the fingerprint rule the sparse lane uses, so a
  context row written before a rename or a redaction is not used until
  `sparsectx --sweep` rewrites it;
- the write paths (scheduler, summarizer, ingest, notes) stop embedding,
  because `ptah inference catchup` keeps the generations current.

`VECTOR_SOURCE=app`, the default, leaves every query and write path as it is.

## Run the demo

```bash
deploy/ptah/demo/run.sh
```

It needs Docker with compose, Go, curl and python3, and it removes what it
started. It builds your PostgreSQL image, applies `migrations/`, loads a small
Korean corpus (everyone in it is fictional), runs `cmd/sparsectx`, then:

1. builds the three generations with the released `ghcr.io/stokaro/ptah:0.8.0`;
2. writes while they are built — a rename, a new mail with chunks, a summary,
   a delete — and runs `sparsectx --sweep`;
3. catches up, builds the HNSW indexes and verifies;
4. cuts over, each approval bound to the plan digest it was shown;
5. starts the server with `VECTOR_SOURCE=ptah` and asks `/api/v1/search` in
   Korean.

## Switching production

```bash
DB=postgres://...    # the production DATABASE_URL, kept out of shell history

for f in documents summaries chunks; do
  ptah inference prepare  --spec deploy/ptah/specs/$f.bge-m3.yaml --db-url "$DB" --run-id $f-bge-m3
  ptah inference backfill --spec deploy/ptah/specs/$f.bge-m3.yaml --db-url "$DB" --run-id $f-bge-m3
done
```

Change `model.endpoint` in the three files to where Ollama runs. The endpoint
is not part of the generation identity, so the same file works in every
environment. `backfill` resumes from its checkpoint if it stops.

Meanwhile the application runs unchanged on its own columns. When you are
ready:

```bash
go run ./cmd/sparsectx --recipe=v1-full --dry-run=false --sweep
for f in documents summaries chunks; do
  ptah inference catchup --spec deploy/ptah/specs/$f.bge-m3.yaml --db-url "$DB" --run-id $f-bge-m3
  ptah inference index   --spec deploy/ptah/specs/$f.bge-m3.yaml --db-url "$DB" --run-id $f-bge-m3
  ptah inference verify  --spec deploy/ptah/specs/$f.bge-m3.yaml --db-url "$DB" --run-id $f-bge-m3
  ptah inference cutover --spec deploy/ptah/specs/$f.bge-m3.yaml --db-url "$DB" --run-id $f-bge-m3
  # prints the plan digest; approve that exact plan:
  ptah inference cutover --spec deploy/ptah/specs/$f.bge-m3.yaml --db-url "$DB" --run-id $f-bge-m3 \
    --approve <digest> --approver "<name>"
done
```

Then deploy with `VECTOR_SOURCE=ptah`, `EMBEDDING_PROVIDER=local`,
`LOCAL_EMBEDDING_MODEL=bge-m3` and `EMBEDDING_DIM=1024`. From then on, two
jobs keep the vectors current, for example as CronJobs beside the collector:
`sparsectx --recipe=v1-full --dry-run=false --sweep` and
`ptah inference catchup` for the three families. A new chunk has a vector
after both have run.

Going back to the previous model is `VECTOR_SOURCE=app` with the previous
embedding settings. The application's columns were not touched, and the
scheduler's backfill embeds the documents and chunks that arrived in between.
A summary written in between keeps no summary vector, because the summarizer
embeds only when it writes the summary. A later model change
(bge-m3 to something else) is a new specification with a new column, and
then `ptah inference rollback` is available within the window given to
`cutover --stabilize-for`.

## Limits

- Ollama serves a 2048-token context unless `OLLAMA_CONTEXT_LENGTH` says
  otherwise, and Korean text measured about 4.8 bytes per bge-m3 token, so
  the specifications cap an input at 8000 bytes. Raise both together.
- Ptah reads PostgreSQL with pgvector only, which is what second-brain runs.
- The specifications name `schema: public` explicitly. On Ptah 0.8.0 a second
  generation over the same source table without it is refused at `prepare`.
