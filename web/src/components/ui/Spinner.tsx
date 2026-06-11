import * as React from "react";
import { cn } from "@/lib/utils";

const SIZE = {
  xs: "h-3   w-3",
  sm: "h-4   w-4",
  md: "h-5   w-5",
  lg: "h-6   w-6",
  xl: "h-8   w-8",
} as const;

export interface SpinnerProps extends React.SVGAttributes<SVGSVGElement> {
  size?: keyof typeof SIZE;
  /** Screen-reader label */
  label?: string;
}

export function Spinner({
  size = "md",
  label = "불러오는 중…",
  className,
  ...props
}: SpinnerProps) {
  return (
    <svg
      role="status"
      aria-label={label}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={2.5}
      strokeLinecap="round"
      className={cn("shrink-0 animate-spin", SIZE[size], className)}
      {...props}
    >
      {/* Background track */}
      <circle cx={12} cy={12} r={9} strokeOpacity={0.25} />
      {/* Animated arc */}
      <path d="M12 3a9 9 0 0 1 9 9" strokeOpacity={0.85} />
    </svg>
  );
}
