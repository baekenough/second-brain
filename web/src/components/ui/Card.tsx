import * as React from "react";
import { cn } from "@/lib/utils";

// ── Variant & padding maps ───────────────────────────────────────────────────

const VARIANT = {
  default: "border border-border bg-surface",
  subtle: "border border-border bg-surface-subtle",
  outlined: "border-2 border-border bg-transparent",
  ghost: "bg-transparent",
} as const;

const PADDING = {
  none: "",
  sm: "p-3",
  md: "p-4",
  lg: "p-6",
} as const;

const RADIUS = {
  md: "rounded-lg",
  lg: "rounded-xl",
  xl: "rounded-2xl",
} as const;

// ── Types ────────────────────────────────────────────────────────────────────

export interface CardProps extends React.HTMLAttributes<HTMLDivElement> {
  variant?: keyof typeof VARIANT;
  padding?: keyof typeof PADDING;
  radius?: keyof typeof RADIUS;
  /** If true, applies hover state styles */
  interactive?: boolean;
}

// ── Subcomponents ────────────────────────────────────────────────────────────

export type CardHeaderProps = React.HTMLAttributes<HTMLDivElement>;

export function CardHeader({ className, children, ...props }: CardHeaderProps) {
  return (
    <div
      className={cn("flex items-start justify-between border-b border-border px-4 py-3", className)}
      {...props}
    >
      {children}
    </div>
  );
}
CardHeader.displayName = "CardHeader";

export interface CardBodyProps extends React.HTMLAttributes<HTMLDivElement> {
  padding?: keyof typeof PADDING;
}

export function CardBody({ padding = "md", className, children, ...props }: CardBodyProps) {
  return (
    <div className={cn(PADDING[padding], className)} {...props}>
      {children}
    </div>
  );
}
CardBody.displayName = "CardBody";

export type CardFooterProps = React.HTMLAttributes<HTMLDivElement>;

export function CardFooter({ className, children, ...props }: CardFooterProps) {
  return (
    <div className={cn("border-t border-border px-4 py-3", className)} {...props}>
      {children}
    </div>
  );
}
CardFooter.displayName = "CardFooter";

// ── Root Card ────────────────────────────────────────────────────────────────

export const Card = React.forwardRef<HTMLDivElement, CardProps>(
  (
    {
      variant = "default",
      padding = "md",
      radius = "lg",
      interactive = false,
      className,
      children,
      ...props
    },
    ref,
  ) => (
    <div
      ref={ref}
      className={cn(
        "overflow-hidden",
        VARIANT[variant],
        RADIUS[radius],
        interactive && "cursor-pointer transition-shadow hover:shadow-md active:shadow-sm",
        // Only apply padding at root level if no subcomponents (CardHeader/Body/Footer)
        padding !== "none" && PADDING[padding],
        className,
      )}
      {...props}
    >
      {children}
    </div>
  ),
);
Card.displayName = "Card";
