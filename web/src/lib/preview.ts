const IMAGE_EXTS = new Set([".jpg", ".jpeg", ".png", ".gif", ".webp", ".svg"]);
const MARKDOWN_EXTS = new Set([".md", ".markdown"]);
const TEXT_EXTS = new Set([".txt", ".log", ".yaml", ".yml", ".json", ".csv"]);

export type PreviewKind = "image" | "markdown" | "text" | "none";

export function getExtension(sourceId: string, metadata?: { ext?: string }): string {
  if (metadata?.ext) return metadata.ext.toLowerCase();
  const idx = sourceId.lastIndexOf(".");
  return idx >= 0 ? sourceId.slice(idx).toLowerCase() : "";
}

export function getPreviewKind(ext: string): PreviewKind {
  if (IMAGE_EXTS.has(ext)) return "image";
  if (MARKDOWN_EXTS.has(ext)) return "markdown";
  if (TEXT_EXTS.has(ext)) return "text";
  return "none";
}

export function rawUrl(id: string): string {
  return `/api/documents/${encodeURIComponent(id)}/raw`;
}
