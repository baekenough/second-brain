/**
 * Extract a short human-readable summary from raw document content.
 * Strategy (in order):
 *  1. If content is TSV-like (starts with ##SHEET), return sheet count + row count hint
 *  2. Strip markdown syntax (headers, lists, links) from first non-empty lines
 *  3. Take first 2-3 non-empty, non-header lines
 *  4. Truncate to `maxChars` with ellipsis
 */
export function extractSummary(content: string, maxChars = 180): string {
  if (!content) return "";
  const trimmed = content.trim();

  // Detect xlsx TSV output
  if (trimmed.startsWith("##SHEET")) {
    const lines = trimmed.split("\n");
    const sheets = lines
      .filter((l) => l.startsWith("##SHEET "))
      .map((l) => l.slice(8).trim());
    const dataLines = lines.filter(
      (l) => l && !l.startsWith("##SHEET ")
    ).length;
    const head = sheets.length > 0 ? `시트 ${sheets.length}개` : "";
    return [head, `약 ${dataLines}행`].filter(Boolean).join(" · ");
  }

  // Plain text / markdown
  const lines = trimmed.split("\n");
  const meaningful: string[] = [];
  for (const raw of lines) {
    let line = raw.trim();
    if (!line) continue;
    // Strip leading markdown symbols
    line = line.replace(/^#{1,6}\s*/, ""); // headers
    line = line.replace(/^[-*+]\s+/, ""); // list bullets
    line = line.replace(/^>\s+/, ""); // blockquote
    // Remove basic inline markdown
    line = line.replace(/\*\*(.+?)\*\*/g, "$1"); // bold
    line = line.replace(/\*(.+?)\*/g, "$1"); // italic
    line = line.replace(/`(.+?)`/g, "$1"); // code
    line = line.replace(/\[(.+?)\]\([^)]+\)/g, "$1"); // links → text
    line = line.replace(/!\[.*?\]\([^)]+\)/g, ""); // images
    line = line.trim();
    if (!line) continue;
    meaningful.push(line);
    if (meaningful.length >= 3) break;
  }
  const joined = meaningful.join(" · ");
  if (joined.length <= maxChars) return joined;
  return joined.slice(0, maxChars - 1).trimEnd() + "…";
}
