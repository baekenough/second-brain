import { getDocument } from "@/lib/api";
import type { SourceType } from "@/lib/types";
import { formatDateTime } from "@/lib/dates";
import { getExtension, rawUrl } from "@/lib/preview";
import { toMarkdownSource } from "@/lib/codeWrap";
import { getRenderKind } from "@/lib/docRender";
import { MarkdownContent } from "./MarkdownContent";
import XlsxTable from "./XlsxTable";

interface DocumentPageProps {
  params: Promise<{ id: string }>;
}

const SOURCE_BADGE_STYLES: Record<SourceType, string> = {
  slack:
    "bg-purple-100 text-purple-700 dark:bg-purple-900/40 dark:text-purple-300",
  github: "bg-gray-100 text-gray-700 dark:bg-gray-800 dark:text-gray-300",
  filesystem:
    "bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300",
};

const SOURCE_LABELS: Record<SourceType, string> = {
  slack: "Slack",
  github: "GitHub",
  filesystem: "Drive",
};

export default async function DocumentPage({ params }: DocumentPageProps) {
  const { id } = await params;

  let document;
  try {
    document = await getDocument(id);
  } catch {
    return (
      <div className="space-y-4">
        <a
          href="/"
          className="text-sm text-gray-500 dark:text-gray-400 hover:text-gray-700 dark:hover:text-gray-200 transition-colors"
        >
          ← 검색으로 돌아가기
        </a>
        <p className="text-sm text-red-500 dark:text-red-400">
          문서를 불러올 수 없습니다.
        </p>
      </div>
    );
  }

  const sourceUrl = document.metadata?.source_url;
  const ext = getExtension(id, document.metadata);
  const fileRawUrl = rawUrl(id);
  const renderKind = getRenderKind(ext);

  return (
    <div className="space-y-6">
      <a
        href="/"
        className="text-sm text-gray-500 dark:text-gray-400 hover:text-gray-700 dark:hover:text-gray-200 transition-colors"
      >
        ← 검색으로 돌아가기
      </a>

      <div className="space-y-3">
        <h1 className="text-xl font-semibold text-gray-900 dark:text-gray-100 leading-snug">
          {document.title}
        </h1>

        <div className="flex items-center gap-3 flex-wrap">
          <span
            className={`inline-flex items-center px-2 py-0.5 rounded text-xs font-medium ${SOURCE_BADGE_STYLES[document.source_type]}`}
          >
            {SOURCE_LABELS[document.source_type]}
          </span>
          {sourceUrl && (
            <a
              href={sourceUrl}
              target="_blank"
              rel="noopener noreferrer"
              className="text-xs text-gray-500 dark:text-gray-400 underline underline-offset-2 hover:text-gray-700 dark:hover:text-gray-200 transition-colors"
            >
              원본 보기 →
            </a>
          )}
        </div>

        <dl className="mt-4 grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-xs text-gray-500 dark:text-gray-400">
          <dt className="font-medium">수정</dt>
          <dd>{formatDateTime(document.collected_at)}</dd>
          {document.created_at && (
            <>
              <dt className="font-medium">생성</dt>
              <dd>{formatDateTime(document.created_at)}</dd>
            </>
          )}
          {document.updated_at && (
            <>
              <dt className="font-medium">수집</dt>
              <dd>{formatDateTime(document.updated_at)}</dd>
            </>
          )}
        </dl>
      </div>

      <hr className="border-gray-200 dark:border-gray-800" />

      <div>
        {renderKind === "image" ? (
          <img
            src={fileRawUrl}
            alt={document.title}
            className="max-w-full rounded"
          />
        ) : renderKind === "markdown" ? (
          document.content ? (
            <MarkdownContent source={document.content} />
          ) : (
            <p className="text-sm text-gray-400 dark:text-gray-500 italic">
              (본문 없음)
            </p>
          )
        ) : renderKind === "xlsx" ? (
          document.content ? (
            <XlsxTable content={document.content} />
          ) : (
            <p className="text-sm text-gray-400 dark:text-gray-500 italic">
              (본문 없음)
            </p>
          )
        ) : renderKind === "text" ? (
          document.content ? (
            <pre className="font-mono text-sm whitespace-pre-wrap p-4 rounded bg-gray-50 dark:bg-gray-900 overflow-x-auto leading-relaxed">
              {document.content}
            </pre>
          ) : (
            <p className="text-sm text-gray-400 dark:text-gray-500 italic">
              (본문 없음)
            </p>
          )
        ) : document.content ? (
          <MarkdownContent source={toMarkdownSource(document.content, ext)} />
        ) : (
          <p className="text-sm text-gray-400 dark:text-gray-500 italic">
            (본문 없음)
          </p>
        )}
      </div>

      <div className="flex items-center gap-4 pt-2">
        <a
          href={fileRawUrl}
          target="_blank"
          rel="noopener noreferrer"
          className="text-xs text-gray-500 dark:text-gray-400 underline underline-offset-2 hover:text-gray-700 dark:hover:text-gray-200 transition-colors"
        >
          원본 파일 보기 →
        </a>
      </div>

      {Object.keys(document.metadata).length > 0 && (
        <div className="border border-gray-200 dark:border-gray-800 rounded-lg p-4 space-y-2">
          <h2 className="text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wide">
            메타데이터
          </h2>
          <dl className="grid grid-cols-2 gap-x-4 gap-y-1">
            {Object.entries(document.metadata)
              .filter(([, v]) => v !== undefined && v !== "")
              .map(([key, value]) => (
                <div key={key} className="contents">
                  <dt className="text-xs text-gray-400 dark:text-gray-500 truncate">
                    {key}
                  </dt>
                  <dd className="text-xs text-gray-700 dark:text-gray-300 truncate">
                    {String(value)}
                  </dd>
                </div>
              ))}
          </dl>
        </div>
      )}
    </div>
  );
}
