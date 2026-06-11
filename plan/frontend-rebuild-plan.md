# Frontend Rebuild Plan — second-brain Web UI

> Issue: #113 | Phase 1 Research & Plan
> Agent: fe-vercel-agent | Date: 2026-06-11

---

## 1. Scope & Goals

Rebuild `web/` from scratch using **Next.js 15 App Router + Bun + TypeScript strict**.
Deliver the four core feature areas from issue #113:

| Priority | Feature | API Dependency |
|----------|---------|----------------|
| P0 | Hybrid search + document view | `POST /api/v1/search`, `GET /api/v1/documents/{id}` |
| P1 | Collection status dashboard | `GET /api/v1/stats`, `GET /api/v1/stats/baseline`, `GET /api/v1/documents/recent` |
| P2 | Curation & governance page (#112) | `GET /api/v1/sources`, GraphQL (future: HITL delete API) |

Authentication: `Authorization: Bearer <API_KEY>` on all `/api/v1/*` routes — handled server-side by Next.js route handlers (proxy pattern, same as existing web).

---

## 2. Backend API Contract

All endpoints require `Authorization: Bearer ${API_KEY}` (env var `BRAIN_API_KEY`).
Base URL: `BRAIN_API_URL` (e.g. `http://localhost:8080`).

### 2.1 Search

```
POST /api/v1/search
Body: {
  query: string,               // required
  source_type?: SourceType,    // filter to single source
  exclude_source_types?: SourceType[],
  limit?: number,              // default 10
  sort?: "relevance" | "recent",
  use_hyde?: boolean,          // HyDE query expansion
  use_rerank?: boolean,        // cross-encoder reranking
  curated?: boolean,           // kimi-k2.6 LLM curation
  include_deleted?: boolean
}

Response (curated=false):
{
  results: SearchResult[],
  count: number,
  total: number,
  query: string,
  took_ms: number
}

Response (curated=true):
{
  results: [],
  curated: CuratedResult[],
  count: number,
  query: string,
  curated: true,
  took_ms: number
}

GET /api/v1/search?q=<query>&limit=&source_type=&curated=&use_hyde=&use_rerank=
```

### 2.2 Documents

```
GET /api/v1/documents?source=<type>&exclude_source=<csv>&limit=20&offset=0
Response: { documents: Document[] }

GET /api/v1/documents/recent?kind=<sms|call-recording|voice-memo>&limit=50
Response: { kind: string, count: number, items: RecentItem[] }

GET /api/v1/documents/{uuid}
Response: Document

GET /api/v1/documents/{uuid}/raw   (filesystem only — streams file bytes)
```

### 2.3 Stats & Sources

```
GET /api/v1/stats
Response: { by_source: Record<string, number>, total: number }

GET /api/v1/stats/baseline
Response: BaselineStats (doc counts, content-length percentiles, chunk agg, failure counts, last collection ts per source)

GET /api/v1/sources
Response: { sources: Record<string, number> }
```

### 2.4 GraphQL

```
POST/GET /api/v1/graphql
Schema: Query { search, documents, document, sources, stats, baselineStats }
        Mutation { createFeedback }
```

### 2.5 Source Types (from model)

```typescript
type SourceType =
  | "slack" | "github" | "gdrive" | "notion" | "filesystem"
  | "discord" | "telegram" | "secretary" | "llm-memory"
  | "gmail" | "calendar" | "sms" | "call-log" | "call-transcript" | "upload"
```

> Note: existing `web/src/lib/types.ts` only lists `slack | github | filesystem` — **must update**.

### 2.6 Auth Proxy Pattern (Next.js route handlers)

Client components call `/api/*` (relative). Next.js route handlers add `Authorization: Bearer` and forward to `BRAIN_API_URL`. This keeps `API_KEY` server-side only.

---

## 3. Existing `web/` — Reusable Patterns

The existing web is a minimal Next.js + pnpm + Tailwind v4 app. Safe to delete (git history preserves it).

**Reuse as reference/port:**

| File | Reuse Status | Notes |
|------|-------------|-------|
| `src/lib/api.ts` | Port + extend | Good proxy pattern; add all new endpoints |
| `src/lib/types.ts` | Port + fix | SourceType wrong; add new response types |
| `src/lib/dates.ts` | Reuse as-is | `formatDateTime` utility |
| `src/lib/summary.ts` | Reuse as-is | Content preview extraction |
| `src/lib/docRender.ts` | Reuse | Markdown/code rendering |
| `src/lib/codeWrap.ts` | Reuse | Code block utilities |
| `src/lib/preview.ts` | Reuse | `getExtension`, `rawUrl` helpers |
| `src/app/page.tsx` | Port + enhance | Search UI with filter/sort; add advanced options |
| `src/app/layout.tsx` | Port + extend | Add nav links for Dashboard, Governance |
| `src/app/globals.css` | Port | Tailwind base |

**Do NOT reuse:**
- `package.json` — switch to Bun, add quality tools
- `tsconfig.json` — rebuild with strict
- No existing `eslint.config.mjs` quality (missing Prettier, husky, etc.)

---

## 4. Target Architecture

### 4.1 Directory Structure

```
web/
├── src/
│   ├── app/
│   │   ├── layout.tsx              # Root layout + nav (Search | Dashboard | Governance)
│   │   ├── page.tsx                # / → Search + recent documents
│   │   ├── documents/
│   │   │   └── [id]/
│   │   │       └── page.tsx        # Document detail + transcript view
│   │   ├── dashboard/
│   │   │   └── page.tsx            # Collection status dashboard
│   │   ├── governance/
│   │   │   └── page.tsx            # Curation + governance (HITL, PII, kimi log)
│   │   ├── globals.css
│   │   └── api/                    # Server-side proxy route handlers
│   │       ├── search/route.ts
│   │       ├── documents/route.ts
│   │       ├── documents/[id]/route.ts
│   │       ├── documents/[id]/raw/route.ts
│   │       ├── documents/recent/route.ts
│   │       ├── sources/route.ts
│   │       ├── stats/route.ts
│   │       └── stats/baseline/route.ts
│   ├── components/
│   │   ├── ui/
│   │   │   ├── Badge.tsx           # SourceBadge, MatchTypeBadge, StatusBadge
│   │   │   ├── Button.tsx
│   │   │   ├── Card.tsx
│   │   │   └── Spinner.tsx
│   │   ├── search/
│   │   │   ├── SearchBar.tsx       # Query input + submit
│   │   │   ├── FilterBar.tsx       # Source filter chips + sort select
│   │   │   ├── ResultCard.tsx      # Search result card
│   │   │   └── SearchOptions.tsx   # HyDE / Rerank / Curated toggles
│   │   ├── document/
│   │   │   ├── DocumentMeta.tsx    # source, timestamps, metadata
│   │   │   ├── TranscriptView.tsx  # Speaker-aware transcript (#111)
│   │   │   └── MarkdownContent.tsx # react-markdown with code highlight
│   │   ├── dashboard/
│   │   │   ├── SourceStatsGrid.tsx # Per-source doc counts + sparkline
│   │   │   ├── MobileSyncStatus.tsx # SMS/call/voice-memo recent items
│   │   │   ├── WhisperQueue.tsx    # call-transcript pending count
│   │   │   └── CutoverBanner.tsx   # 2026-05-30 cutover boundary indicator
│   │   └── governance/
│   │       ├── CurationQueue.tsx   # Low-utility docs pending HITL delete approval
│   │       ├── PIIGuardrailStatus.tsx # PII redaction coverage per source
│   │       └── GovernanceLog.tsx   # kimi-k2.6 curation activity log
│   └── lib/
│       ├── api.ts                  # API client (proxy-aware, typed)
│       ├── types.ts                # All TypeScript types (updated SourceType etc.)
│       ├── constants.ts            # SOURCE_LABELS, SOURCE_BADGE_STYLES
│       ├── dates.ts                # Date formatting (port from existing)
│       ├── summary.ts              # Content preview (port from existing)
│       ├── docRender.ts            # Doc rendering (port from existing)
│       ├── preview.ts              # Extension/raw URL helpers
│       └── codeWrap.ts             # Code wrapping
├── package.json                    # Bun + Next.js 15 + quality tools
├── bun.lock
├── tsconfig.json                   # strict: true
├── next.config.ts
├── postcss.config.mjs
├── tailwind.config.ts
├── eslint.config.mjs               # @eslint/js + typescript-eslint + next
├── .prettierrc                     # Prettier config
├── .prettierignore
├── .lintstagedrc.json              # lint-staged: prettier + eslint on staged
├── .husky/
│   └── pre-commit                  # bun lint-staged
├── commitlint.config.ts            # conventional commits
├── .editorconfig
├── .env.example
└── Dockerfile
```

### 4.2 Page Specifications

#### `/` — Search Page
- Search bar (query input)
- Source filter chips (All, SMS, Call, Gmail, Calendar, Drive, …) with counts from `/api/v1/stats`
- Sort: relevance | recent
- Advanced toggles: HyDE, Rerank, Curated (LLM)
- Results: list of `ResultCard` (title, source badge, match type badge, preview, timestamps)
- Empty state: recent documents from `/api/v1/documents`

#### `/documents/[id]` — Document Detail
- Title, source type badge, `occurred_at` / `collected_at` timestamps
- `TranscriptView` for `call-transcript` source: speaker segments `[화자1] ... [화자2] ...` (#111)
- `MarkdownContent` for other sources (react-markdown + code highlight)
- Metadata accordion (source_id, author, channel, etc.)
- Feedback buttons (👍 / 👎 → `POST /api/v1/feedback`)
- Back link

#### `/dashboard` — Collection Status
- Source stats grid: doc count per source (sms, call-log, call-transcript, gmail, calendar, filesystem, upload)
- Mobile push sync panel: recent SMS / call-recording / voice-memo from `/api/v1/documents/recent`
- Whisper queue: call-log count vs call-transcript count → pending transcription estimate
- Cutover boundary: 2026-05-30 marker, counts before/after (from baseline stats)
- Last collected timestamp per source

#### `/governance` — Curation & Governance (#112)
- **Curation Queue (HITL)**: Placeholder UI — low-utility/duplicate documents pending delete approval. Real data requires future backend endpoint (`GET /api/v1/governance/curation-queue`). Show placeholder with explanation.
- **PII Guardrail Status**: Table of source types + PII redaction coverage (sms ✅ OTP hash, call-transcript ⚠️ not yet filtered, gmail TBD, etc.)
- **Knowledge Graph**: Placeholder panel explaining GraphRAG roadmap (#112 #3)
- **kimi-k2.6 Activity Log**: Placeholder (requires future backend curation log endpoint)

> Note: Governance page is largely placeholder in Phase 1 — the core infrastructure (auth, nav, layout) is the deliverable. HITL and graph require backend work tracked in #112.

---

## 5. Quality Tooling Stack

| Tool | Config | Purpose |
|------|--------|---------|
| **Bun** | runtime + pkg manager | replaces Node/pnpm |
| **TypeScript 5.x** | `tsconfig.json` strict | type safety |
| **ESLint 9** | `eslint.config.mjs` (flat config) | linting |
| **eslint-config-next** | via ESLint | Next.js rules |
| **typescript-eslint** | via ESLint | TS-aware rules |
| **Prettier** | `.prettierrc` + tailwind plugin | formatting |
| **lint-staged** | `.lintstagedrc.json` | run prettier+eslint on staged |
| **Husky** | `.husky/pre-commit` | git hook runner |
| **commitlint** | `commitlint.config.ts` | conventional commits |
| **EditorConfig** | `.editorconfig` | cross-editor consistency |

**Scripts (bun run …):**
```json
{
  "dev": "next dev",
  "build": "next build",
  "start": "next start",
  "lint": "eslint src",
  "lint:fix": "eslint src --fix",
  "format": "prettier --write src",
  "type-check": "tsc --noEmit",
  "prepare": "husky"
}
```

---

## 6. Implementation Priority (Task 4)

### Phase A — Foundation (must complete before pages)
1. Delete `web/` directory
2. Scaffold new Next.js 15 with Bun (`bunx create-next-app@latest`)
3. Install + configure quality tools (ESLint, Prettier, lint-staged, husky, commitlint, EditorConfig)
4. Configure tsconfig strict
5. Port lib utilities (api.ts, types.ts with updated SourceType, dates.ts, summary.ts, preview.ts)
6. Create API proxy route handlers
7. Verify: `bun run type-check && bun run lint && bun run build`

### Phase B — Core Pages
8. Root layout + nav (Search | Dashboard | Governance)
9. Search page (port + enhance: add advanced toggles HyDE/Rerank/Curated, expand SourceType support)
10. Document detail page (`/documents/[id]`)
11. Verify build green

### Phase C — New Pages
12. Dashboard page (`/dashboard`)
13. Governance page (`/governance`) — placeholder-heavy but structured
14. Final: `bun run type-check && bun run lint && bun run build`

---

## 7. Key Decisions & Constraints

| Decision | Rationale |
|----------|-----------|
| Bun as runtime AND package manager | User requirement; faster installs, native TS |
| Keep API proxy pattern | `API_KEY` must stay server-side; client never sees token |
| Tailwind CSS v4 | Existing pattern; v4 uses `@import "tailwindcss"` in globals.css |
| No GraphQL client lib | Governance page uses REST for now; GraphQL schema is available if needed later |
| Governance as placeholder | HITL delete and curation APIs don't exist yet in backend (#112 not implemented) |
| Speaker transcript | Parse `[화자1]`/`[화자2]` prefix in content field for `call-transcript` source |

---

## 8. Open Questions for team-lead

1. **Governance backend**: Is there any existing endpoint for the HITL curation queue, or is `governance/` purely placeholder UI for now?
2. **Knowledge Graph data**: Any graph/entity extraction data in the DB yet, or full placeholder?
3. **`/api/v1/documents/recent` — `RecentItem` shape**: Need to check `store.RecentItem` struct to type correctly. (Need to read `internal/store/` if dashboard is P0.)
4. **Cutover date constant**: `2026-05-30` confirmed as the cutover boundary shown in dashboard?

---

## 9. Files to Delete

```
web/   (entire directory — git history preserves content)
```
