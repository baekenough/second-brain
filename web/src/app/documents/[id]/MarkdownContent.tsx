"use client";

import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import rehypeHighlight from "rehype-highlight";

export function MarkdownContent({ source }: { source: string }) {
  return (
    <article className="prose prose-sm md:prose dark:prose-invert max-w-none prose-pre:rounded-lg prose-pre:my-3 prose-code:before:content-none prose-code:after:content-none">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        rehypePlugins={[rehypeHighlight]}
      >
        {source}
      </ReactMarkdown>
    </article>
  );
}
