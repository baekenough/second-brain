"use client";

import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import rehypeHighlight from "rehype-highlight";

export function MarkdownContent({ source, citationIds }: { source: string; citationIds?: readonly string[] }) {
  return (
    <article className="prose prose-sm md:prose dark:prose-invert max-w-none prose-pre:rounded-lg prose-pre:my-3 prose-code:before:content-none prose-code:after:content-none">
      <ReactMarkdown
        components={citationIds ? { a: ({ href, children }) => {
          const id = href?.match(/^\/documents\/([0-9a-f-]+)$/i)?.[1];
          return id && citationIds.includes(id)
            ? <a href={href}>{children}</a>
            : <span title="현재 답변의 근거 목록에 없는 링크">{children}</span>;
        } } : undefined}
        remarkPlugins={[remarkGfm]}
        rehypePlugins={[rehypeHighlight]}
      >
        {source}
      </ReactMarkdown>
    </article>
  );
}
