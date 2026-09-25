---
name: project-feedback-input-limits-286
description: "#286 PR-A: feedback-path input limits, GraphQL createFeedback cap + non-POST mutation 405, OpenSearch error sanitizing/drain, UUID canonicalization (PG rejects urn), api real-DB CI gap"
metadata:
  type: project
---

#286 PR-A (2026-09-25) added feedback-path input validation in internal/api/feedback_input.go (validateFeedback shared by REST and the GraphQL resolver), a GraphQL createFeedback limit of 1 per request, and a non-POST mutation 405 in guardGraphQL. It also cut OpenSearch errors down to status + error.type.

Findings that are hard to see in the code alone:
- graphql-go handler v0.2.4 runs the query-string document for ANY HTTP method (GET/PUT...). That is why the 405 check is `r.Method != POST`, not just GET. A document with several operations and no resolvable operationName is also rejected if it contains any mutation (safe side).
- Alias amplification reproduced: one 64KB body gave 1,000 Record calls and about 20MB of comment, because every alias reuses one variable.
- google/uuid Parse accepts `urn:uuid:`, braces, 32-hex, uppercase; PostgreSQL uuid input rejects only urn (22P02, and the error text echoes the raw value into slog). Rule: never pass a parsed UUID's original string to SQL — pass `id.String()` (canonicalUUID in feedback_input.go).
- pgx rejects an int above INT4 at encode time (client-side error, not 22003); golden rank is capped 0..10000 in the handler.
- OpenSearchClient uses the shared http.DefaultTransport. A connection-reuse test (ConnState counting) was 20/20 flaky when run in parallel with another large-body test; it needs its own Transport and no t.Parallel. Bounded-drain is proven by server-side bytes written, not by connection count.
- CI store-integration runs only ./internal/store/..., so the internal/api `*_RealDB` tests always Skip in CI. A step that runs `-run 'RealDB$' ./internal/api/...` and fails on SKIP was proposed. internal/worker/structural_signals_sql_test.go has the same gap.
- A store golden 100-judgment UpsertJudgments test lives in internal/store, not internal/api. store golden tests TRUNCATE golden tables, which would make a cross-package test flaky.

**Why:** these explain design choices (non-POST scope, where tests live) and verification traps (flaky connection-reuse tests, UUID forms PG rejects).
**How to apply:** for new mutations or feedback-like endpoints, reuse validateFeedback/countGraphQLCost. Related: [[project-search-timeout-input-validation]].
