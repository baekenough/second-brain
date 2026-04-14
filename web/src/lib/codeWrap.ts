// Map filesystem extension to fenced-code language hint for rehype-highlight.
const CODE_EXT_LANGS: Record<string, string> = {
  ".json": "json",
  ".yaml": "yaml",
  ".yml": "yaml",
  ".toml": "toml",
  ".ini": "ini",
  ".csv": "csv",
  ".tsv": "csv",
  ".xml": "xml",
  ".html": "html",
  ".htm": "html",
  ".css": "css",
  ".scss": "scss",
  ".js": "javascript",
  ".jsx": "javascript",
  ".ts": "typescript",
  ".tsx": "typescript",
  ".mjs": "javascript",
  ".cjs": "javascript",
  ".py": "python",
  ".go": "go",
  ".rs": "rust",
  ".java": "java",
  ".kt": "kotlin",
  ".swift": "swift",
  ".rb": "ruby",
  ".sh": "bash",
  ".bash": "bash",
  ".zsh": "bash",
  ".sql": "sql",
  ".dockerfile": "dockerfile",
  ".env": "bash",
};

export function toMarkdownSource(content: string, ext: string): string {
  const lower = ext.toLowerCase();

  // Markdown passthrough
  if (lower === ".md" || lower === ".markdown") return content;

  // Plain text (no fencing — still rendered via prose)
  if (lower === ".txt" || lower === ".log") return content;

  // Code formats → fenced block
  const lang = CODE_EXT_LANGS[lower];
  if (lang) {
    // Pretty-print JSON specifically for readability
    if (lang === "json") {
      try {
        const parsed = JSON.parse(content) as unknown;
        content = JSON.stringify(parsed, null, 2);
      } catch {
        // leave as-is if invalid JSON
      }
    }
    return "```" + lang + "\n" + content + "\n```";
  }

  // Unknown: wrap as plain fenced block for uniform look
  return "```\n" + content + "\n```";
}
