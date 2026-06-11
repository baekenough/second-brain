import * as React from "react";
import { cn } from "@/lib/utils";
import type { SourceType } from "@/lib/types";
import { SOURCE_LABELS, SOURCE_BADGE_CLASSES } from "@/lib/constants";

// ── Generic badge (color variants) ─────────────────────────────────────────

const VARIANT = {
  default: "bg-surface-subtle text-foreground-muted border border-border",
  accent: "bg-accent-subtle text-text-accent border border-accent/20",
  success: "bg-[--status-success-light] text-[--status-success]",
  warning: "bg-[--status-warning-light] text-[--status-warning]",
  danger: "bg-[--status-danger-light]  text-danger",
} as const;

const SIZE = {
  sm: "px-1.5 py-0    text-xs  rounded",
  md: "px-2   py-0.5  text-xs  rounded",
  lg: "px-2.5 py-1    text-sm  rounded-md",
} as const;

export interface BadgeProps extends React.HTMLAttributes<HTMLSpanElement> {
  variant?: keyof typeof VARIANT;
  size?: keyof typeof SIZE;
}

export function Badge({
  variant = "default",
  size = "md",
  className,
  children,
  ...props
}: BadgeProps) {
  return (
    <span
      className={cn(
        "inline-flex items-center leading-none font-medium",
        VARIANT[variant],
        SIZE[size],
        className,
      )}
      {...props}
    >
      {children}
    </span>
  );
}
Badge.displayName = "Badge";

// ── Source type badge (uses .badge-* CSS classes from globals.css) ──────────

export interface SourceBadgeProps extends React.HTMLAttributes<HTMLSpanElement> {
  sourceType: SourceType;
  size?: keyof typeof SIZE;
  /** Override the label (defaults to SOURCE_LABELS[sourceType]) */
  label?: string;
}

export function SourceBadge({
  sourceType,
  size = "md",
  label,
  className,
  ...props
}: SourceBadgeProps) {
  const badgeClass = SOURCE_BADGE_CLASSES[sourceType] ?? "badge-default";
  const displayLabel = label ?? SOURCE_LABELS[sourceType];

  return (
    <span
      className={cn(
        "inline-flex items-center leading-none font-medium",
        SIZE[size],
        badgeClass,
        className,
      )}
      {...props}
    >
      {displayLabel}
    </span>
  );
}
SourceBadge.displayName = "SourceBadge";
