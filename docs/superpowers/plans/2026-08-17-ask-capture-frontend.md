# Ask & Capture Frontend Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the Ask and Capture user-facing screens to `web/` (Next.js), replace the app's existing "Claude Aesthetic" warm-cream Tailwind theme with a linear.app-derived dark-first token set, and wire the two new screens to the Go API server through session-protected Next.js route handlers — including the SSE pass-through required for streamed Ask answers.

**Architecture:** `web/` remains an auth proxy in front of `cmd/server` — no generation/search/storage logic lives in TypeScript (spec §4). New route handlers under `web/src/app/api/notes/*` and `web/src/app/api/ask` forward requests to `BRAIN_API_URL` with a server-held `Bearer ${API_KEY}`, exactly like the existing `web/src/app/api/search/route.ts` and `web/src/app/api/documents/route.ts`. These new handlers deliberately live under `/api/*`, not `/api/v1/*` — `web/src/proxy.ts` (Next.js Edge Middleware) excludes `/api/v1/*` from its OAuth session gate because that prefix is reserved for the mobile app's own Bearer `API_KEY` passthrough (`web/src/app/api/v1/ingest/*`). Placing session-consuming browser proxies there would silently disable auth on them.

**Tech Stack:** Next.js 16.2.3, React 19.2.5, Tailwind CSS 4 (CSS-first `@theme` config in `web/src/app/globals.css`, confirmed — no `tailwind.config.js` exists in this repo), next-auth v5 (`web/src/auth.ts`), no test runner currently installed (`web/package.json` has no `test` script and no `vitest`/`jest`/`@testing-library/*` dependency — confirmed by reading the file).

**Spec:** `docs/superpowers/specs/2026-08-17-ask-capture-design.md` (as amended 2026-08-17, §7)

**Design source:** `web/DESIGN.md`, vendored from `https://github.com/VoltAgent/awesome-design-md` (`design-md/linear.app/DESIGN.md`).

---

## Global Constraints

- **External precondition — this plan does not vendor `web/DESIGN.md` itself.** That file must exist at `web/DESIGN.md` before Task 1 begins. It is fetched with `gh api repos/VoltAgent/awesome-design-md/contents/design-md/linear.app/DESIGN.md --jq '.content' | base64 -d > web/DESIGN.md` (plus `preview.html`/`preview-dark.html` if present in that directory) by whichever execution environment has `gh` + network access, with the header comment specified in Task 1 Step 0. Task 1's token derivation below does not depend on reading that file's exact contents — the color/surface/accent decisions are taken directly from the design brief (near-black `#010102` canvas, `#5e6ad2` accent, charcoal surfaces, hairline borders) and the type scale is independently re-derived for application density (never copied from a marketing site's display scale — this is a hard requirement, not a preference, per the caveat mandated for the vendored file's header).
- **No Go files are touched anywhere in this plan.** Another implementer is concurrently working in `internal/worker/` on the same branch (`feature/capture-backend`) — nothing here overlaps that directory or any `.go` file.
- **No new component library.** `web/src/components/ui/` keeps its existing four components (`Card`, `Badge`, `Button`, `Spinner`) plus whatever small additions this plan needs; no Cloudscape, no shadcn, no headless-UI package is added (spec §7.1, 2026-08-17 amendment).
- **Dark-only, no light/dark toggle.** The current theme uses `prefers-color-scheme: dark` to swap CSS custom properties. This plan removes that branch entirely and makes the near-black palette the sole theme — this is a personal, single-user, primarily-nighttime tool (per the design brief), and maintaining two palettes doubles the token surface for no user this app has. This is a design decision made in Task 1, not implied by the design brief alone, and is called out there.
- **Accessibility vs. literal Linear tones — flagged, not silently resolved.** A literal copy of Linear's own muted-gray tertiary text token fails WCAG AA (4.5:1) for small text on `#010102` (measured ≈3.99:1 for `#6b6c76`). Task 1 lightens that one token to ≈5.16:1 (`#7d7e88`) and documents why. The accent `#5e6ad2` itself measures ≈4.43:1 on canvas — adequate for buttons/large text/UI components (3:1 threshold) but not guaranteed-safe as small standalone body text; Task 1 restricts its use accordingly.
- **No test runner exists in `web/` today.** This plan adds Vitest (Task 3) scoped narrowly to pure functions in `web/src/lib/` that have logic worth pinning with a test — the enrichment-status→badge mapping and the SSE event parser. Page components (`capture/page.tsx`, `ask/page.tsx`) and the Next.js route handlers are **not** unit tested in this plan; they are verified manually (Task 10), the same way this codebase already verifies mobile layouts and SSE token timing (spec §11.3) and the same way the backend plan (`docs/superpowers/plans/2026-08-17-capture-backend.md`) declines to add DB-integration tests for `DocumentStore` CRUD methods. Inventing a component-test suite for a single-maintainer personal tool that has never had one is not in scope.
- **Ask-path tasks (Task 8, Task 9) are blocked and sequenced last.** `POST /api/v1/ask` does not exist yet — it is explicitly a separate, later backend plan (spec §5, §14 step 4). Task 7 (the SSE parser) can be built and unit-tested now against the documented event contract (spec §5.1) without a live backend. Task 8 (the `/api/ask` pass-through route) and Task 9 (the Ask page) are written to that same contract but cannot be exercised end-to-end until the Ask backend plan ships. Do not implement Task 8/9 before Task 1–7 are done and merged, even though nothing here technically prevents it — the point of the ordering is that Capture ships and is usable on its own first.
- **Capture's backend is being implemented concurrently, not yet merged.** `POST /api/v1/notes`, `POST /api/v1/notes/{id}/retry-enrichment`, `DELETE /api/v1/notes/{id}` are the subject of a separate, concurrent implementation (`docs/superpowers/plans/2026-08-17-capture-backend.md`, being executed in `internal/worker/` right now per the coordinating session). Tasks 5–6 here can be implemented and unit-tested against that plan's documented contracts without waiting for it to merge, but cannot be manually end-to-end verified (Task 10) until it does.
- **No "list notes" backend endpoint is needed.** The Capture screen's recent-notes list reuses the existing generic `GET /api/v1/documents?source=note` path, already proxied at `web/src/app/api/documents/route.ts` and already wrapped by `listDocuments({ source, limit })` in `web/src/lib/api.ts` (confirmed by reading both files — the existing search page's "최근 추가된 문서" panel already calls this exact function with a `source` filter). No new backend surface is invented for this.
- **A pre-existing, unrelated dark-mode gap is fixed in passing, not left to compound.** `web/src/app/globals.css` imports `highlight.js/styles/github.css` — a *light* code-highlighting theme — used by `documents/[id]`'s markdown renderer. Before this plan, that mismatch already affected any OS set to dark mode; after this plan removes the light palette entirely, it would affect *all* users, not just dark-mode ones. Task 1 swaps this one import for `github-dark.css` since it is a one-line fix directly caused by this plan's own dark-only decision. Re-theming `documents/[id]`'s markdown rendering beyond that one import is out of scope (spec §2 비목표).
- Korean UI copy, English code identifiers, comments, and commit messages (project convention, confirmed throughout `web/src/**`).
- Streamed Ask answers are rendered as plain text (`white-space: pre-wrap`), not parsed as markdown. The spec does not require markdown rendering for `/ask` answers, and parsing streamed, incrementally-arriving text as markdown/HTML introduces unnecessary XSS surface for no stated requirement. If a future spec revision requires markdown answers, that is a new task, not an implicit extension of this one.

---

## File Structure

| File | Action | Responsibility |
|------|--------|-----------------|
| `web/DESIGN.md` | External precondition (not created by this plan) | Vendored linear.app token spec — see Global Constraints |
| `web/src/app/globals.css` | Modify | Replace `@theme`/`@theme inline`/`:root` with dark-only linear.app tokens; swap highlight.js theme; add `.badge-note`/`.badge-insight` |
| `web/src/app/layout.tsx` | Modify | Task 1: drop serif font loading. Task 4: add Ask/Capture nav links |
| `web/src/lib/types.ts` | Modify | Add `"note"`/`"insight"` to `SourceType`, `NoteMetadata`, `getNoteMetadata()`, `CreateNoteResponse`, `AskSourceItem`, `AskStreamEvent` |
| `web/src/lib/constants.ts` | Modify | Add labels/badge classes for `note`/`insight`; add `AskLayer`, `ASK_LAYER_LABELS`, `getAskLayer()` |
| `web/vitest.config.ts` | Create | Minimal Vitest config, scoped to `web/src/lib/**/*.test.ts` |
| `web/package.json` | Modify | Add `vitest` devDependency + `test`/`test:watch` scripts |
| `web/src/lib/enrichmentStatus.ts` | Create | Pure function mapping `enrichment_status` → badge label/variant/retryable |
| `web/src/lib/enrichmentStatus.test.ts` | Create | Unit tests for the above |
| `web/src/proxy.ts` | Modify | Update the route-protection doc comment to list `/ask`, `/capture`, `/api/ask`, `/api/notes/*` |
| `web/src/lib/api.ts` | Modify | Add `createNote`, `retryNoteEnrichment`, `deleteNote` (Task 6) |
| `web/src/app/api/notes/route.ts` | Create | `POST` proxy → `POST /api/v1/notes` |
| `web/src/app/api/notes/[id]/route.ts` | Create | `DELETE` proxy → `DELETE /api/v1/notes/{id}` |
| `web/src/app/api/notes/[id]/retry-enrichment/route.ts` | Create | `POST` proxy → `POST /api/v1/notes/{id}/retry-enrichment` |
| `web/src/app/capture/page.tsx` | Create | Capture screen: compose field, recent-notes list, retry/delete actions, mobile layout |
| `web/src/lib/sseEvents.ts` | Create | `splitSSEBuffer` (chunk buffering) + `parseAskEvent` (typed event mapping) |
| `web/src/lib/sseEvents.test.ts` | Create | Unit tests covering mid-event chunk splits, all 4 event types, malformed payloads |
| `web/src/app/api/ask/route.ts` | Create — **BLOCKED, sequenced last** | SSE pass-through proxy → `POST /api/v1/ask` |
| `web/src/app/ask/page.tsx` | Create — **BLOCKED, sequenced last** | Ask screen: question input, streamed answer, layered source cards, mobile fixed input bar |

---

## Task 1: Dark-only linear.app design tokens

### Files
- Modify: `web/src/app/globals.css` (full `@theme`/`@theme inline`/`:root` rewrite)
- Modify: `web/src/app/layout.tsx:1-30` (drop Lora import/variable)

### Interfaces
Produces (consumed by every subsequent task's Tailwind class names):
- Semantic aliases: `bg-background`, `bg-surface`, `bg-surface-subtle`, `bg-surface-overlay`, `text-foreground`, `text-foreground-muted`, `text-foreground-subtle`, `border-border`, `border-border-strong`, `bg-accent`/`text-accent`/`bg-accent-hover`/`bg-accent-subtle`, `text-danger`, `text-success`, `text-warning` — all unchanged **names**, new values, so no consuming file outside this task needs to change its class names.
- New type-scale tokens `--text-md` (15px) sitting between the existing `--text-sm`/`--text-lg`.

### Steps

- [ ] **Step 0 (external, not run by this task):** Confirm `web/DESIGN.md` exists and its header comment states the source URL, vendor date (2026-08-17), and the marketing-vs-application type-scale caveat, per Global Constraints. If it does not exist yet, stop here and vendor it first — see Global Constraints for the exact command. Everything else in this task proceeds independently of that file's exact byte content.

- [ ] Replace the entire `@theme { ... }` and `@theme inline { ... }` blocks in `web/src/app/globals.css` (currently lines 13–114) with:

```css
@theme {
  /* ── Font families ─────────────────────────────────────────────────────
     Serif dropped (2026-08-17 token revision): a serif display face fights
     the "dense and technical" linear.app identity this app now uses.
     DM Sans/JetBrains Mono are already loaded via next/font in layout.tsx —
     reusing them avoids adding a new font. */
  --font-sans: "DM Sans", system-ui, -apple-system, sans-serif;
  --font-mono: "JetBrains Mono", "Courier New", monospace;

  /* ── Type scale — re-derived for application density ─────────────────
     Deliberately NOT copied from web/DESIGN.md's marketing display scale
     (e.g. an 80px hero size makes no sense in a dense personal-tool UI).
     Linear's own product UI runs body text around 13px; this scale mirrors
     that instead of the previous warm-cream theme's 16px/40px range. */
  --text-xs: 0.75rem;     /* 12px — timestamps, badges, metadata */
  --text-sm: 0.8125rem;   /* 13px — secondary text, dense list rows */
  --text-base: 0.875rem;  /* 14px — primary body text (app default) */
  --text-md: 0.9375rem;   /* 15px — emphasized body, form labels */
  --text-lg: 1.0625rem;   /* 17px — section headers, card titles */
  --text-xl: 1.25rem;     /* 20px — page titles */
  --text-2xl: 1.5rem;     /* 24px — rare, top-level only */

  /* ── Border radius — tighter than the previous warm-cream theme ──────── */
  --radius-sm: 0.25rem;
  --radius-md: 0.375rem;
  --radius-lg: 0.5rem;
  --radius-xl: 0.625rem;
  --radius-full: 9999px;

  /* ── Raw palette — near-black canvas + charcoal surfaces (linear.app) ── */
  --color-canvas: #010102;
  --color-charcoal-950: #060607; /* sunken / inset */
  --color-charcoal-900: #0a0a0c;
  --color-charcoal-800: #131316; /* default raised surface (cards) */
  --color-charcoal-700: #1c1c20; /* overlay (dropdowns, modals) */
  --color-charcoal-600: #232329; /* hairline border */
  --color-charcoal-500: #35353d; /* strong border (focus-adjacent) */

  --color-ink-50: #f2f3f5;  /* primary text — off-white, not pure white */
  --color-ink-400: #9a9ba6; /* secondary text — measured ≈7.55:1 on canvas */
  --color-ink-600: #7d7e88; /* tertiary/subtle text — see accessibility note */
  --color-ink-800: #4a4b53; /* disabled */

  --color-accent-raw: #5e6ad2;         /* single accent — see design brief */
  --color-accent-raw-hover: #6e79dd;
  --color-accent-raw-subtle: #1a1c2e;  /* tinted panel bg for accent contexts */

  --color-status-success: #4cc38a;
  --color-status-warning: #e2b93b;
  --color-status-danger: #eb5757;
  --color-status-success-subtle: #12261d;
  --color-status-warning-subtle: #2b2412;
  --color-status-danger-subtle: #2b1414;
}

/*
  @theme inline: semantic aliases, unchanged names from the previous theme so
  every existing class name (bg-background, text-foreground-muted, etc.)
  keeps working across the whole app — only the values change.
*/
@theme inline {
  --color-background: var(--color-canvas);
  --color-surface: var(--color-charcoal-800);
  --color-surface-subtle: var(--color-charcoal-950);
  --color-surface-overlay: var(--color-charcoal-700);

  --color-foreground: var(--color-ink-50);
  --color-foreground-muted: var(--color-ink-400);
  /* --color-foreground-subtle intentionally lighter than Linear's own
     tertiary gray (~#6b6c76, ≈3.99:1 on #010102 — fails WCAG AA 4.5:1 for
     small text). This app uses foreground-subtle for timestamps and badge
     text, which IS small text, so the literal Linear tone is not safe here.
     #7d7e88 measures ≈5.16:1 — passes AA. This is a deliberate deviation
     from a literal copy of the source tone, not an oversight. */
  --color-foreground-subtle: var(--color-ink-600);

  --color-border: var(--color-charcoal-600);
  --color-border-strong: var(--color-charcoal-500);

  --color-accent: var(--color-accent-raw);
  --color-accent-hover: var(--color-accent-raw-hover);
  --color-accent-subtle: var(--color-accent-raw-subtle);

  --color-danger: var(--color-status-danger);
  --color-success: var(--color-status-success);
  --color-warning: var(--color-status-warning);
  --color-disabled: var(--color-ink-800);
}
```

- [ ] Replace the `@layer base { :root { ... } @media (prefers-color-scheme: dark) { ... } ... } }` block (currently lines 116–270) — remove the light-mode default values and the `@media (prefers-color-scheme: dark)` override block entirely, folding the (former) dark values in as the only `:root` values, and change heading font-family references from `var(--font-serif)` to `var(--font-sans)`:

```css
@layer base {
  :root {
    color-scheme: dark; /* single theme — see Global Constraints */

    --status-success: var(--color-status-success);
    --status-success-light: var(--color-status-success-subtle);
    --status-warning: var(--color-status-warning);
    --status-warning-light: var(--color-status-warning-subtle);
    --status-danger: var(--color-status-danger);
    --status-danger-light: var(--color-status-danger-subtle);

    --shadow-sm: 0 1px 2px oklch(0% 0 0 / 0.25);
    --shadow-md: 0 2px 8px oklch(0% 0 0 / 0.3), 0 1px 2px oklch(0% 0 0 / 0.2);
    --shadow-lg: 0 8px 24px oklch(0% 0 0 / 0.4), 0 2px 6px oklch(0% 0 0 / 0.25);
    --shadow-xl: 0 20px 48px oklch(0% 0 0 / 0.55), 0 6px 16px oklch(0% 0 0 / 0.35);
    --shadow-inset: inset 0 1px 3px oklch(0% 0 0 / 0.35);
  }

  @media (prefers-reduced-motion: reduce) {
    :root {
      --duration-instant: 0.01ms;
      --duration-fast: 0.01ms;
      --duration-normal: 0.01ms;
      --duration-slow: 0.01ms;
      --duration-deliberate: 0.01ms;
    }
  }

  html {
    font-family: var(--font-sans);
    font-size: 1rem;
    line-height: 1.6;
    -webkit-font-smoothing: antialiased;
    -moz-osx-font-smoothing: grayscale;
    background-color: var(--color-canvas);
    color: var(--color-ink-50);
  }

  h1, h2, h3, h4, h5, h6 {
    font-family: var(--font-sans);
    color: var(--color-ink-50);
  }

  h1 { font-size: var(--text-2xl); font-weight: 600; line-height: 1.25; letter-spacing: -0.02em; }
  h2 { font-size: var(--text-xl);  font-weight: 600; line-height: 1.3;  letter-spacing: -0.01em; }
  h3 { font-size: var(--text-lg);  font-weight: 500; line-height: 1.35; }
  h4 { font-size: var(--text-md);  font-weight: 500; line-height: 1.4; }
  h5 {
    font-size: var(--text-sm); font-weight: 600; line-height: 1.5;
    letter-spacing: 0.04em; text-transform: uppercase; color: var(--color-ink-400);
  }
  h6 {
    font-size: var(--text-xs); font-weight: 600; line-height: 1.5;
    letter-spacing: 0.06em; text-transform: uppercase; color: var(--color-ink-600);
  }

  code, pre, kbd, samp { font-family: var(--font-mono); font-variant-ligatures: none; }
  td, .tabular { font-variant-numeric: tabular-nums; }
}
```

- [ ] Swap the light code-highlighting theme for a dark one, in the import block at the top of the file (line 3):

```css
@import "highlight.js/styles/github-dark.css";
```

  (Rationale in Global Constraints: this import already mismatched dark-mode OS users before this plan; forcing the whole app dark makes it mismatch for everyone unless fixed. `highlight.js` ships `github-dark.css` alongside `github.css` — no new dependency.)

- [ ] Collapse the `.badge-*` classes in `@layer components` (currently lines 272–439) to a single set of values (the former "dark" variants — since the app is dark-only, drop the light variants and the `@media (prefers-color-scheme: dark)` wrapper around the dark set). Also update `.skeleton`'s gradient stops from `--color-warm-*` (removed token) to the new charcoal scale:

```css
@layer components {
  .skeleton {
    background: linear-gradient(
      90deg,
      var(--color-charcoal-800) 0%,
      var(--color-charcoal-700) 50%,
      var(--color-charcoal-800) 100%
    );
    background-size: 200% 100%;
    border-radius: var(--radius-md);
    animation: skeleton-shimmer 1.5s ease-in-out infinite;
  }
  @keyframes skeleton-shimmer {
    0% { background-position: 200% 0; }
    100% { background-position: -200% 0; }
  }

  .badge-sms { background: oklch(20% 0.07 145); color: oklch(62% 0.15 145); }
  .badge-call-log { background: oklch(19% 0.06 250); color: oklch(62% 0.13 250); }
  .badge-call-transcript { background: oklch(18% 0.06 240); color: oklch(60% 0.12 240); }
  .badge-gmail { background: oklch(22% 0.07 25); color: oklch(62% 0.17 25); }
  .badge-calendar { background: oklch(19% 0.06 200); color: oklch(60% 0.12 200); }
  .badge-filesystem { background: oklch(20% 0.05 80); color: oklch(65% 0.13 80); }
  .badge-upload { background: oklch(20% 0.07 310); color: oklch(62% 0.13 310); }
  .badge-voice-memo { background: oklch(20% 0.06 30); color: oklch(64% 0.15 30); }
  .badge-slack { background: oklch(21% 0.07 310); color: oklch(62% 0.14 310); }
  .badge-github { background: oklch(18% 0.02 50); color: oklch(72% 0.02 50); }
  .badge-llm-memory { background: oklch(20% 0.05 200); color: oklch(60% 0.11 200); }
  .badge-secretary { background: oklch(20% 0.07 260); color: oklch(62% 0.13 260); }
  .badge-gdrive { background: oklch(20% 0.05 80); color: oklch(65% 0.13 80); }
  .badge-notion { background: oklch(18% 0.02 50); color: oklch(70% 0.02 50); }
  .badge-telegram { background: oklch(19% 0.06 215); color: oklch(60% 0.11 215); }
  .badge-discord { background: oklch(20% 0.07 275); color: oklch(62% 0.13 275); }
}
```

  (`.badge-note`/`.badge-insight` are added in Task 2, alongside the `SourceType` extension they belong to — not here, to keep this task scoped to the base theme swap.)

- [ ] In `web/src/app/layout.tsx`, remove the `Lora` import and the `lora` font object (lines 3, 10–16), and remove `lora.variable` from the `className` on `<html>` (line 45):

```tsx
import type { Metadata } from "next";
import Link from "next/link";
import { DM_Sans, JetBrains_Mono } from "next/font/google";
import "./globals.css";

const dmSans = DM_Sans({
  subsets: ["latin"],
  weight: ["300", "400", "500", "600"],
  variable: "--font-sans",
  display: "swap",
});

const jetbrainsMono = JetBrains_Mono({
  subsets: ["latin"],
  weight: ["400", "500"],
  variable: "--font-mono",
  display: "swap",
});
```

  and:

```tsx
      className={`${dmSans.variable} ${jetbrainsMono.variable}`}
```

- [ ] Run `cd web && npm run type-check` and confirm it passes (this catches any remaining `--font-serif`/`lora` reference left dangling).

- [ ] Run `cd web && npm run build` and confirm it compiles cleanly — this is the closest thing to a regression check available without a running dev server, since there is no visual test.

- [ ] Manually load `/` (search page), `/dashboard`, `/documents/[id]` in a browser (`npm run dev`) and confirm: near-black background, readable text, source badges still legible, no console errors about missing CSS variables. This is a **required manual check**, not optional — the theme swap touches every page in the app, not just Ask/Capture.

- [ ] Commit: `git add web/src/app/globals.css web/src/app/layout.tsx && git commit -m "feat(web): replace warm-cream theme with dark-only linear.app tokens"`

---

## Task 2: Extend `SourceType` for `note`/`insight`

### Files
- Modify: `web/src/lib/types.ts:5-20` (`SourceType` union), append new interfaces
- Modify: `web/src/lib/constants.ts` (labels, badge classes, Ask-layer mapping)
- Modify: `web/src/app/globals.css` (`@layer components`, append `.badge-note`/`.badge-insight`)

### Interfaces
Consumes:
- `internal/model/document.go`'s `SourceNote`/`SourceInsight` constants (spec §3.3) — read-only reference, no Go file touched.

Produces (consumed by Task 6 Capture screen, Task 9 Ask screen):
- `SourceType` gains `"note" | "insight"`.
- `export interface NoteMetadata { enrichment_status?: "pending" | "done" | "failed"; enrichment_attempts?: number; enrichment_last_error?: string | null; [key: string]: unknown }`
- `export function getNoteMetadata(doc: DocumentDetail): NoteMetadata`
- `export interface CreateNoteResponse { id: string; status: "pending" }`
- `export interface AskSourceItem { id: string; title: string; source_type: SourceType; score: number }`
- `export type AskStreamEvent = | { type: "sources"; sources: AskSourceItem[] } | { type: "token"; text: string } | { type: "done"; finish_reason: "stop" | "error" | "no_evidence" } | { type: "error"; message: string }`
- `export type AskLayer = "note" | "observed" | "insight"`
- `export const ASK_LAYER_LABELS: Record<AskLayer, string>`
- `export function getAskLayer(sourceType: SourceType): AskLayer`

### Steps

- [ ] In `web/src/lib/types.ts`, extend the `SourceType` union (lines 5–20):

```ts
export type SourceType =
  | "slack"
  | "github"
  | "gdrive"
  | "notion"
  | "filesystem"
  | "discord"
  | "telegram"
  | "secretary"
  | "llm-memory"
  | "gmail"
  | "calendar"
  | "sms"
  | "call-log"
  | "call-transcript"
  | "upload"
  | "note"
  | "insight";
```

- [ ] Append to `web/src/lib/types.ts` (after the existing `DocumentDetail` interface):

```ts
/**
 * Metadata shape specific to source_type=note documents (spec §6.1).
 * Deliberately not extending DocumentMetadata — its index signature
 * (string | number | boolean | undefined) cannot accommodate
 * enrichment_last_error's `string | null`, and note metadata does not
 * overlap with DocumentMetadata's other fields (channel, repo, folder, ...).
 */
export interface NoteMetadata {
  enrichment_status?: "pending" | "done" | "failed";
  enrichment_attempts?: number;
  enrichment_last_error?: string | null;
  [key: string]: unknown;
}

/** Reads a DocumentDetail's metadata as NoteMetadata in one place, so call
 * sites never need an inline `as NoteMetadata` cast (spec §6.1). */
export function getNoteMetadata(doc: DocumentDetail): NoteMetadata {
  return (doc.metadata ?? {}) as NoteMetadata;
}

/** Response shape of POST /api/v1/notes (spec §6.1) — the note document
 * itself is not returned; the caller already has the content it just sent. */
export interface CreateNoteResponse {
  id: string;
  status: "pending";
}

/** One entry of the `sources` SSE event (spec §5.1). */
export interface AskSourceItem {
  id: string;
  title: string;
  source_type: SourceType;
  score: number;
}

/** Parsed, typed form of the four /api/v1/ask SSE event types (spec §5.1). */
export type AskStreamEvent =
  | { type: "sources"; sources: AskSourceItem[] }
  | { type: "token"; text: string }
  | { type: "done"; finish_reason: "stop" | "error" | "no_evidence" }
  | { type: "error"; message: string };
```

- [ ] In `web/src/lib/constants.ts`, add to `SOURCE_LABELS` (after `upload: "Upload",`):

```ts
  note: "내 노트",
  insight: "추론",
```

- [ ] Add to `SOURCE_BADGE_CLASSES` (after `discord: "badge-discord",`):

```ts
  note: "badge-note",
  insight: "badge-insight",
```

- [ ] Append to `web/src/lib/constants.ts` — the Ask-screen source-layer classifier (spec §5.2, §7.2: search results split into 관찰된 사실/추론 lanes, plus a third "내 노트" case for the user's own raw notes surfacing as evidence):

```ts
import type { AskLayer } from "./types";

/** Which of the three Ask-answer lanes (spec §5.2, §7.2) a source belongs
 * to: the user's own raw notes, everything else observed/collected, or
 * LLM-derived insight documents (always the most visually distinct — they
 * are inferences, not facts, per spec §3.2's echo-chamber guards). */
export const ASK_LAYER_LABELS: Record<AskLayer, string> = {
  note: "내 노트",
  observed: "관찰 데이터",
  insight: "추론",
};

export function getAskLayer(sourceType: SourceType): AskLayer {
  if (sourceType === "note") return "note";
  if (sourceType === "insight") return "insight";
  return "observed";
}
```

  (Note: `AskLayer` must also be exported from `web/src/lib/types.ts` — add `export type AskLayer = "note" | "observed" | "insight";` next to `AskStreamEvent` in the previous step, and import it into `constants.ts` as shown.)

- [ ] Append to `web/src/app/globals.css`'s `@layer components` block (after `.badge-discord`):

```css
  /* note: muted indigo-charcoal — echoes the accent hue without competing
     with it, since note badges appear constantly (every Capture list row)
     while the accent itself is reserved for interactive elements. */
  .badge-note {
    background: oklch(20% 0.05 265);
    color: oklch(78% 0.03 265);
  }
  /* insight: reuses the accent tokens directly. This app has exactly one
     accent (design brief: "single lavender-blue accent") — marking
     insight/inference content with that same, otherwise-unused-for-badges
     color makes it visually distinct without introducing a second hue,
     and doubles as a constant visual reminder that insights are inferences,
     not facts (spec §3.2 echo-chamber guard 2). */
  .badge-insight {
    background: var(--color-accent-subtle);
    color: var(--color-accent);
  }
```

- [ ] Run `cd web && npm run type-check` and confirm it passes.

- [ ] Commit: `git add web/src/lib/types.ts web/src/lib/constants.ts web/src/app/globals.css && git commit -m "feat(web): add note/insight source types, labels, and Ask-layer classification"`

---

## Task 3: Testing foundation (Vitest) + enrichment-status mapper

### Files
- Create: `web/vitest.config.ts`
- Modify: `web/package.json` (add `vitest` devDependency, `test`/`test:watch` scripts)
- Create: `web/src/lib/enrichmentStatus.ts`
- Create: `web/src/lib/enrichmentStatus.test.ts`

### Interfaces
Produces (consumed by Task 6 Capture screen):
- `export type EnrichmentStatus = "pending" | "done" | "failed"`
- `export interface EnrichmentBadge { label: string; variant: "success" | "warning" | "danger"; retryable: boolean }`
- `export function describeEnrichmentStatus(status: EnrichmentStatus | undefined): EnrichmentBadge`

### Steps

- [ ] Add `vitest` to `web/package.json` devDependencies and two scripts:

```json
    "test": "vitest run",
    "test:watch": "vitest",
```

```json
    "vitest": "^3.0.0",
```

  (Only `vitest` is added — no `@testing-library/react`, no `jsdom`. This plan's test scope (Global Constraints) is pure `web/src/lib/**` functions, which run fine under Vitest's default Node environment; a DOM environment is not needed until/unless component tests are added, which this plan deliberately does not do.)

- [ ] Create `web/vitest.config.ts`:

```ts
import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    include: ["src/lib/**/*.test.ts"],
    environment: "node",
  },
});
```

- [ ] Write the failing test file `web/src/lib/enrichmentStatus.test.ts`:

```ts
import { describe, it, expect } from "vitest";
import { describeEnrichmentStatus } from "./enrichmentStatus";

describe("describeEnrichmentStatus", () => {
  it("maps pending to a non-retryable warning badge", () => {
    const badge = describeEnrichmentStatus("pending");
    expect(badge).toEqual({ label: "정리 중", variant: "warning", retryable: false });
  });

  it("maps done to a non-retryable success badge", () => {
    const badge = describeEnrichmentStatus("done");
    expect(badge).toEqual({ label: "정리 완료", variant: "success", retryable: false });
  });

  it("maps failed to a retryable danger badge", () => {
    const badge = describeEnrichmentStatus("failed");
    expect(badge.variant).toBe("danger");
    expect(badge.retryable).toBe(true);
  });

  it("treats undefined status as pending (a note whose worker hasn't ticked yet)", () => {
    const badge = describeEnrichmentStatus(undefined);
    expect(badge.variant).toBe("warning");
    expect(badge.retryable).toBe(false);
  });
});
```

- [ ] Run `cd web && npx vitest run src/lib/enrichmentStatus.test.ts` and confirm it fails to compile (`Cannot find module './enrichmentStatus'`).

- [ ] Create `web/src/lib/enrichmentStatus.ts`:

```ts
export type EnrichmentStatus = "pending" | "done" | "failed";

export interface EnrichmentBadge {
  label: string;
  variant: "success" | "warning" | "danger";
  retryable: boolean;
}

const STATUS_MAP: Record<EnrichmentStatus, EnrichmentBadge> = {
  pending: { label: "정리 중", variant: "warning", retryable: false },
  done: { label: "정리 완료", variant: "success", retryable: false },
  // Only the terminal failed state (3 attempts exhausted, spec §6.3) is
  // retryable — a note mid-retry is still "pending", not "failed".
  failed: { label: "정리 실패", variant: "danger", retryable: true },
};

/** A note with no enrichment_status yet (worker hasn't ticked, spec §6.1's
 * 202 response predates the first worker pass) reads the same as pending. */
export function describeEnrichmentStatus(status: EnrichmentStatus | undefined): EnrichmentBadge {
  return STATUS_MAP[status ?? "pending"];
}
```

- [ ] Run `cd web && npx vitest run src/lib/enrichmentStatus.test.ts` and confirm all 4 tests pass.

- [ ] Commit: `git add web/vitest.config.ts web/package.json web/src/lib/enrichmentStatus.ts web/src/lib/enrichmentStatus.test.ts && git commit -m "test(web): add Vitest + enrichment-status badge mapper"`

---

## Task 4: App shell — nav links + proxy doc update

### Files
- Modify: `web/src/app/layout.tsx:58-77` (`<nav>` block)
- Modify: `web/src/proxy.ts:1-13` (doc comment only)

### Interfaces
Consumes: none new. Produces: no new exports — this task is markup/comment only.

### Steps

- [ ] Add `Ask`/`Capture` links to the `<nav>` block in `web/src/app/layout.tsx`, before the existing `검색` link (Ask/Capture are the primary daily-use actions per the design brief — "used daily" — so they lead the nav, not trail it):

```tsx
            <nav className="flex items-center gap-1 text-sm">
              <Link
                href="/ask"
                className="rounded-md px-3 py-1.5 text-foreground-muted transition-colors hover:bg-surface-subtle hover:text-foreground"
              >
                Ask
              </Link>
              <Link
                href="/capture"
                className="rounded-md px-3 py-1.5 text-foreground-muted transition-colors hover:bg-surface-subtle hover:text-foreground"
              >
                Capture
              </Link>
              <Link
                href="/"
                className="rounded-md px-3 py-1.5 text-foreground-muted transition-colors hover:bg-surface-subtle hover:text-foreground"
              >
                검색
              </Link>
              <Link
                href="/dashboard"
                className="rounded-md px-3 py-1.5 text-foreground-muted transition-colors hover:bg-surface-subtle hover:text-foreground"
              >
                수집 현황
              </Link>
              <Link
                href="/governance"
                className="rounded-md px-3 py-1.5 text-foreground-muted transition-colors hover:bg-surface-subtle hover:text-foreground"
              >
                거버넌스
              </Link>
            </nav>
```

- [ ] Update the doc comment at the top of `web/src/proxy.ts` (lines 1–13) to list the new routes — this is documentation only; the `matcher` regex below it already covers `/ask` and `/capture` (they are not in the exclusion list, so the existing catch-all protects them with zero code change) and already excludes `/api/v1/*` (so the Task 5/8 route handlers under `/api/notes/*` and `/api/ask` are automatically session-protected, and must **never** be moved under `/api/v1/*` — see Global Constraints):

```ts
/**
 * Next.js Edge Middleware — authentication gate.
 *
 * Protected routes (require GitHub OAuth session):
 *   - All page routes: /, /dashboard, /governance, /documents/*, /ask, /capture
 *   - Internal API proxy routes: /api/search, /api/documents/*, /api/stats/*,
 *     /api/sources, /api/ask, /api/notes/*
 *
 * Excluded from auth (pass-through):
 *   - /api/auth/*              — Auth.js sign-in / callback / sign-out
 *   - /api/v1/*                — All mobile proxy routes (Bearer API_KEY, not OAuth).
 *                                 Ask/Capture proxies MUST NOT be added under this
 *                                 prefix — see docs/superpowers/plans/2026-08-17-ask-capture-frontend.md
 *                                 Global Constraints for why that would silently
 *                                 disable session auth on them.
 *   - /_next/static, /_next/image — Next.js static assets
 *   - /favicon.ico, /robots.txt
 */
```

- [ ] Run `cd web && npm run lint` and confirm no new issues.

- [ ] Commit: `git add web/src/app/layout.tsx web/src/proxy.ts && git commit -m "feat(web): add Ask/Capture nav links, document their auth boundary"`

---

## Task 5: Capture route handlers

### Files
- Create: `web/src/app/api/notes/route.ts`
- Create: `web/src/app/api/notes/[id]/route.ts`
- Create: `web/src/app/api/notes/[id]/retry-enrichment/route.ts`

### Interfaces
Consumes:
- `POST /api/v1/notes`, `DELETE /api/v1/notes/{id}`, `POST /api/v1/notes/{id}/retry-enrichment` (spec §6.1, §6.3 — backend being implemented concurrently, see Global Constraints)
- `process.env.BRAIN_API_URL`, `process.env.API_KEY` (existing convention, `web/src/app/api/search/route.ts`)

Produces (consumed by Task 6):
- `POST /api/notes` (body `{ title?, content }` → `202 { id, status }`)
- `DELETE /api/notes/{id}` (→ backend status, body forwarded verbatim)
- `POST /api/notes/{id}/retry-enrichment` (no body → backend status)

### Steps

- [ ] Create `web/src/app/api/notes/route.ts`:

```ts
/**
 * Capture proxy — POST /api/notes
 *
 * Session-protected (web/src/proxy.ts catch-all — this path is NOT under
 * /api/v1/*, see that file's doc comment). Forwards the browser's note
 * content to the Go backend using the server-held API_KEY, mirroring
 * web/src/app/api/search/route.ts's pattern exactly.
 *
 * Request body: { title?: string; content: string }
 * Response: 202 { id: string; status: "pending" } (spec §6.1)
 */
import { type NextRequest, NextResponse } from "next/server";

const BACKEND_URL =
  process.env.BRAIN_API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:9200";
const API_KEY = process.env.API_KEY ?? "";

export async function POST(request: NextRequest): Promise<NextResponse> {
  const body = await request.text();

  try {
    const upstream = await fetch(`${BACKEND_URL}/api/v1/notes`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(API_KEY && { Authorization: `Bearer ${API_KEY}` }),
      },
      body,
    });
    const data: unknown = await upstream.json().catch(() => ({}));
    return NextResponse.json(data, { status: upstream.status });
  } catch (error: unknown) {
    const message = error instanceof Error ? error.message : "Upstream request failed";
    return NextResponse.json({ error: message }, { status: 502 });
  }
}
```

- [ ] Create `web/src/app/api/notes/[id]/route.ts`:

```ts
/**
 * Capture proxy — DELETE /api/notes/{id}
 * Cascades to derived insight documents on the backend (spec §6.5).
 */
import { type NextRequest, NextResponse } from "next/server";

const BACKEND_URL =
  process.env.BRAIN_API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:9200";
const API_KEY = process.env.API_KEY ?? "";

export async function DELETE(
  _request: NextRequest,
  { params }: { params: Promise<{ id: string }> },
): Promise<NextResponse> {
  const { id } = await params;

  try {
    const upstream = await fetch(`${BACKEND_URL}/api/v1/notes/${encodeURIComponent(id)}`, {
      method: "DELETE",
      headers: { ...(API_KEY && { Authorization: `Bearer ${API_KEY}` }) },
    });
    const text = await upstream.text();
    return new NextResponse(text.length > 0 ? text : null, {
      status: upstream.status,
      headers: { "Content-Type": "application/json" },
    });
  } catch (error: unknown) {
    const message = error instanceof Error ? error.message : "Upstream request failed";
    return NextResponse.json({ error: message }, { status: 502 });
  }
}
```

- [ ] Create `web/src/app/api/notes/[id]/retry-enrichment/route.ts`:

```ts
/**
 * Capture proxy — POST /api/notes/{id}/retry-enrichment
 * Only valid for notes in the terminal "failed" state; backend returns
 * 409 Conflict otherwise (spec §6.3, §9.2) — forwarded verbatim.
 */
import { type NextRequest, NextResponse } from "next/server";

const BACKEND_URL =
  process.env.BRAIN_API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:9200";
const API_KEY = process.env.API_KEY ?? "";

export async function POST(
  _request: NextRequest,
  { params }: { params: Promise<{ id: string }> },
): Promise<NextResponse> {
  const { id } = await params;

  try {
    const upstream = await fetch(
      `${BACKEND_URL}/api/v1/notes/${encodeURIComponent(id)}/retry-enrichment`,
      {
        method: "POST",
        headers: { ...(API_KEY && { Authorization: `Bearer ${API_KEY}` }) },
      },
    );
    const data: unknown = await upstream.json().catch(() => ({}));
    return NextResponse.json(data, { status: upstream.status });
  } catch (error: unknown) {
    const message = error instanceof Error ? error.message : "Upstream request failed";
    return NextResponse.json({ error: message }, { status: 502 });
  }
}
```

- [ ] Run `cd web && npm run type-check && npm run build` and confirm both succeed.

- [ ] **Not unit tested** (Global Constraints) — these three handlers are thin, side-effect-only proxies with no branching logic beyond status-code passthrough; the existing codebase has zero tests for its seven existing route handlers of the same shape (`api/search`, `api/documents`, `api/documents/[id]`, `api/documents/[id]/raw`, `api/documents/recent`, `api/stats`, `api/stats/baseline`, `api/sources` — confirmed by the file listing in Global Constraints research), so adding tests here would be new-and-inconsistent scope, not filling a gap. Verified manually in Task 10 once the concurrent backend plan merges.

- [ ] Commit: `git add web/src/app/api/notes && git commit -m "feat(web): add Capture route handlers (create/delete/retry-enrichment)"`

---

## Task 6: Capture screen

### Files
- Modify: `web/src/lib/api.ts` (append `createNote`, `retryNoteEnrichment`, `deleteNote`)
- Create: `web/src/app/capture/page.tsx`

### Interfaces
Consumes:
- `CreateNoteResponse`, `getNoteMetadata` (Task 2)
- `describeEnrichmentStatus` (Task 3)
- `listDocuments` (existing, `web/src/lib/api.ts`)
- `Button` (existing, `web/src/components/ui`)
- `formatRelative` (existing, `web/src/lib/dates.ts`)
- `POST /api/notes`, `DELETE /api/notes/{id}`, `POST /api/notes/{id}/retry-enrichment` (Task 5)

Produces:
- `web/src/lib/api.ts`: `createNote(content, title?)`, `retryNoteEnrichment(id)`, `deleteNote(id)`
- Route `/capture`

### Steps

- [ ] Append to `web/src/lib/api.ts` (after the `getSources` function, following the existing `fetchJson`/`getApiBase` pattern used by every other function in this file):

```ts
// ── Notes (Capture) ──────────────────────────────────────────────────────

export async function createNote(content: string, title = ""): Promise<CreateNoteResponse> {
  return fetchJson<CreateNoteResponse>(`${getApiBase()}/notes`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ title, content }),
  });
}

export async function retryNoteEnrichment(id: string): Promise<void> {
  await fetchJson<unknown>(`${getApiBase()}/notes/${encodeURIComponent(id)}/retry-enrichment`, {
    method: "POST",
  });
}

export async function deleteNote(id: string): Promise<void> {
  await fetchJson<unknown>(`${getApiBase()}/notes/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}
```

  and add `CreateNoteResponse` to the `import type { ... } from "./types";` block at the top of the file.

- [ ] Create `web/src/app/capture/page.tsx`:

```tsx
"use client";

import { useState, useEffect, useCallback, useRef } from "react";
import { listDocuments, createNote, retryNoteEnrichment, deleteNote } from "@/lib/api";
import { getNoteMetadata } from "@/lib/types";
import type { DocumentDetail } from "@/lib/types";
import { describeEnrichmentStatus } from "@/lib/enrichmentStatus";
import { formatRelative } from "@/lib/dates";
import { Button } from "@/components/ui";

const NOTES_LIMIT = 20;
const POLL_INTERVAL_MS = 15_000;

const BADGE_CLASS: Record<"success" | "warning" | "danger", string> = {
  success: "bg-[--color-status-success-subtle] text-success",
  warning: "bg-[--color-status-warning-subtle] text-warning",
  danger: "bg-[--color-status-danger-subtle] text-danger",
};

function NoteRow({
  doc,
  onRetry,
  onDelete,
}: {
  doc: DocumentDetail;
  onRetry: (id: string) => void;
  onDelete: (id: string) => void;
}) {
  const meta = getNoteMetadata(doc);
  const badge = describeEnrichmentStatus(meta.enrichment_status);

  return (
    <li className="border-b border-border px-4 py-3 last:border-b-0">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0 flex-1">
          <p className="line-clamp-1 text-sm text-foreground">
            {doc.title || <span className="text-foreground-subtle italic">제목 정리 중…</span>}
          </p>
          <p className="mt-1 line-clamp-2 text-xs text-foreground-muted">{doc.content}</p>
          {badge.retryable && meta.enrichment_last_error && (
            <p className="mt-1 text-xs text-danger">{meta.enrichment_last_error}</p>
          )}
        </div>
        <div className="flex shrink-0 flex-col items-end gap-1.5">
          <span
            className={`rounded px-1.5 py-0.5 text-xs font-medium ${BADGE_CLASS[badge.variant]}`}
          >
            {badge.label}
          </span>
          <div className="flex gap-2">
            {badge.retryable && (
              <button
                type="button"
                onClick={() => onRetry(doc.id)}
                className="text-xs text-accent hover:underline"
              >
                재시도
              </button>
            )}
            <button
              type="button"
              onClick={() => onDelete(doc.id)}
              className="text-xs text-foreground-subtle hover:text-danger hover:underline"
            >
              삭제
            </button>
          </div>
        </div>
      </div>
      <p className="mt-1.5 text-xs text-foreground-subtle">{formatRelative(doc.collected_at)}</p>
    </li>
  );
}

export default function CapturePage() {
  const [content, setContent] = useState("");
  const [notes, setNotes] = useState<DocumentDetail[]>([]);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null);

  const refresh = useCallback(async () => {
    try {
      const docs = await listDocuments({ source: "note", limit: NOTES_LIMIT });
      setNotes(docs);
    } catch {
      // A background refresh failing must not disrupt the compose field —
      // the user's in-progress typing is more important than a stale list.
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  // Poll only while at least one note is not yet "done" — the enrichment
  // worker ticks on its own schedule (backend plan: 5-minute interval), so
  // there is no push channel; short client polling is the simplest way to
  // reflect status changes without the user manually reloading.
  useEffect(() => {
    const hasPending = notes.some((n) => getNoteMetadata(n).enrichment_status !== "done");
    if (pollRef.current) {
      clearInterval(pollRef.current);
      pollRef.current = null;
    }
    if (hasPending) {
      pollRef.current = setInterval(() => void refresh(), POLL_INTERVAL_MS);
    }
    return () => {
      if (pollRef.current) clearInterval(pollRef.current);
    };
  }, [notes, refresh]);

  async function handleSave() {
    const trimmed = content.trim();
    if (!trimmed || saving) return;
    setSaving(true);
    setError(null);
    try {
      const result = await createNote(trimmed);
      setContent("");
      const now = new Date().toISOString();
      // Optimistic prepend — title/enrichment status are filled in
      // asynchronously by the worker (spec §6.1); an empty title here is
      // correct, not a bug (see the "제목 정리 중…" placeholder above).
      setNotes((prev) => [
        {
          id: result.id,
          title: "",
          content: trimmed,
          source_type: "note",
          source_id: result.id,
          status: "active",
          collected_at: now,
          created_at: now,
          updated_at: now,
          metadata: { enrichment_status: "pending", enrichment_attempts: 0 },
        },
        ...prev,
      ]);
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "저장 중 오류가 발생했습니다.");
    } finally {
      setSaving(false);
    }
  }

  async function handleRetry(id: string) {
    try {
      await retryNoteEnrichment(id);
      await refresh();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "재시도 요청이 실패했습니다.");
    }
  }

  async function handleDelete(id: string) {
    const prev = notes;
    setNotes((cur) => cur.filter((n) => n.id !== id)); // optimistic
    try {
      await deleteNote(id);
    } catch (e: unknown) {
      setNotes(prev); // roll back on failure
      setError(e instanceof Error ? e.message : "삭제가 실패했습니다.");
    }
  }

  return (
    <div className="space-y-6">
      {/* Compose — single field + large save button on every breakpoint
          (spec §7.3): the desktop and mobile requirements converge here,
          so this section does not need responsive variants the way Ask's
          input bar does (Task 9). */}
      <div className="space-y-2">
        <textarea
          value={content}
          onChange={(e) => setContent(e.target.value)}
          placeholder="무슨 생각이 드셨나요?"
          rows={4}
          disabled={saving}
          className="w-full resize-none rounded-lg border border-border bg-surface px-3 py-2.5 text-sm text-foreground placeholder:text-foreground-subtle focus:ring-2 focus:ring-accent/40 focus:outline-none disabled:opacity-50"
        />
        <Button
          type="button"
          variant="primary"
          size="lg"
          onClick={() => void handleSave()}
          loading={saving}
          disabled={saving || !content.trim()}
          className="w-full sm:w-auto"
        >
          저장
        </Button>
        {error && <p className="text-sm text-danger">{error}</p>}
      </div>

      {/* Recent notes */}
      <div className="rounded-lg border border-border bg-surface">
        <div className="border-b border-border px-4 py-3">
          <h2 className="text-sm font-medium text-foreground">최근 노트</h2>
        </div>
        {notes.length === 0 ? (
          <p className="px-4 py-8 text-center text-sm text-foreground-subtle">
            아직 노트가 없습니다
          </p>
        ) : (
          <ul>
            {notes.map((doc) => (
              <NoteRow key={doc.id} doc={doc} onRetry={handleRetry} onDelete={handleDelete} />
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}
```

- [ ] Run `cd web && npm run type-check && npm run lint` and confirm both succeed.

- [ ] **Not unit tested** (Global Constraints — no component-test infra exists or is added by this plan). Manually verified in Task 10 once the concurrent Capture backend plan merges: save persists and clears the field, the note appears immediately with "정리 중" badge, polling picks up the eventual "정리 완료"/"정리 실패" transition, retry re-triggers enrichment on a failed note, delete removes the row and rolls back on network failure.

- [ ] Commit: `git add web/src/lib/api.ts web/src/app/capture/page.tsx && git commit -m "feat(web): add Capture screen (compose, recent-notes list, retry/delete)"`

---

## Task 7: SSE event parser (pure, unit-tested)

### Files
- Create: `web/src/lib/sseEvents.ts`
- Create: `web/src/lib/sseEvents.test.ts`

### Interfaces
Consumes:
- `AskSourceItem`, `AskStreamEvent` (Task 2)
- Spec §5.1's exact SSE contract (`event: sources|token|done|error`, JSON `data:` payloads)

Produces (consumed by Task 9 Ask screen):
- `export interface RawSSEEvent { event: string; data: string }`
- `export function splitSSEBuffer(buffer: string): { events: RawSSEEvent[]; remainder: string }`
- `export function parseAskEvent(raw: RawSSEEvent): AskStreamEvent | null`

### Steps

- [ ] Write the failing test file `web/src/lib/sseEvents.test.ts`:

```ts
import { describe, it, expect } from "vitest";
import { splitSSEBuffer, parseAskEvent } from "./sseEvents";

const SOURCES_EVENT =
  'event: sources\ndata: {"sources":[{"id":"a","title":"t","source_type":"sms","score":0.8}]}\n\n';
const TOKEN_EVENT = 'event: token\ndata: {"text":"hello "}\n\n';
const DONE_EVENT = 'event: done\ndata: {"finish_reason":"stop"}\n\n';
const ERROR_EVENT = 'event: error\ndata: {"message":"boom"}\n\n';

describe("splitSSEBuffer", () => {
  it("parses a single complete event and leaves no remainder", () => {
    const { events, remainder } = splitSSEBuffer(TOKEN_EVENT);
    expect(events).toHaveLength(1);
    expect(events[0]).toEqual({ event: "token", data: '{"text":"hello "}' });
    expect(remainder).toBe("");
  });

  it("parses multiple events arriving in one chunk", () => {
    const { events } = splitSSEBuffer(SOURCES_EVENT + TOKEN_EVENT + DONE_EVENT);
    expect(events.map((e) => e.event)).toEqual(["sources", "token", "done"]);
  });

  it("holds back an incomplete trailing event across chunk boundaries", () => {
    // Simulates fetch's ReadableStream splitting mid-event — a byte
    // boundary that lands inside the DONE_EVENT, not at its edge.
    const chunk1 = TOKEN_EVENT + DONE_EVENT.slice(0, 10);
    const chunk2 = DONE_EVENT.slice(10);

    const first = splitSSEBuffer(chunk1);
    expect(first.events).toEqual([{ event: "token", data: '{"text":"hello "}' }]);
    expect(first.remainder).toBe(DONE_EVENT.slice(0, 10));

    const second = splitSSEBuffer(first.remainder + chunk2);
    expect(second.events).toEqual([{ event: "done", data: '{"finish_reason":"stop"}' }]);
    expect(second.remainder).toBe("");
  });
});

describe("parseAskEvent", () => {
  it("parses a sources event", () => {
    const [raw] = splitSSEBuffer(SOURCES_EVENT).events;
    expect(parseAskEvent(raw)).toEqual({
      type: "sources",
      sources: [{ id: "a", title: "t", source_type: "sms", score: 0.8 }],
    });
  });

  it("parses a token event", () => {
    const [raw] = splitSSEBuffer(TOKEN_EVENT).events;
    expect(parseAskEvent(raw)).toEqual({ type: "token", text: "hello " });
  });

  it("parses a done event", () => {
    const [raw] = splitSSEBuffer(DONE_EVENT).events;
    expect(parseAskEvent(raw)).toEqual({ type: "done", finish_reason: "stop" });
  });

  it("parses an error event", () => {
    const [raw] = splitSSEBuffer(ERROR_EVENT).events;
    expect(parseAskEvent(raw)).toEqual({ type: "error", message: "boom" });
  });

  it("returns null for an unrecognized event name rather than throwing", () => {
    expect(parseAskEvent({ event: "ping", data: "" })).toBeNull();
  });

  it("returns null for malformed JSON rather than throwing", () => {
    expect(parseAskEvent({ event: "token", data: "{not json" })).toBeNull();
  });
});
```

- [ ] Run `cd web && npx vitest run src/lib/sseEvents.test.ts` and confirm it fails to compile (`Cannot find module './sseEvents'`).

- [ ] Create `web/src/lib/sseEvents.ts`:

```ts
import type { AskStreamEvent, AskSourceItem } from "./types";

export interface RawSSEEvent {
  event: string;
  data: string;
}

/**
 * Splits accumulated SSE text on the "\n\n" event delimiter (spec §5.1's
 * wire format). fetch's ReadableStream delivers arbitrary byte boundaries
 * that do not line up with SSE event boundaries — callers must keep
 * `remainder` and prepend it to the next chunk before calling this again.
 */
export function splitSSEBuffer(buffer: string): { events: RawSSEEvent[]; remainder: string } {
  const parts = buffer.split("\n\n");
  const remainder = parts.pop() ?? "";
  const events: RawSSEEvent[] = [];

  for (const part of parts) {
    if (!part.trim()) continue;
    let event = "message";
    const dataLines: string[] = [];
    for (const line of part.split("\n")) {
      if (line.startsWith("event:")) {
        event = line.slice("event:".length).trim();
      } else if (line.startsWith("data:")) {
        dataLines.push(line.slice("data:".length).trim());
      }
    }
    events.push({ event, data: dataLines.join("\n") });
  }

  return { events, remainder };
}

/**
 * Maps a raw SSE event into a typed AskStreamEvent per the spec §5.1
 * contract. Returns null (never throws) for unrecognized event names or
 * malformed JSON payloads — a single bad event must not crash the whole
 * stream reader loop (Task 9).
 */
export function parseAskEvent(raw: RawSSEEvent): AskStreamEvent | null {
  try {
    switch (raw.event) {
      case "sources": {
        const payload = JSON.parse(raw.data) as { sources: AskSourceItem[] };
        return { type: "sources", sources: payload.sources ?? [] };
      }
      case "token": {
        const payload = JSON.parse(raw.data) as { text: string };
        return { type: "token", text: payload.text ?? "" };
      }
      case "done": {
        const payload = JSON.parse(raw.data) as {
          finish_reason: "stop" | "error" | "no_evidence";
        };
        return { type: "done", finish_reason: payload.finish_reason };
      }
      case "error": {
        const payload = JSON.parse(raw.data) as { message: string };
        return { type: "error", message: payload.message ?? "unknown error" };
      }
      default:
        return null;
    }
  } catch {
    return null;
  }
}
```

- [ ] Run `cd web && npx vitest run src/lib/sseEvents.test.ts` and confirm all 9 tests pass.

- [ ] Commit: `git add web/src/lib/sseEvents.ts web/src/lib/sseEvents.test.ts && git commit -m "feat(web): add SSE event buffer splitter + Ask event parser"`

---

## Task 8 — BLOCKED on backend `POST /api/v1/ask` (separate LATER plan): Ask SSE proxy route

> Do not start this task before Tasks 1–7 are merged and Capture is usable end-to-end. `POST /api/v1/ask` does not exist in the Go backend yet (spec §5, §14 — a separate, later plan). This task is written against the documented SSE contract so it is ready to wire up the moment that plan ships, but it cannot be exercised against a real upstream until then.

### Files
- Create: `web/src/app/api/ask/route.ts`

### Interfaces
Consumes:
- `POST /api/v1/ask` (spec §5.1 — not yet implemented)

Produces (consumed by Task 9):
- `POST /api/ask` — SSE pass-through, same origin/content-type as the upstream stream

### Steps

- [ ] Create `web/src/app/api/ask/route.ts`. This is the one genuinely tricky piece of the whole plan: **the response body must be streamed through unmodified, not buffered.** Every other proxy in this codebase (including Task 5's) calls `.json()`/`.text()` on the upstream response before replying — that pattern is wrong here, because buffering would make the browser wait for the entire LLM generation to finish before seeing anything, which defeats the reason `/ask` streams at all.

```ts
/**
 * Ask proxy — POST /api/ask
 *
 * Session-protected (web/src/proxy.ts catch-all). Unlike every other proxy
 * in this app, this handler does NOT buffer the upstream response with
 * .json()/.text() — it passes upstream.body straight through as the
 * Response body, preserving the SSE stream (spec §5.1). Buffering here
 * would silently turn a streaming answer into a "wait for everything, then
 * show it all at once" answer, with no error and no obvious symptom other
 * than a much longer time-to-first-token.
 *
 * BLOCKED: POST /api/v1/ask does not exist in the Go backend yet — this
 * handler is written to the documented contract (spec §5.1) but is not
 * exercisable end-to-end until that plan ships (Global Constraints).
 */
import { type NextRequest } from "next/server";

const BACKEND_URL =
  process.env.BRAIN_API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:9200";
const API_KEY = process.env.API_KEY ?? "";

export async function POST(request: NextRequest): Promise<Response> {
  const body = await request.text();

  let upstream: Response;
  try {
    upstream = await fetch(`${BACKEND_URL}/api/v1/ask`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(API_KEY && { Authorization: `Bearer ${API_KEY}` }),
      },
      body,
    });
  } catch (error: unknown) {
    const message = error instanceof Error ? error.message : "upstream unreachable";
    return new Response(JSON.stringify({ error: message }), {
      status: 502,
      headers: { "Content-Type": "application/json" },
    });
  }

  // A non-SSE error response (e.g. 401/400/503 per spec §5.1's error table)
  // is still JSON, not a stream — forward it as-is rather than trying to
  // stream a body that was never a stream.
  if (!upstream.body || !(upstream.headers.get("content-type") ?? "").includes("text/event-stream")) {
    const text = await upstream.text();
    return new Response(text, {
      status: upstream.status,
      headers: { "Content-Type": upstream.headers.get("content-type") ?? "application/json" },
    });
  }

  return new Response(upstream.body, {
    status: upstream.status,
    headers: {
      "Content-Type": "text/event-stream",
      "Cache-Control": "no-cache, no-transform",
      Connection: "keep-alive",
      // Disables response buffering in any reverse proxy sitting between
      // this handler and the browser (cloudflared, per spec §8) — without
      // this, an intermediate proxy can still batch the stream even though
      // this handler itself does not.
      "X-Accel-Buffering": "no",
    },
  });
}

// Never cache an SSE response — each call is a unique, stateful generation.
export const dynamic = "force-dynamic";
```

- [ ] Run `cd web && npm run type-check && npm run build` and confirm both succeed (this validates the handler compiles and type-checks against the Next.js route-handler signature; it does not and cannot validate the actual streaming behavior without a live backend).

- [ ] **Not unit tested, and not manually verifiable yet** — there is no upstream to test against. Once the Ask backend plan ships and is deployed to a dev/staging target, verify manually: confirm the browser's Network tab shows response bytes arriving incrementally (not all at once at the end), confirm a client-side abort (navigating away mid-stream) does not leave a hung connection, confirm a 401 (logged-out) and a 503 (LLM disabled) both return their JSON body correctly instead of an empty stream.

- [ ] Commit: `git add web/src/app/api/ask/route.ts && git commit -m "feat(web): add Ask SSE pass-through proxy (blocked on backend POST /api/v1/ask)"`

---

## Task 9 — BLOCKED on backend `POST /api/v1/ask` (separate LATER plan): Ask screen

> Same blocking condition as Task 8. Do not start before Tasks 1–7 are merged.

### Files
- Create: `web/src/app/ask/page.tsx`

### Interfaces
Consumes:
- `splitSSEBuffer`, `parseAskEvent` (Task 7)
- `AskSourceItem` (Task 2)
- `getAskLayer`, `ASK_LAYER_LABELS` (Task 2)
- `Button` (existing, `web/src/components/ui`)
- `POST /api/ask` (Task 8)

Produces: route `/ask`

### Steps

- [ ] Create `web/src/app/ask/page.tsx`:

```tsx
"use client";

import { useRef, useState } from "react";
import { splitSSEBuffer, parseAskEvent } from "@/lib/sseEvents";
import type { AskSourceItem } from "@/lib/types";
import { getAskLayer, ASK_LAYER_LABELS } from "@/lib/constants";
import { Button } from "@/components/ui";

function SourceCard({ source }: { source: AskSourceItem }) {
  const layer = getAskLayer(source.source_type);
  const isInsight = layer === "insight";
  return (
    <div
      className={`rounded-md border px-3 py-2 text-xs ${
        isInsight ? "border-accent/30 bg-accent-subtle" : "border-border bg-surface-subtle"
      }`}
    >
      <div className="flex items-center gap-1.5">
        <span className={`font-medium ${isInsight ? "text-accent" : "text-foreground-subtle"}`}>
          {ASK_LAYER_LABELS[layer]}
        </span>
        <span className="line-clamp-1 text-foreground-muted">{source.title}</span>
      </div>
    </div>
  );
}

export default function AskPage() {
  const [question, setQuestion] = useState("");
  const [answer, setAnswer] = useState("");
  const [sources, setSources] = useState<AskSourceItem[]>([]);
  const [streaming, setStreaming] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const bufferRef = useRef("");

  async function handleAsk() {
    const trimmed = question.trim();
    if (!trimmed || streaming) return;

    setAnswer("");
    setSources([]);
    setError(null);
    setStreaming(true);
    bufferRef.current = "";

    try {
      const res = await fetch("/api/ask", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ question: trimmed }),
      });

      if (!res.ok || !res.body) {
        const body = (await res.json().catch(() => ({}))) as { error?: string };
        throw new Error(body.error ?? `요청이 실패했습니다 (${res.status})`);
      }

      const reader = res.body.getReader();
      const decoder = new TextDecoder();

      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;

        bufferRef.current += decoder.decode(value, { stream: true });
        const { events, remainder } = splitSSEBuffer(bufferRef.current);
        bufferRef.current = remainder;

        for (const raw of events) {
          const evt = parseAskEvent(raw);
          if (!evt) continue;
          if (evt.type === "sources") setSources(evt.sources);
          else if (evt.type === "token") setAnswer((prev) => prev + evt.text);
          else if (evt.type === "error") setError(evt.message);
          else if (evt.type === "done" && evt.finish_reason === "no_evidence") {
            setError((prev) => prev ?? "관련된 근거를 찾지 못했습니다.");
          }
        }
      }
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "답변 생성 중 오류가 발생했습니다.");
    } finally {
      setStreaming(false);
    }
  }

  return (
    <div className="flex min-h-[calc(100vh-8rem)] flex-col">
      {/* Answer + sources */}
      <div className="flex-1 space-y-4 overflow-y-auto pb-24 sm:pb-4">
        {sources.length > 0 && (
          <div className="flex flex-wrap gap-1.5">
            {sources.map((s) => (
              <SourceCard key={s.id} source={s} />
            ))}
          </div>
        )}
        {answer && (
          <p className="text-sm leading-relaxed whitespace-pre-wrap text-foreground">{answer}</p>
        )}
        {error && <p className="text-sm text-danger">{error}</p>}
        {!answer && !error && !streaming && (
          <p className="py-12 text-center text-sm text-foreground-subtle">
            내 데이터에 질문해보세요
          </p>
        )}
      </div>

      {/* Input — mobile: fixed to the viewport bottom (spec §7.2's required
          deviation). This app has a real prior incident with an element
          fixed to the bottom edge overlapping content underneath it (a
          save button hidden behind the tab bar on a past mobile release) —
          the pb-24 on the scroll container above exists specifically to
          reserve room for this bar so the same class of bug does not repeat
          here. Desktop: inline, no fixed positioning needed. */}
      <div className="fixed inset-x-0 bottom-0 border-t border-border bg-background px-4 py-3 sm:static sm:border-0 sm:bg-transparent sm:px-0 sm:py-0">
        <div className="mx-auto flex max-w-4xl gap-2">
          <input
            type="text"
            value={question}
            onChange={(e) => setQuestion(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && !e.shiftKey) {
                e.preventDefault();
                void handleAsk();
              }
            }}
            placeholder="질문을 입력하세요…"
            disabled={streaming}
            className="flex-1 rounded-lg border border-border bg-surface px-3 py-2.5 text-sm text-foreground placeholder:text-foreground-subtle focus:ring-2 focus:ring-accent/40 focus:outline-none disabled:opacity-50"
          />
          <Button
            type="button"
            variant="primary"
            onClick={() => void handleAsk()}
            loading={streaming}
            disabled={streaming || !question.trim()}
          >
            질문
          </Button>
        </div>
      </div>
    </div>
  );
}
```

- [ ] Run `cd web && npm run type-check && npm run lint` and confirm both succeed.

- [ ] **Not unit tested, and not manually verifiable end-to-end yet** — same reason as Task 8. The stream-reading loop itself is exercised indirectly by Task 7's `sseEvents.test.ts` (the parsing logic this loop calls is what was actually tricky to get right); the loop's own control flow (reader lifecycle, buffer threading between reads) has no automated coverage and is deferred to manual verification once Task 8's upstream exists.

- [ ] Commit: `git add web/src/app/ask/page.tsx && git commit -m "feat(web): add Ask screen (blocked on backend POST /api/v1/ask)"`

---

## Task 10: Manual verification checklist

This app has no browser automation (no Playwright/Cypress) and this plan does not add any (Global Constraints). Everything below is a required manual pass, not optional polish — it is the only verification these behaviors get.

### Files
None — this task produces no files, only a verification pass.

### Steps

- [ ] **Theme (Task 1)**: load every existing page (`/`, `/dashboard`, `/governance`, `/documents/[id]`, `/login`, `/api-docs`) and confirm no `undefined`/broken CSS custom property renders as visible text, all previously-colored source badges remain legible against the new near-black surfaces, and code blocks on `/documents/[id]` render with the dark highlight.js theme (no white/light rectangle).
- [ ] **Capture, once the concurrent backend plan (`docs/superpowers/plans/2026-08-17-capture-backend.md`) is merged and deployed**: type a note and save — content clears, note appears immediately at the top of the list with "정리 중"; wait for a worker tick (or trigger one manually) and confirm the badge transitions to "정리 완료" with a real title; force a failure (e.g. temporarily point `LLM_API_URL` at an invalid endpoint) and confirm three attempts later the note shows "정리 실패" with a working "재시도" button; delete a note and confirm it disappears from the list and its derived insights (if any existed) are gone from search.
- [ ] **Capture mobile**: on a real device or narrow emulator viewport, confirm the compose textarea and save button are usable without horizontal scroll and the save button is never obscured by any OS chrome or nav bar (this project has a specific prior incident of exactly this class of bug on the mobile app — verify carefully, not just glance).
- [ ] **Ask, once the LATER backend plan ships**: ask a question with results, confirm sources render before the answer text starts appearing (spec §5.1 event order), confirm `insight`-layer sources are visually distinct (accent-tinted) from `note`/observed sources, ask a question with zero corpus matches and confirm the "관련된 근거를 찾지 못했습니다" message appears, kill the network mid-stream and confirm the UI recovers to an error state rather than hanging on "질문" button's loading spinner forever.
- [ ] **Ask mobile**: confirm the input bar stays fixed to the bottom edge while scrolling a long answer, and confirm the last line of the answer is not hidden underneath that fixed bar (same class of bug as the Capture mobile check above — `pb-24` on the scroll container is the guard for this, verify it actually works at real viewport heights, not just in theory).
- [ ] Run `cd web && npm run lint && npm run type-check && npm test && npm run build` one final time, all green, before considering this plan done.

---

## Self-Review

Walking each in-scope spec section (as amended 2026-08-17) against the task that implements it:

| Spec section | Implementing task |
|---|---|
| §7.1 linear.app tokens, dark-only, accessibility caveat | Task 1 |
| §7.1 file layout note (`web/DESIGN.md`, no component-library directory) | Task 1 (precondition), File Structure |
| Route path correction (`/api/*` not `/api/v1/*`, added during this plan's own grounding, not literally in the spec's original §12) | Task 4, Task 5, Task 8 |
| §7.2 Ask desktop/mobile (source cards, layered by source_type, fixed bottom input on mobile) | Task 7 (layer classification, parsing), Task 9 |
| §7.3 Capture desktop/mobile (single field, large save button, enrichment-status list, retry action) | Task 3 (status mapping), Task 6 |
| §6.1 `POST /api/v1/notes` consumption | Task 5, Task 6 |
| §6.3 retry-enrichment consumption, terminal-failure UI | Task 5, Task 6 |
| §6.5 `DELETE /api/v1/notes/{id}` consumption | Task 5, Task 6 |
| §5.1 SSE event contract (`sources`/`token`/`done`/`error`) | Task 7, Task 8, Task 9 |
| §5.2 layered context (내 노트/관찰 데이터/추론 visual distinction) | Task 2 (`getAskLayer`), Task 9 |
| §7.4 auth reuse (no new auth mechanism, existing next-auth session) | Task 4, Task 5, Task 8 (all rely on `web/src/proxy.ts`, unmodified logic) |

Gaps found and folded in during scoping, not left residual:
- The spec's original file layout (§12, pre-amendment) placed the new proxies under `web/src/app/api/v1/*`, which would have silently bypassed OAuth given how `web/src/proxy.ts` actually excludes that prefix. Caught by reading `web/src/proxy.ts` directly rather than trusting the spec's literal path — corrected in the spec amendment itself (§12) and carried through consistently in Tasks 4/5/8 here.
- The spec did not specify how the Capture screen's note *list* is populated (only create/retry/delete are mentioned as endpoints). Resolved by reusing the existing `GET /api/v1/documents?source=` path already proxied and wrapped — no new backend surface invented (Task 6).
- The dark-only theme change has a real, unrelated side effect (`highlight.js/styles/github.css` mismatch, previously partial, now total) — fixed in Task 1 rather than left as a silent regression, since it is a one-line, low-risk fix directly caused by this plan's own decision.

Not turned into a concrete task, with reason:
- **Re-theming the other 5 existing pages with the new tokens** — explicitly out of scope (spec §2 비목표, unchanged by the 2026-08-17 amendment).
- **Markdown rendering for streamed Ask answers** — the spec does not require it; plain `white-space: pre-wrap` text avoids introducing XSS surface for an unrequested feature (Global Constraints).
- **Component/integration test suite (Playwright, React Testing Library, MSW)** — this app has never had one; Global Constraints explains why this plan does not start one, and Task 10 is the compensating manual-verification pass.
- **Rate-limiting on `/api/notes/{id}/retry-enrichment`** — the backend spec itself leaves this undecided (§13.3); no frontend behavior depends on a decision that hasn't been made.

Placeholder scan: no "TBD", "add appropriate error handling", "write tests for the above", or "similar to Task N" phrases appear in any task's code blocks — each implementation step above contains complete, real TypeScript/CSS, not a stub.

Blocked-task scan: Tasks 8 and 9 are explicitly marked BLOCKED in their own headings and Global Constraints, sequenced last, and their verification steps say plainly what cannot be checked yet and why — they are not disguised as complete when they are not.
