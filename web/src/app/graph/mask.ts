/**
 * Client-side display masking for entity names (plan §privacy 3).
 *
 * This is a screen-sharing aid, not a security boundary: the API response is
 * unchanged and still carries the real names, because server-side masking
 * would make evidence cross-checking impossible. Masking is applied only at
 * render time, in the browser.
 *
 * Rule: keep the first character, replace every remaining character —
 * including spaces — with `*`, so the masked length does not hint at word
 * boundaries.
 */
export function maskName(name: string): string {
  const chars = Array.from(name);
  if (chars.length <= 1) return name;
  return chars[0] + "*".repeat(chars.length - 1);
}

/** Convenience wrapper for render paths that carry the toggle state. */
export function displayName(name: string, mask: boolean): string {
  return mask ? maskName(name) : name;
}

/** localStorage key for the masking toggle. */
export const MASK_STORAGE_KEY = "graph.maskNames";

// ── Toggle store ──────────────────────────────────────────────────────────
//
// Exposed as an external store (useSyncExternalStore) rather than an effect
// that reads localStorage after mount: the server snapshot is "off", so the
// prerendered HTML never depends on a browser-only value.

type Listener = () => void;

const listeners = new Set<Listener>();
let cached: boolean | null = null;

export function subscribeMask(listener: Listener): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

export function getMaskSnapshot(): boolean {
  if (cached === null) {
    cached = window.localStorage.getItem(MASK_STORAGE_KEY) === "true";
  }
  return cached;
}

/** Masking is always off in prerendered output. */
export function getMaskServerSnapshot(): boolean {
  return false;
}

export function setMask(value: boolean): void {
  cached = value;
  window.localStorage.setItem(MASK_STORAGE_KEY, String(value));
  for (const listener of listeners) listener();
}
