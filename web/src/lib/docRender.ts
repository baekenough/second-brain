export type RenderKind = "image" | "markdown" | "xlsx" | "text" | "code";

export function getRenderKind(ext: string): RenderKind {
  const lower = ext.toLowerCase();
  if ([".jpg", ".jpeg", ".png", ".gif", ".webp", ".svg"].includes(lower))
    return "image";
  if (lower === ".md" || lower === ".markdown") return "markdown";
  if (lower === ".xlsx" || lower === ".xls") return "xlsx";
  if ([".txt", ".log"].includes(lower)) return "text";
  return "code";
}
