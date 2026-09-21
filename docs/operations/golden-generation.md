# Replenishing golden-set questions

Generation is an explicit user action (`POST /api/v1/golden/queries/generate`).
Page loads, next-question requests, feedback and judgments never create questions.

The generator first inserts unused historical questions and fixed seeds. It now
streams history until 50 unused questions are found, so the newest 200 already
reviewed questions cannot hide older unused ones. If no questions were added and
none remain open, a single bounded LLM call proposes up to 10 new questions from
20 source-balanced source documents. Sampling varies on each button click to
avoid repeatedly selecting an exhausted group of documents. Existing question
texts (up to 100) are supplied to discourage repetition.

Corpus sources are active, explicitly `retention=keep`, occurred within the last
14 days, and have useful content. Calls require completed transcripts. A document
already used to generate a question is excluded regardless of whether that query
is open, completed or skipped. New questions have `source=document` and a nullable
`source_document_id` provenance link (migration 036). Neither link nor LLM output
is a relevance label: the user still reviews search results normally.

Strict JSON, source-ID allowlists, literal evidence and length checks reject
unverifiable candidates. Valid siblings survive an invalid candidate; malformed
batches fail with a retryable error. Inserts serialize with an advisory transaction
lock and deduplicate against all questions, preserving existing statuses and
judgments. The API has an overall 55-second generation deadline; the WebUI proxy
allows 75 seconds. No private source/provider response bodies are logged.

If no eligible new evidence exists, the UI reports that no further questions can
be created until new documents/questions arrive. It never reopens completed work
or manufactures answer labels. Monitoring can use counts only:

```sql
SELECT source, status, count(*) FROM golden_queries GROUP BY source, status;
SELECT count(*) FROM golden_queries WHERE source='document' AND source_document_id IS NOT NULL;
```
