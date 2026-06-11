import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

/**
 * Compose Tailwind CSS class names safely.
 * - clsx: conditionals, arrays, objects
 * - twMerge: deduplicates conflicting Tailwind utilities (e.g. px-2 + px-4 → px-4)
 */
export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs));
}
