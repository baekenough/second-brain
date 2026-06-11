import * as React from "react";
import { cn } from "@/lib/utils";
import { Spinner } from "./Spinner";

// ── Variant & size maps ─────────────────────────────────────────────────────

const VARIANT = {
  primary:
    "bg-accent text-white hover:bg-accent-hover active:opacity-90 disabled:bg-accent/40 disabled:text-white/60",
  secondary:
    "border border-border bg-surface text-foreground hover:bg-surface-subtle active:bg-surface-subtle/80 disabled:opacity-40",
  ghost:
    "text-foreground-muted hover:bg-surface-subtle hover:text-foreground active:bg-surface-subtle/80 disabled:opacity-40",
  destructive: "bg-danger text-white hover:opacity-90 active:opacity-80 disabled:opacity-40",
} as const;

// py-2.5 is the canonical padding for medium buttons (per design-system.md)
const SIZE = {
  sm: "h-8  px-3    text-xs  gap-1.5 rounded-md",
  md: "h-10 px-4    py-2.5 text-sm  gap-2   rounded-lg",
  lg: "h-12 px-5    text-base gap-2   rounded-lg",
  icon: "h-9  w-9   text-sm  rounded-lg justify-center",
} as const;

// ── Types ───────────────────────────────────────────────────────────────────

export interface ButtonProps extends React.ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: keyof typeof VARIANT;
  size?: keyof typeof SIZE;
  loading?: boolean;
  /** Left-slot icon (rendered before children) */
  icon?: React.ReactNode;
}

// ── Component ───────────────────────────────────────────────────────────────

export const Button = React.forwardRef<HTMLButtonElement, ButtonProps>(
  (
    {
      variant = "primary",
      size = "md",
      loading = false,
      icon,
      disabled,
      className,
      children,
      ...props
    },
    ref,
  ) => {
    const isDisabled = disabled || loading;

    return (
      <button
        ref={ref}
        disabled={isDisabled}
        aria-busy={loading}
        className={cn(
          // Base
          "inline-flex items-center font-medium transition-colors focus-visible:outline-none",
          "focus-visible:ring-2 focus-visible:ring-accent/50 focus-visible:ring-offset-1",
          "cursor-pointer select-none disabled:cursor-not-allowed",
          VARIANT[variant],
          SIZE[size],
          className,
        )}
        {...props}
      >
        {loading ? (
          <Spinner
            size="sm"
            className={variant === "primary" ? "text-white/80" : "text-foreground-muted"}
          />
        ) : (
          icon
        )}
        {children}
      </button>
    );
  },
);

Button.displayName = "Button";
