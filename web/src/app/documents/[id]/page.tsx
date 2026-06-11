import type { Metadata } from "next";
import Link from "next/link";
import { notFound } from "next/navigation";
import { getDocument } from "@/lib/api";
import { formatDateTime, formatShortDate } from "@/lib/dates";
import { getExtension, rawUrl } from "@/lib/preview";
import { getRenderKind } from "@/lib/docRender";
import { toMarkdownSource } from "@/lib/codeWrap";
import { SourceBadge } from "@/components/ui";
import type { DocumentDetail } from "@/lib/types";
import { MarkdownContent } from "./MarkdownContent";
import XlsxTable from "./XlsxTable";

interface PageProps {
  params: Promise<{ id: string }>;
}

export async function generateMetadata({ params }: PageProps): Promise<Metadata> {
  const { id } = await params;
  try {
    const doc = await getDocument(id);
    return { title: `${doc.title} — Second Brain` };
  } catch {
    return { title: "Document — Second Brain" };
  }
}

/**
 * Parse speaker-labelled transcript segments from content.
 * Supports the format used by the diarization pipeline (#111):
 *   [화자1] text...
 *   [화자2] other text...
 */
function parseTranscriptSegments(content: string): { speaker: string; text: string }[] | null {
  const speakerRe = /^\[(화자\d+|Speaker\s*\d+|발신자|수신자)\]\s*/i;
  const lines = content.split("\n").filter((l) => l.trim());
  const segments = lines.map((line) => {
    const match = speakerRe.exec(line);
    if (!match) return null;
    return { speaker: match[1] ?? "", text: line.slice(match[0].length).trim() };
  });
  const valid = segments.filter(Boolean) as { speaker: string; text: string }[];
  if (valid.length === 0 || valid.length < lines.length * 0.5) return null;
  return valid;
}

const SPEAKER_COLOR_MAP: Record<string, string> = {
  화자1: "text-accent font-medium",
  발신자: "text-accent font-medium",
  화자2: "text-foreground-muted",
  수신자: "text-foreground-muted",
};

function TranscriptView({ content }: { content: string }) {
  const segments = parseTranscriptSegments(content);

  if (!segments) {
    return (
      <div className="space-y-1 text-sm leading-relaxed text-foreground">
        {content
          .split("\n")
          .filter((l) => l.trim())
          .map((line, i) => (
            <p key={i}>{line}</p>
          ))}
      </div>
    );
  }

  return (
    <div className="space-y-2">
      {segments.map((seg, i) => (
        <div key={i} className="flex gap-3">
          <span
            className={`mt-0.5 w-16 shrink-0 text-xs ${SPEAKER_COLOR_MAP[seg.speaker] ?? "text-foreground-subtle"}`}
          >
            {seg.speaker}
          </span>
          <p className="text-sm leading-relaxed text-foreground">{seg.text}</p>
        </div>
      ))}
    </div>
  );
}

function DocumentContent({ doc }: { doc: DocumentDetail }) {
  const ext = getExtension(doc.source_id ?? doc.id, doc.metadata);
  const renderKind = getRenderKind(ext);

  if (doc.source_type === "call-transcript") {
    return doc.content ? (
      <TranscriptView content={doc.content} />
    ) : (
      <p className="text-sm text-foreground-subtle italic">(전사 없음)</p>
    );
  }

  if (renderKind === "image") {
    return (
      <img
        src={rawUrl(doc.id)}
        alt={doc.title}
        className="max-w-full rounded-lg border border-border"
      />
    );
  }

  if (renderKind === "xlsx") {
    return doc.content ? (
      <XlsxTable content={doc.content} />
    ) : (
      <p className="text-sm text-foreground-subtle italic">(본문 없음)</p>
    );
  }

  if (renderKind === "text") {
    return doc.content ? (
      <pre className="overflow-x-auto rounded-lg bg-surface-subtle p-4 font-mono text-sm leading-relaxed text-foreground">
        {doc.content}
      </pre>
    ) : (
      <p className="text-sm text-foreground-subtle italic">(본문 없음)</p>
    );
  }

  // markdown, code, default
  return doc.content ? (
    <MarkdownContent source={toMarkdownSource(doc.content, ext)} />
  ) : (
    <p className="text-sm text-foreground-subtle italic">(본문 없음)</p>
  );
}

export default async function DocumentPage({ params }: PageProps) {
  const { id } = await params;

  let doc: DocumentDetail;
  try {
    doc = await getDocument(id);
  } catch {
    notFound();
  }

  const sourceUrl = doc.metadata.source_url as string | undefined;
  const occurredAt =
    (doc.metadata.occurred_at as string | undefined) ??
    (doc.metadata.occurred_at as string | undefined);

  return (
    <article className="space-y-6">
      {/* Back link */}
      <Link
        href="/"
        className="inline-flex items-center gap-1 text-sm text-foreground-muted transition-colors hover:text-foreground"
      >
        ← 검색으로 돌아가기
      </Link>

      {/* Header */}
      <header className="space-y-3">
        <div className="flex flex-wrap items-center gap-2">
          <SourceBadge sourceType={doc.source_type} />
          {doc.status && doc.status !== "active" && (
            <span className="inline-flex items-center rounded bg-warning/10 px-2 py-0.5 text-xs font-medium text-warning">
              {doc.status}
            </span>
          )}
          {sourceUrl && (
            <a
              href={sourceUrl}
              target="_blank"
              rel="noopener noreferrer"
              className="text-xs text-foreground-muted underline underline-offset-2 transition-colors hover:text-foreground"
            >
              원본 보기 →
            </a>
          )}
        </div>

        <h1 className="font-serif text-2xl leading-snug font-semibold text-foreground">
          {doc.title}
        </h1>

        <div className="flex flex-wrap gap-4 text-xs text-foreground-subtle">
          {occurredAt && <span>발생 {formatShortDate(occurredAt)}</span>}
          <span>수집 {formatDateTime(doc.collected_at)}</span>
          {doc.created_at && <span>생성 {formatDateTime(doc.created_at)}</span>}
        </div>
      </header>

      {/* Content */}
      <div className="rounded-lg border border-border bg-surface p-6">
        <DocumentContent doc={doc} />
      </div>

      {/* Metadata */}
      {Object.keys(doc.metadata).length > 0 && (
        <details className="group rounded-lg border border-border bg-surface-subtle">
          <summary className="cursor-pointer px-4 py-3 text-xs font-medium text-foreground-muted select-none group-open:border-b group-open:border-border">
            메타데이터
          </summary>
          <dl className="divide-y divide-border px-4">
            <div className="flex gap-3 py-1.5">
              <dt className="w-28 shrink-0 text-xs text-foreground-subtle">ID</dt>
              <dd className="text-xs break-all text-foreground-muted">{doc.id}</dd>
            </div>
            {doc.source_id && (
              <div className="flex gap-3 py-1.5">
                <dt className="w-28 shrink-0 text-xs text-foreground-subtle">Source ID</dt>
                <dd className="text-xs break-all text-foreground-muted">{doc.source_id}</dd>
              </div>
            )}
            {Object.entries(doc.metadata)
              .filter(([, v]) => v !== undefined && v !== "")
              .map(([key, value]) => (
                <div key={key} className="flex gap-3 py-1.5">
                  <dt className="w-28 shrink-0 text-xs text-foreground-subtle">{key}</dt>
                  <dd className="text-xs break-all text-foreground-muted">{String(value)}</dd>
                </div>
              ))}
          </dl>
        </details>
      )}
    </article>
  );
}
