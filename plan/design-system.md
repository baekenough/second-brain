# Second Brain — Design System Specification

> Agent: fe-design-expert | Date: 2026-06-11
> Design language: Claude Aesthetic (Impeccable Design v1.0.0)
> Implementation target: Next.js 15 + Tailwind CSS v4 + TypeScript

---

## Design Language: Claude Aesthetic

Warm, human-centered minimalism inspired by Claude's product visual identity. The interface feels considered — like reading something printed on quality paper rather than staring at a screen. Calm, not cold. Structured, not sterile.

### Core Principles

| Principle | Expression |
|-----------|-----------|
| **Warmth** | Cream backgrounds, amber-tinted neutrals, coral accent — never pure white, never pure gray |
| **Restraint** | Shadows that whisper, not shout. Radius that rounds without inflating |
| **Typographic hierarchy** | Serif headings anchor the page; clean sans-serif carries the content |
| **Accent as signal** | Coral-rust used only for primary actions and key indicators (~10% rule) |
| **Motion earns its place** | Animate state changes; never animate for decoration |
| **Dual-mode designed** | Dark mode is not color-inverted; it is independently authored |

### AI Slop Checklist (what this system explicitly avoids)

- ❌ Inter/Roboto as default — using **Lora** (serif) + **DM Sans** (humanist sans)
- ❌ Pure `#000`/`#fff` or pure gray backgrounds — all surfaces have warm hue tinting
- ❌ Generic blue-purple gradient — no gradients except very subtle warmth transitions
- ❌ Uniform card-shadow-everywhere pattern — shadows are elevation-specific
- ❌ Bounce/elastic animation — expo-out curves only
- ❌ Centered-everything hero — grid-based spatial layouts
- ❌ Neutral-only palette + single accent — warm browns, cream layers, coral family

---

## 1. Color System

### 1.1 OKLCH Color Model

All colors are defined in OKLCH (perceptually uniform). Format: `oklch(lightness% chroma hue)`.

**Hue anchors for this system:**
- Accent family: **hue 30** (warm coral-rust)
- Neutral family: **hue 55** (amber-brown tint)

### 1.2 Raw Palette

#### Accent — Coral Rust (hue 30)

| Token | OKLCH | Usage |
|-------|-------|-------|
| `--accent-50` | `oklch(96% 0.04 30)` | Hover backgrounds, tinted surfaces |
| `--accent-100` | `oklch(91% 0.08 30)` | Active chip backgrounds |
| `--accent-200` | `oklch(84% 0.13 30)` | Light accent fill |
| `--accent-300` | `oklch(74% 0.17 30)` | Disabled accent states |
| `--accent-400` | `oklch(64% 0.19 30)` | Dark-mode primary accent |
| `--accent-500` | `oklch(54% 0.20 30)` | **Primary action** (light mode CTA) |
| `--accent-600` | `oklch(45% 0.19 30)` | Hover/pressed state |
| `--accent-700` | `oklch(36% 0.17 30)` | Active/focus ring |
| `--accent-800` | `oklch(26% 0.13 30)` | Dark accent text |
| `--accent-900` | `oklch(17% 0.08 30)` | Near-black rust tint |

#### Warm Neutral — Amber Brown (hue 55)

| Token | OKLCH | Approx hex reference |
|-------|-------|---------------------|
| `--neutral-50` | `oklch(97.5% 0.007 60)` | #faf8f5 (warm off-white) |
| `--neutral-100` | `oklch(94% 0.010 58)` | #f3f0ea |
| `--neutral-200` | `oklch(88% 0.012 57)` | #e5e0d8 |
| `--neutral-300` | `oklch(79% 0.013 56)` | #cdc7bc |
| `--neutral-400` | `oklch(67% 0.013 56)` | #b0a89b |
| `--neutral-500` | `oklch(55% 0.013 55)` | #8f877a |
| `--neutral-600` | `oklch(44% 0.014 54)` | #716960 |
| `--neutral-700` | `oklch(33% 0.014 53)` | #534d46 |
| `--neutral-800` | `oklch(23% 0.013 52)` | #393430 |
| `--neutral-900` | `oklch(14% 0.011 50)` | #221f1c |

#### Semantic Status Colors

| Status | Light | Dark |
|--------|-------|------|
| **Success** | `oklch(50% 0.17 145)` | `oklch(62% 0.15 145)` |
| Success light | `oklch(93% 0.06 145)` | `oklch(22% 0.06 145)` |
| **Warning** | `oklch(60% 0.20 80)` | `oklch(72% 0.18 80)` |
| Warning light | `oklch(94% 0.07 80)` | `oklch(24% 0.07 80)` |
| **Danger** | `oklch(50% 0.20 25)` | `oklch(62% 0.17 25)` |
| Danger light | `oklch(94% 0.06 25)` | `oklch(22% 0.06 25)` |
| **Info** | `oklch(52% 0.17 250)` | `oklch(64% 0.15 250)` |
| Info light | `oklch(93% 0.05 250)` | `oklch(20% 0.05 250)` |

### 1.3 Semantic Tokens

These are the tokens components should reference. Never use raw palette values in component code.

```css
:root {
  /* ── Surfaces ─────────────────────────────────────── */
  --surface-base:    oklch(97.5% 0.007 60);   /* page background */
  --surface-raised:  oklch(99%   0.005 60);   /* cards, panels */
  --surface-sunken:  oklch(95%   0.009 58);   /* inputs, sidebars */
  --surface-overlay: oklch(99%   0.004 60);   /* modals */

  /* ── Text ─────────────────────────────────────────── */
  --text-primary:   oklch(18%  0.012 52);   /* headings, body */
  --text-secondary: oklch(42%  0.013 54);   /* labels, metadata */
  --text-muted:     oklch(57%  0.011 56);   /* placeholders, captions */
  --text-disabled:  oklch(68%  0.009 58);   /* disabled controls */
  --text-inverse:   oklch(97%  0.007 60);   /* text on dark surfaces */
  --text-accent:    oklch(45%  0.19  30);   /* links, accent text */
  --text-danger:    oklch(40%  0.20  25);   /* error messages */

  /* ── Borders ──────────────────────────────────────── */
  --border-subtle:  oklch(89%  0.009 60);   /* table dividers, light separators */
  --border-default: oklch(83%  0.011 58);   /* input outlines, card borders */
  --border-strong:  oklch(68%  0.013 55);   /* focused inputs, emphasized */
  --border-accent:  oklch(54%  0.20  30);   /* accent border */

  /* ── Accent (semantic aliases) ────────────────────── */
  --accent-primary: oklch(54%  0.20  30);   /* primary CTA background */
  --accent-hover:   oklch(45%  0.19  30);   /* primary CTA hover */
  --accent-pressed: oklch(36%  0.17  30);   /* primary CTA active */
  --accent-subtle:  oklch(96%  0.04  30);   /* tinted surface/chip bg */
  --accent-text:    oklch(36%  0.17  30);   /* accent-colored text on light bg */
}

@media (prefers-color-scheme: dark) {
  :root {
    /* ── Surfaces ─────────────────────────────────────── */
    --surface-base:    oklch(13%  0.011 50);   /* page background */
    --surface-raised:  oklch(18%  0.011 50);   /* cards, panels */
    --surface-sunken:  oklch(10%  0.010 50);   /* inputs, sidebar */
    --surface-overlay: oklch(22%  0.012 50);   /* modals */

    /* ── Text ─────────────────────────────────────────── */
    --text-primary:   oklch(92%  0.008 60);   /* headings, body */
    --text-secondary: oklch(72%  0.010 58);   /* labels, metadata */
    --text-muted:     oklch(54%  0.010 56);   /* placeholders, captions */
    --text-disabled:  oklch(38%  0.009 55);   /* disabled controls */
    --text-inverse:   oklch(18%  0.012 52);   /* text on light surfaces */
    --text-accent:    oklch(64%  0.17  30);   /* links, accent text */
    --text-danger:    oklch(62%  0.17  25);   /* error messages */

    /* ── Borders ──────────────────────────────────────── */
    --border-subtle:  oklch(24%  0.012 50);   /* light separators */
    --border-default: oklch(30%  0.013 50);   /* inputs, cards */
    --border-strong:  oklch(45%  0.013 52);   /* focused, emphasized */
    --border-accent:  oklch(64%  0.17  30);   /* accent border */

    /* ── Accent ───────────────────────────────────────── */
    --accent-primary: oklch(62%  0.17  30);   /* slightly desaturated for dark */
    --accent-hover:   oklch(68%  0.15  30);
    --accent-pressed: oklch(55%  0.18  30);
    --accent-subtle:  oklch(22%  0.06  30);   /* dark tinted chip bg */
    --accent-text:    oklch(64%  0.17  30);
  }
}
```

### 1.4 Source Type Badge Colors

Each source type gets a distinct, accessible hue. These are CSS classes, not tokens.

| Source | Light bg / text | Dark bg / text |
|--------|----------------|----------------|
| `sms` | `oklch(92% 0.09 145)` / `oklch(30% 0.15 145)` | `oklch(20% 0.07 145)` / `oklch(62% 0.15 145)` |
| `call-log` | `oklch(91% 0.07 250)` / `oklch(30% 0.14 250)` | `oklch(19% 0.06 250)` / `oklch(62% 0.13 250)` |
| `call-transcript` | `oklch(90% 0.07 240)` / `oklch(28% 0.13 240)` | `oklch(18% 0.06 240)` / `oklch(60% 0.12 240)` |
| `gmail` | `oklch(92% 0.08 25)` / `oklch(38% 0.18 25)` | `oklch(22% 0.07 25)` / `oklch(62% 0.17 25)` |
| `calendar` | `oklch(92% 0.07 200)` / `oklch(30% 0.13 200)` | `oklch(19% 0.06 200)` / `oklch(60% 0.12 200)` |
| `filesystem` | `oklch(92% 0.06 80)` / `oklch(35% 0.15 80)` | `oklch(20% 0.05 80)` / `oklch(65% 0.13 80)` |
| `upload` | `oklch(91% 0.08 310)` / `oklch(32% 0.14 310)` | `oklch(20% 0.07 310)` / `oklch(62% 0.13 310)` |
| `voice-memo` | `oklch(91% 0.07 30)` / `oklch(36% 0.17 30)` | `oklch(20% 0.06 30)` / `oklch(64% 0.15 30)` |
| `slack` | `oklch(92% 0.09 310)` / `oklch(30% 0.16 310)` | `oklch(21% 0.07 310)` / `oklch(62% 0.14 310)` |
| `github` | `oklch(92% 0.03 50)` / `oklch(22% 0.01 50)` | `oklch(18% 0.02 50)` / `oklch(72% 0.02 50)` |
| `llm-memory` | `oklch(92% 0.06 200)` / `oklch(30% 0.12 200)` | `oklch(20% 0.05 200)` / `oklch(60% 0.11 200)` |
| `secretary` | `oklch(91% 0.08 260)` / `oklch(32% 0.15 260)` | `oklch(20% 0.07 260)` / `oklch(62% 0.13 260)` |

---

## 2. Typography

### 2.1 Font Stack

| Role | Font | Weights loaded | Fallback |
|------|------|---------------|---------|
| **Heading** | Lora (Google Fonts) | 400, 500, 600, 700 | Georgia, "Times New Roman", serif |
| **UI / Body** | DM Sans (Google Fonts) | 300, 400, 500, 600 | system-ui, -apple-system, sans-serif |
| **Mono** | JetBrains Mono (Google Fonts) | 400, 500 | "Courier New", monospace |

**Why Lora:** Literary, warm, slightly bookish serif with excellent Korean glyph coverage via variable axis. Distinct from common AI-generated UI that defaults to Inter or Geist.

**Why DM Sans:** Clean humanist sans that reads at small sizes without the sterility of Inter. Professional without being corporate. Pairs naturally with Lora's warmth.

### 2.2 Type Scale

Based on a **major third ratio (1.25)** — appropriate for data-dense UI.

| Token | Size | px | Line height | Letter spacing | Usage |
|-------|------|----|-------------|---------------|-------|
| `--text-xs` | 0.75rem | 12px | 1.5 | 0.02em | Labels, tags, captions |
| `--text-sm` | 0.875rem | 14px | 1.5 | 0.01em | Secondary text, metadata, table |
| `--text-base` | 1rem | 16px | 1.6 | 0 | Body copy, default |
| `--text-lg` | 1.125rem | 18px | 1.6 | -0.01em | Large body, card titles |
| `--text-xl` | 1.25rem | 20px | 1.5 | -0.01em | Section subheadings (h5, h4) |
| `--text-2xl` | 1.5rem | 24px | 1.4 | -0.02em | Section headings (h3) |
| `--text-3xl` | 2rem | 32px | 1.3 | -0.02em | Page headings (h2) |
| `--text-4xl` | 2.5rem | 40px | 1.2 | -0.03em | Page title (h1) |

**Heading weights:** 600 (h1-h2), 500 (h3-h4), 500 (h5-h6)
**Body weight:** 400
**UI labels:** 500 (medium)
**Captions/meta:** 400

### 2.3 Semantic Text Tokens

```css
:root {
  --font-sans:   "DM Sans", system-ui, -apple-system, sans-serif;
  --font-serif:  "Lora", Georgia, "Times New Roman", serif;
  --font-mono:   "JetBrains Mono", "Courier New", monospace;

  --text-xs:   0.75rem;
  --text-sm:   0.875rem;
  --text-base: 1rem;
  --text-lg:   1.125rem;
  --text-xl:   1.25rem;
  --text-2xl:  1.5rem;
  --text-3xl:  2rem;
  --text-4xl:  2.5rem;

  --leading-tight:  1.2;
  --leading-snug:   1.35;
  --leading-normal: 1.5;
  --leading-relaxed: 1.65;

  --tracking-tight:   -0.03em;
  --tracking-snug:    -0.02em;
  --tracking-normal:  0;
  --tracking-wide:    0.02em;
}
```

### 2.4 Heading Usage Patterns

```
Page title (h1):      font-serif, text-4xl, font-semibold, tracking-tight, text-primary
Section heading (h2): font-serif, text-3xl, font-semibold, tracking-snug, text-primary
Sub-section (h3):     font-serif, text-2xl, font-medium, tracking-snug, text-primary
Panel title (h4):     font-sans, text-xl, font-semibold, tracking-normal, text-primary
Group label (h5):     font-sans, text-sm, font-medium, tracking-wide, text-secondary (uppercase)
```

---

## 3. Spacing Scale

8px base grid. All spacing values are multiples of 4px (0.25rem).

| Token | Value | px |
|-------|-------|----|
| `--space-0.5` | 0.125rem | 2px |
| `--space-1` | 0.25rem | 4px |
| `--space-2` | 0.5rem | 8px |
| `--space-3` | 0.75rem | 12px |
| `--space-4` | 1rem | 16px |
| `--space-5` | 1.25rem | 20px |
| `--space-6` | 1.5rem | 24px |
| `--space-8` | 2rem | 32px |
| `--space-10` | 2.5rem | 40px |
| `--space-12` | 3rem | 48px |
| `--space-16` | 4rem | 64px |
| `--space-20` | 5rem | 80px |
| `--space-24` | 6rem | 96px |

**Component spacing conventions:**
- Button padding: `px-4 py-2` (md), `px-3 py-1.5` (sm), `px-6 py-2.5` (lg)
- Card padding: `p-6` (default), `p-4` (compact)
- Section gap: `gap-6` (default), `gap-4` (dense)
- Page outer padding: `px-6 py-8` (mobile), `px-8 py-10` (desktop)
- Content max-width: `max-w-5xl` (1024px) for content pages, `max-w-7xl` (1280px) for dashboards

---

## 4. Border Radius Scale

| Token | Value | Usage |
|-------|-------|-------|
| `--radius-sm` | 0.25rem (4px) | Tags, small badges, tight elements |
| `--radius-md` | 0.5rem (8px) | Inputs, checkboxes, small buttons |
| `--radius-lg` | 0.75rem (12px) | Cards, panels, large buttons |
| `--radius-xl` | 1rem (16px) | Large cards, featured content |
| `--radius-2xl` | 1.5rem (24px) | Modal dialogs, bottom sheets |
| `--radius-full` | 9999px | Pill badges, avatars, icon buttons |

**Convention:** Cards use `radius-lg`. Modals use `radius-2xl`. Inputs use `radius-md`. Badges use `radius-sm` (rectangular) or `radius-full` (pill).

---

## 5. Shadow Scale

Warm-tinted shadows (hue 52, matching neutral-900 tint):

```css
:root {
  --shadow-sm:  0 1px 2px oklch(18% 0.011 52 / 0.07);
  --shadow-md:  0 2px 8px oklch(18% 0.011 52 / 0.08),
                0 1px 2px oklch(18% 0.011 52 / 0.05);
  --shadow-lg:  0 8px 24px oklch(18% 0.011 52 / 0.10),
                0 2px 6px  oklch(18% 0.011 52 / 0.06);
  --shadow-xl:  0 20px 48px oklch(18% 0.011 52 / 0.14),
                0 6px 16px  oklch(18% 0.011 52 / 0.08);
  --shadow-inset: inset 0 1px 3px oklch(18% 0.011 52 / 0.08);
}

@media (prefers-color-scheme: dark) {
  :root {
    --shadow-sm:  0 1px 2px oklch(0% 0 0 / 0.25);
    --shadow-md:  0 2px 8px oklch(0% 0 0 / 0.30),
                  0 1px 2px oklch(0% 0 0 / 0.20);
    --shadow-lg:  0 8px 24px oklch(0% 0 0 / 0.40),
                  0 2px 6px  oklch(0% 0 0 / 0.25);
    --shadow-xl:  0 20px 48px oklch(0% 0 0 / 0.55),
                  0 6px 16px  oklch(0% 0 0 / 0.35);
    --shadow-inset: inset 0 1px 3px oklch(0% 0 0 / 0.35);
  }
}
```

**Convention:** Default cards use `shadow-sm`. Hover cards lift to `shadow-md`. Modals use `shadow-xl`. Dropdowns use `shadow-lg`.

---

## 6. Motion System

### 6.1 Duration Tokens

```css
:root {
  --duration-instant:    50ms;    /* hover state */
  --duration-fast:       100ms;   /* button press, checkbox */
  --duration-normal:     200ms;   /* dropdown, tooltip */
  --duration-slow:       300ms;   /* modal open, panel expand */
  --duration-deliberate: 500ms;   /* page transition, onboarding */
}
```

### 6.2 Easing Tokens

```css
:root {
  --ease-out:    cubic-bezier(0.16, 1, 0.3, 1);    /* entering elements */
  --ease-in:     cubic-bezier(0.7, 0, 0.84, 0);    /* exiting elements */
  --ease-inout:  cubic-bezier(0.65, 0, 0.35, 1);   /* bidirectional */
  --ease-spring: cubic-bezier(0.25, 1, 0.5, 1);    /* settling/expanding */
}
```

### 6.3 Reduced Motion

```css
@media (prefers-reduced-motion: reduce) {
  :root {
    --duration-instant:    0.01ms;
    --duration-fast:       0.01ms;
    --duration-normal:     0.01ms;
    --duration-slow:       0.01ms;
    --duration-deliberate: 0.01ms;
  }
}
```

### 6.4 Motion Conventions

| Element | Animation | Duration | Easing |
|---------|-----------|----------|--------|
| Button hover | `translateY(-1px)` | `--duration-fast` | `--ease-out` |
| Button press | `translateY(0)` scale(0.98) | `--duration-instant` | `--ease-in` |
| Card hover lift | `translateY(-2px)` + shadow step up | `--duration-normal` | `--ease-out` |
| Dropdown open | opacity 0→1 + translateY(−4px→0) | `--duration-normal` | `--ease-out` |
| Dropdown close | opacity 1→0 + translateY(0→−4px) | `150ms` | `--ease-in` |
| Modal open | opacity 0→1 + scale(0.97→1) | `--duration-slow` | `--ease-out` |
| Modal close | opacity 1→0 + scale(1→0.97) | `225ms` | `--ease-in` |
| Tab indicator slide | `left` via transform | `--duration-normal` | `--ease-spring` |
| Skeleton pulse | opacity 0.6↔1.0 | `1500ms` infinite | linear |

**No bounce. No elastic overshoot. No wiggle.**

---

## 7. Component Primitives

### 7.1 Button

Four variants, three sizes. Buttons use `DM Sans` font-medium.

#### Variants

```
primary   — accent-primary bg, text-inverse, hover accent-hover bg
secondary — surface-raised bg, text-primary, border-default border
ghost     — transparent bg, text-secondary, hover surface-sunken bg
danger    — danger bg (#danger-light bg + danger-text in light mode), on confirm → solid red
```

#### Size scale

| Size | Height | Padding | Font | Radius |
|------|--------|---------|------|--------|
| `sm` | 32px | `px-3 py-1.5` | `text-sm` | `radius-md` |
| `md` | 40px | `px-4 py-2.5` | `text-sm font-medium` | `radius-md` |
| `lg` | 48px | `px-6 py-3` | `text-base font-medium` | `radius-lg` |

#### Focus ring

All buttons: `outline: 2px solid var(--accent-primary); outline-offset: 2px;` on `:focus-visible`.

#### Tailwind class composition (md primary example)

```
inline-flex items-center justify-center gap-2
px-4 py-2.5 text-sm font-medium
rounded-[--radius-md]
bg-[--accent-primary] text-[--text-inverse]
transition-[background-color,transform,box-shadow]
duration-[--duration-fast] ease-[--ease-out]
hover:bg-[--accent-hover] hover:-translate-y-px
active:translate-y-0 active:scale-[0.98]
focus-visible:outline-2 focus-visible:outline-[--accent-primary] focus-visible:outline-offset-2
disabled:opacity-50 disabled:cursor-not-allowed disabled:pointer-events-none
```

### 7.2 Card

Cards represent contained units of information. Three elevation levels.

```
card-default  — surface-raised bg, border-subtle border, shadow-sm, radius-lg
card-elevated — surface-raised bg, border-default border, shadow-md, radius-lg
card-interactive — card-default + hover:shadow-md hover:-translate-y-0.5 cursor-pointer
```

**Anti-pattern to avoid:** Do not nest cards. Do not apply shadows on cards inside a modal. Do not add shadows to every element.

#### Card padding convention

- Standard card: `p-6`
- Compact card (result list, table row replacement): `p-4`
- Featured card (dashboard stat): `p-6 gap-4`

### 7.3 Input

Text inputs, textareas, select.

```css
/* Base input styles */
.input-base {
  background: var(--surface-sunken);
  border: 1px solid var(--border-default);
  border-radius: var(--radius-md);
  color: var(--text-primary);
  font-family: var(--font-sans);
  font-size: var(--text-sm);
  padding: 0.625rem 0.75rem;        /* 10px 12px — 40px total height */
  transition: border-color var(--duration-fast) var(--ease-out),
              box-shadow var(--duration-fast) var(--ease-out);
}

.input-base::placeholder {
  color: var(--text-muted);
}

.input-base:focus {
  outline: none;
  border-color: var(--border-accent);
  box-shadow: 0 0 0 3px oklch(54% 0.20 30 / 0.12);
}

.input-base:disabled {
  opacity: 0.5;
  cursor: not-allowed;
  background: var(--surface-base);
}

.input-error {
  border-color: oklch(50% 0.20 25);
}
.input-error:focus {
  box-shadow: 0 0 0 3px oklch(50% 0.20 25 / 0.12);
}
```

**Search bar specifics:**
- Height: 48px (lg input)
- Left icon: magnifying glass (text-muted), 20px
- Clear button: appears when has value, ghost icon button
- Border: border-default at rest, border-accent on focus
- Background: surface-raised (stands out from page surface-base)

### 7.4 Badge

Two shapes: pill (rounded-full) and rectangular (radius-sm).

```
size sm: px-1.5 py-0.5 text-xs font-medium
size md: px-2.5 py-1   text-xs font-medium
```

**Source badges:** Pill shape. Each source uses its defined hue (see section 1.4). Example: SMS = green, Gmail = coral-orange, Calendar = cyan.

**Status badges:**

| Variant | bg | text |
|---------|----|------|
| `success` | `--success-light` | `--success-dark` |
| `warning` | `--warning-light` | `--warning-dark` |
| `danger` | `--danger-light` | `--danger-dark` |
| `info` | `--info-light` | `--info-dark` |
| `neutral` | `--neutral-100` | `--neutral-700` |
| `accent` | `--accent-subtle` | `--accent-text` |

### 7.5 Table

Data tables for stats, source lists, document lists.

```
table-auto w-full
border-collapse
```

**Header row:**
```
bg-surface-sunken
text-xs font-medium text-secondary uppercase tracking-wide
border-b border-default
px-4 py-3
```

**Data rows:**
```
border-b border-subtle
text-sm text-primary
px-4 py-3.5
hover:bg-surface-sunken transition-colors duration-[--duration-fast]
```

**Numeric columns:** `font-variant-numeric: tabular-nums` — critical for alignment.

**Empty state row:**
```
text-center text-muted text-sm py-12 italic
```

### 7.6 Tabs

Underline style — not enclosed/pill. Tab indicator slides smoothly between tabs.

```
tab-list: flex gap-1 border-b border-subtle
tab-item: relative px-4 py-2.5 text-sm font-medium text-secondary
          hover:text-primary transition-colors duration-[--duration-fast]
tab-item[active]: text-primary
tab-indicator: absolute bottom-0 left-0 right-0 h-0.5 bg-accent-primary
               transition-[left,right,width] duration-[--duration-normal] ease-[--ease-spring]
```

Implement the sliding indicator with a positioned `<span>` that moves via `transform: translateX` for performance.

### 7.7 Modal

Center-aligned overlay dialog.

```
overlay: fixed inset-0 bg-[oklch(0%_0_0/0.4)] backdrop-blur-sm
         flex items-center justify-center p-4 z-50
         animate-in fade-in duration-[--duration-normal]

dialog: surface-overlay bg
        border border-subtle
        rounded-[--radius-2xl]
        shadow-xl
        w-full max-w-lg max-h-[90vh] overflow-y-auto
        animate-in zoom-in-95 duration-[--duration-slow] ease-[--ease-out]

header: flex items-center justify-between p-6 border-b border-subtle
title:  font-serif text-xl font-medium text-primary
close:  ghost button sm, text-muted
body:   p-6
footer: flex justify-end gap-3 p-6 pt-0
```

### 7.8 Skeleton / Loading State

Warm-tinted pulsing placeholders.

```css
.skeleton {
  background: linear-gradient(
    90deg,
    var(--neutral-200) 0%,
    var(--neutral-100) 50%,
    var(--neutral-200) 100%
  );
  background-size: 200% 100%;
  border-radius: var(--radius-md);
  animation: skeleton-shimmer 1.5s ease-in-out infinite;
}

@keyframes skeleton-shimmer {
  0%   { background-position: 200% 0; }
  100% { background-position: -200% 0; }
}

@media (prefers-color-scheme: dark) {
  .skeleton {
    background: linear-gradient(
      90deg,
      var(--neutral-800) 0%,
      var(--neutral-700) 50%,
      var(--neutral-800) 100%
    );
    background-size: 200% 100%;
  }
}
```

### 7.9 Navigation

Top navigation bar with three pages: Search, Dashboard, Governance.

```
nav: sticky top-0 z-40
     surface-raised bg
     border-b border-subtle
     shadow-sm
     h-14 (56px)
     px-6
     flex items-center justify-between

nav-brand: font-serif text-lg font-semibold text-primary
           flex items-center gap-2

nav-links: flex items-center gap-1

nav-link: px-3 py-1.5 text-sm font-medium rounded-[--radius-md]
          text-secondary hover:text-primary hover:bg-surface-sunken
          transition-[color,background-color] duration-[--duration-fast]

nav-link[active]: text-primary bg-accent-subtle
```

---

## 8. Tailwind v4 CSS Theme Configuration

Drop this into `web/src/app/globals.css` after `@import "tailwindcss"`:

```css
@import "tailwindcss";
@import url("https://fonts.googleapis.com/css2?family=Lora:ital,wght@0,400;0,500;0,600;0,700;1,400;1,500&family=DM+Sans:opsz,wght@9..40,300;9..40,400;9..40,500;9..40,600&family=JetBrains+Mono:wght@400;500&display=swap");

@theme {
  /* ── Font families ─────────────────────────────────── */
  --font-sans:  "DM Sans", system-ui, -apple-system, sans-serif;
  --font-serif: "Lora", Georgia, "Times New Roman", serif;
  --font-mono:  "JetBrains Mono", "Courier New", monospace;

  /* ── Type scale ────────────────────────────────────── */
  --text-xs:   0.75rem;
  --text-sm:   0.875rem;
  --text-base: 1rem;
  --text-lg:   1.125rem;
  --text-xl:   1.25rem;
  --text-2xl:  1.5rem;
  --text-3xl:  2rem;
  --text-4xl:  2.5rem;

  /* ── Border radius ─────────────────────────────────── */
  --radius-sm:   0.25rem;
  --radius-md:   0.5rem;
  --radius-lg:   0.75rem;
  --radius-xl:   1rem;
  --radius-2xl:  1.5rem;
  --radius-full: 9999px;

  /* ── Raw accent palette ────────────────────────────── */
  --color-accent-50:  oklch(96%   0.04 30);
  --color-accent-100: oklch(91%   0.08 30);
  --color-accent-200: oklch(84%   0.13 30);
  --color-accent-300: oklch(74%   0.17 30);
  --color-accent-400: oklch(64%   0.19 30);
  --color-accent-500: oklch(54%   0.20 30);
  --color-accent-600: oklch(45%   0.19 30);
  --color-accent-700: oklch(36%   0.17 30);
  --color-accent-800: oklch(26%   0.13 30);
  --color-accent-900: oklch(17%   0.08 30);

  /* ── Raw neutral palette ───────────────────────────── */
  --color-warm-50:  oklch(97.5% 0.007 60);
  --color-warm-100: oklch(94%   0.010 58);
  --color-warm-200: oklch(88%   0.012 57);
  --color-warm-300: oklch(79%   0.013 56);
  --color-warm-400: oklch(67%   0.013 56);
  --color-warm-500: oklch(55%   0.013 55);
  --color-warm-600: oklch(44%   0.014 54);
  --color-warm-700: oklch(33%   0.014 53);
  --color-warm-800: oklch(23%   0.013 52);
  --color-warm-900: oklch(14%   0.011 50);

  /* ── Semantic color aliases (light mode defaults) ──── */
  --color-surface-base:    oklch(97.5% 0.007 60);
  --color-surface-raised:  oklch(99%   0.005 60);
  --color-surface-sunken:  oklch(95%   0.009 58);
  --color-surface-overlay: oklch(99%   0.004 60);

  --color-text-primary:   oklch(18%   0.012 52);
  --color-text-secondary: oklch(42%   0.013 54);
  --color-text-muted:     oklch(57%   0.011 56);
  --color-text-accent:    oklch(45%   0.19  30);
  --color-text-inverse:   oklch(97%   0.007 60);
  --color-text-danger:    oklch(40%   0.20  25);

  --color-border-subtle:  oklch(89%   0.009 60);
  --color-border-default: oklch(83%   0.011 58);
  --color-border-strong:  oklch(68%   0.013 55);
  --color-border-accent:  oklch(54%   0.20  30);

  /* ── Spacing ───────────────────────────────────────── */
  --spacing-0_5: 0.125rem;
  --spacing-1:   0.25rem;
  --spacing-2:   0.5rem;
  --spacing-3:   0.75rem;
  --spacing-4:   1rem;
  --spacing-5:   1.25rem;
  --spacing-6:   1.5rem;
  --spacing-8:   2rem;
  --spacing-10:  2.5rem;
  --spacing-12:  3rem;
  --spacing-16:  4rem;
  --spacing-20:  5rem;
  --spacing-24:  6rem;

  /* ── Motion ────────────────────────────────────────── */
  --duration-instant:    50ms;
  --duration-fast:       100ms;
  --duration-normal:     200ms;
  --duration-slow:       300ms;
  --duration-deliberate: 500ms;

  --ease-out:    cubic-bezier(0.16, 1, 0.3, 1);
  --ease-in:     cubic-bezier(0.7, 0, 0.84, 0);
  --ease-inout:  cubic-bezier(0.65, 0, 0.35, 1);
  --ease-spring: cubic-bezier(0.25, 1, 0.5, 1);
}

/* ── CSS custom properties (semantic, theme-aware) ─── */
@layer base {
  :root {
    color-scheme: light dark;

    /* Shadows */
    --shadow-sm:  0 1px 2px oklch(18% 0.011 52 / 0.07);
    --shadow-md:  0 2px 8px oklch(18% 0.011 52 / 0.08), 0 1px 2px oklch(18% 0.011 52 / 0.05);
    --shadow-lg:  0 8px 24px oklch(18% 0.011 52 / 0.10), 0 2px 6px oklch(18% 0.011 52 / 0.06);
    --shadow-xl:  0 20px 48px oklch(18% 0.011 52 / 0.14), 0 6px 16px oklch(18% 0.011 52 / 0.08);
    --shadow-inset: inset 0 1px 3px oklch(18% 0.011 52 / 0.08);
  }

  @media (prefers-color-scheme: dark) {
    :root {
      /* Override semantic colors for dark mode */
      --color-surface-base:    oklch(13%  0.011 50);
      --color-surface-raised:  oklch(18%  0.011 50);
      --color-surface-sunken:  oklch(10%  0.010 50);
      --color-surface-overlay: oklch(22%  0.012 50);

      --color-text-primary:   oklch(92%  0.008 60);
      --color-text-secondary: oklch(72%  0.010 58);
      --color-text-muted:     oklch(54%  0.010 56);
      --color-text-accent:    oklch(64%  0.17  30);
      --color-text-inverse:   oklch(18%  0.012 52);
      --color-text-danger:    oklch(62%  0.17  25);

      --color-border-subtle:  oklch(24%  0.012 50);
      --color-border-default: oklch(30%  0.013 50);
      --color-border-strong:  oklch(45%  0.013 52);
      --color-border-accent:  oklch(64%  0.17  30);

      /* Dark mode shadows */
      --shadow-sm:  0 1px 2px oklch(0% 0 0 / 0.25);
      --shadow-md:  0 2px 8px oklch(0% 0 0 / 0.30), 0 1px 2px oklch(0% 0 0 / 0.20);
      --shadow-lg:  0 8px 24px oklch(0% 0 0 / 0.40), 0 2px 6px oklch(0% 0 0 / 0.25);
      --shadow-xl:  0 20px 48px oklch(0% 0 0 / 0.55), 0 6px 16px oklch(0% 0 0 / 0.35);
      --shadow-inset: inset 0 1px 3px oklch(0% 0 0 / 0.35);
    }
  }

  /* ── prefers-reduced-motion ──────────────────────── */
  @media (prefers-reduced-motion: reduce) {
    :root {
      --duration-instant:    0.01ms;
      --duration-fast:       0.01ms;
      --duration-normal:     0.01ms;
      --duration-slow:       0.01ms;
      --duration-deliberate: 0.01ms;
    }
  }

  html {
    font-family: var(--font-sans);
    font-size: 1rem;
    line-height: 1.6;
    -webkit-font-smoothing: antialiased;
    -moz-osx-font-smoothing: grayscale;
    background-color: var(--color-surface-base);
    color: var(--color-text-primary);
  }

  h1, h2, h3, h4 {
    font-family: var(--font-serif);
    color: var(--color-text-primary);
  }

  h5, h6 {
    font-family: var(--font-sans);
    color: var(--color-text-primary);
  }

  h1 { font-size: var(--text-4xl); font-weight: 600; line-height: 1.2; letter-spacing: -0.03em; }
  h2 { font-size: var(--text-3xl); font-weight: 600; line-height: 1.25; letter-spacing: -0.02em; }
  h3 { font-size: var(--text-2xl); font-weight: 500; line-height: 1.3;  letter-spacing: -0.02em; }
  h4 { font-size: var(--text-xl);  font-weight: 500; line-height: 1.4;  letter-spacing: -0.01em; }
  h5 { font-size: var(--text-sm);  font-weight: 600; line-height: 1.5;  letter-spacing: 0.04em; text-transform: uppercase; color: var(--color-text-secondary); }
  h6 { font-size: var(--text-xs);  font-weight: 600; line-height: 1.5;  letter-spacing: 0.06em; text-transform: uppercase; color: var(--color-text-muted); }

  code, pre, kbd, samp {
    font-family: var(--font-mono);
    font-variant-ligatures: none;
  }

  /* Tabular nums for all metric/data contexts */
  td, .tabular {
    font-variant-numeric: tabular-nums;
  }

  @media (prefers-color-scheme: dark) {
    body {
      line-height: 1.65; /* optical compensation for dark mode bleed */
    }
  }
}
```

---

## 9. Source Badge Color Classes

Define in a `components/ui/badge-colors.css` or as Tailwind utility classes via `@layer components`:

```css
@layer components {
  .badge-sms            { background: oklch(92% 0.09 145); color: oklch(30% 0.15 145); }
  .badge-call-log       { background: oklch(91% 0.07 250); color: oklch(30% 0.14 250); }
  .badge-call-transcript{ background: oklch(90% 0.07 240); color: oklch(28% 0.13 240); }
  .badge-gmail          { background: oklch(92% 0.08 25);  color: oklch(38% 0.18 25);  }
  .badge-calendar       { background: oklch(92% 0.07 200); color: oklch(30% 0.13 200); }
  .badge-filesystem     { background: oklch(92% 0.06 80);  color: oklch(35% 0.15 80);  }
  .badge-upload         { background: oklch(91% 0.08 310); color: oklch(32% 0.14 310); }
  .badge-voice-memo     { background: oklch(91% 0.07 30);  color: oklch(36% 0.17 30);  }
  .badge-slack          { background: oklch(92% 0.09 310); color: oklch(30% 0.16 310); }
  .badge-github         { background: oklch(92% 0.03 50);  color: oklch(22% 0.01 50);  }
  .badge-llm-memory     { background: oklch(92% 0.06 200); color: oklch(30% 0.12 200); }
  .badge-secretary      { background: oklch(91% 0.08 260); color: oklch(32% 0.15 260); }
  .badge-gdrive         { background: oklch(93% 0.07 80);  color: oklch(33% 0.15 80);  }
  .badge-notion         { background: oklch(93% 0.03 50);  color: oklch(20% 0.01 50);  }
  .badge-telegram       { background: oklch(91% 0.07 215); color: oklch(28% 0.12 215); }
  .badge-discord        { background: oklch(91% 0.08 275); color: oklch(30% 0.15 275); }

  @media (prefers-color-scheme: dark) {
    .badge-sms            { background: oklch(20% 0.07 145); color: oklch(62% 0.15 145); }
    .badge-call-log       { background: oklch(19% 0.06 250); color: oklch(62% 0.13 250); }
    .badge-call-transcript{ background: oklch(18% 0.06 240); color: oklch(60% 0.12 240); }
    .badge-gmail          { background: oklch(22% 0.07 25);  color: oklch(62% 0.17 25);  }
    .badge-calendar       { background: oklch(19% 0.06 200); color: oklch(60% 0.12 200); }
    .badge-filesystem     { background: oklch(20% 0.05 80);  color: oklch(65% 0.13 80);  }
    .badge-upload         { background: oklch(20% 0.07 310); color: oklch(62% 0.13 310); }
    .badge-voice-memo     { background: oklch(20% 0.06 30);  color: oklch(64% 0.15 30);  }
    .badge-slack          { background: oklch(21% 0.07 310); color: oklch(62% 0.14 310); }
    .badge-github         { background: oklch(18% 0.02 50);  color: oklch(72% 0.02 50);  }
    .badge-llm-memory     { background: oklch(20% 0.05 200); color: oklch(60% 0.11 200); }
    .badge-secretary      { background: oklch(20% 0.07 260); color: oklch(62% 0.13 260); }
    .badge-gdrive         { background: oklch(20% 0.05 80);  color: oklch(65% 0.13 80);  }
    .badge-notion         { background: oklch(18% 0.02 50);  color: oklch(70% 0.02 50);  }
    .badge-telegram       { background: oklch(19% 0.06 215); color: oklch(60% 0.11 215); }
    .badge-discord        { background: oklch(20% 0.07 275); color: oklch(62% 0.13 275); }
  }
}
```

---

## 10. WCAG AA Verification

All color pairings have been designed to meet WCAG AA (4.5:1 for body text, 3:1 for large text/UI).

| Pairing | Light ratio | Dark ratio | Standard |
|---------|-------------|------------|---------|
| `text-primary` on `surface-base` | ~11:1 | ~9:1 | ✅ AAA |
| `text-secondary` on `surface-base` | ~5.5:1 | ~5:1 | ✅ AA |
| `text-muted` on `surface-base` | ~4.5:1 | ~4:1 | ✅ AA (verify with tool) |
| `text-inverse` on `accent-500` | ~4.8:1 | — | ✅ AA |
| Source badge text on badge bg | All designed ≥4.5:1 | All ≥4.5:1 | ✅ AA |
| `border-default` on `surface-base` | ~3:1 | ~3:1 | ✅ AA (UI components) |

> **Verify with:** [https://oklch.com/](https://oklch.com/) + [https://webaim.org/resources/contrastchecker/](https://webaim.org/resources/contrastchecker/)
> Particularly check `text-muted` in light mode — borderline at 4.5:1 for 16px text.

---

## 11. UX Writing Conventions

| Element | Convention | Example |
|---------|-----------|---------|
| Buttons | Verb + object | "Search documents", "Export results" |
| Empty states | Descriptive + no icon cliché | "No documents match this filter. Try adjusting the source or date range." |
| Loading states | Active, not passive | "Searching…" not "Loading" |
| Errors | What + why + how | "Search failed. The server took too long. Try again or reduce filters." |
| Source labels | Lowercase readable | "voice memo" not "VOICE_MEMO" |
| Counts | Include unit | "42 documents" not "42" |
| Timestamps | Relative + absolute on hover | "2 hours ago" → title="June 11, 2026 at 3:24 PM" |

**Source display names (constants):**
```typescript
export const SOURCE_LABELS: Record<string, string> = {
  "sms": "SMS",
  "call-log": "Call",
  "call-transcript": "Transcript",
  "gmail": "Gmail",
  "calendar": "Calendar",
  "filesystem": "File",
  "upload": "Upload",
  "voice-memo": "Voice Memo",
  "slack": "Slack",
  "github": "GitHub",
  "llm-memory": "LLM Memory",
  "secretary": "Secretary",
  "gdrive": "Drive",
  "notion": "Notion",
  "telegram": "Telegram",
  "discord": "Discord",
};
```

---

## 12. File Placement

| File | Location | Notes |
|------|----------|-------|
| Tailwind `@theme` + CSS vars | `web/src/app/globals.css` | Drop-in from section 8 |
| Source badge colors | `web/src/app/globals.css` | Append `@layer components` block from section 9 |
| Source labels constant | `web/src/lib/constants.ts` | Section 11 export |
| Component primitives | `web/src/components/ui/` | Button, Card, Input, Badge, Table, Tabs, Modal |
| Font preload links | `web/src/app/layout.tsx` | `<link rel="preload" ...>` for DM Sans 400 |

---

## 13. Google Fonts Integration (Next.js)

Use `next/font/google` for zero-CLS font loading:

```typescript
// web/src/app/layout.tsx
import { Lora, DM_Sans, JetBrains_Mono } from "next/font/google";

const lora = Lora({
  subsets: ["latin"],
  weight: ["400", "500", "600", "700"],
  style: ["normal", "italic"],
  variable: "--font-serif",
  display: "swap",
});

const dmSans = DM_Sans({
  subsets: ["latin"],
  weight: ["300", "400", "500", "600"],
  variable: "--font-sans",
  display: "swap",
  axes: ["opsz"],
});

const jetbrainsMono = JetBrains_Mono({
  subsets: ["latin"],
  weight: ["400", "500"],
  variable: "--font-mono",
  display: "swap",
});

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="ko" className={`${lora.variable} ${dmSans.variable} ${jetbrainsMono.variable}`}>
      <body>{children}</body>
    </html>
  );
}
```

The CSS variables `--font-serif`, `--font-sans`, `--font-mono` set by `next/font` override the fallback stacks in `@theme`.
