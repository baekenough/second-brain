import type { SourceType } from "./types";

/** Human-readable labels for each source type. */
export const SOURCE_LABELS: Record<SourceType, string> = {
  slack: "Slack",
  github: "GitHub",
  gdrive: "Drive",
  notion: "Notion",
  filesystem: "Files",
  discord: "Discord",
  telegram: "Telegram",
  secretary: "Secretary",
  "llm-memory": "Memory",
  gmail: "Gmail",
  calendar: "Calendar",
  sms: "SMS",
  "call-log": "통화",
  "call-transcript": "전사",
  upload: "Upload",
};

/**
 * CSS class names for source type badges.
 * Each class is defined in globals.css @layer components as .badge-{source}.
 * Dark mode is handled inside the CSS class (prefers-color-scheme media query),
 * so no dark: Tailwind prefix is needed here.
 */
export const SOURCE_BADGE_CLASSES: Record<SourceType, string> = {
  sms: "badge-sms",
  "call-log": "badge-call-log",
  "call-transcript": "badge-call-transcript",
  gmail: "badge-gmail",
  calendar: "badge-calendar",
  filesystem: "badge-filesystem",
  upload: "badge-upload",
  slack: "badge-slack",
  github: "badge-github",
  "llm-memory": "badge-llm-memory",
  secretary: "badge-secretary",
  gdrive: "badge-gdrive",
  notion: "badge-notion",
  telegram: "badge-telegram",
  discord: "badge-discord",
};

/** Source types shown as filter options in the search UI. */
export const SEARCH_FILTER_SOURCES: (SourceType | "all")[] = [
  "all",
  "sms",
  "call-transcript",
  "gmail",
  "calendar",
  "filesystem",
  "upload",
];

/** Source types excluded from the "all" filter in search (noisy/low-value). */
export const DEFAULT_EXCLUDED_SOURCES: SourceType[] = ["slack"];

/** Source types shown in the dashboard stats grid. */
export const DASHBOARD_SOURCES: SourceType[] = [
  "sms",
  "call-log",
  "call-transcript",
  "gmail",
  "calendar",
  "filesystem",
  "upload",
  "github",
];

/** Cutover date boundary (documents collected after this date use new pipeline) */
export const CUTOVER_DATE = "2026-05-30";
