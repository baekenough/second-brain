import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { expect, it } from "vitest";
import { MarkdownContent } from "./MarkdownContent";

it("only links answer citations to the current retrieved document IDs", () => {
  const id = "11111111-1111-1111-1111-111111111111";
  const unknown = "22222222-2222-2222-2222-222222222222";
  const html = renderToStaticMarkup(createElement(MarkdownContent, {
    source: `[known](/documents/${id}) [unknown](/documents/${unknown}) [external](https://example.com)`,
    citationIds: [id],
  }));
  expect(html).toContain(`href="/documents/${id}"`);
  expect(html).not.toContain(`href="/documents/${unknown}"`);
  expect(html).not.toContain('href="https://example.com"');
  expect(html).toContain("unknown");
});

it("preserves normal document links when citation restrictions are not requested", () => {
  const html = renderToStaticMarkup(createElement(MarkdownContent, {
    source: "[reference](https://example.com)",
  }));
  expect(html).toContain('href="https://example.com"');
});
