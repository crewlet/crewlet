/**
 * The doc templates read as the outlines they are: every list marker drawn as
 * a list, and no line of Markdown left showing as text.
 */

import { render } from "@testing-library/react";
import { expect, test } from "vitest";
import { renderMarkdown } from "~/lib/markdown.ts";
import { DOC_TEMPLATES, docTemplate } from "./docTemplates.ts";

test.each(DOC_TEMPLATES.map((t) => [t.key, t] as const))(
  "the %s template draws its sections and lists, and no marker as text",
  (_key, t) => {
    const { container } = render(<div>{renderMarkdown(t.body)}</div>);
    const markers = t.body.split("\n").filter((line) => /^(\d+\.|-) /.test(line)).length;
    // A DOC'S SECTION HEADINGS sit one level under its title.
    expect(container.querySelectorAll("h3").length).toBeGreaterThan(0);
    expect(container.querySelectorAll("li").length).toBe(markers);
    // AN EMPTY ITEM ("1. ") IS NOT A LIST ITEM in the grammar, and drew as
    // "1. 2. 3." inside a paragraph.
    for (const p of container.querySelectorAll("p")) {
      expect(p.textContent).not.toMatch(/^\s*(\d+\.|-)(\s|$)/);
    }
  },
);

test("every template is named once and found by its key", () => {
  const keys = DOC_TEMPLATES.map((t) => t.key);
  expect(new Set(keys).size).toBe(keys.length);
  for (const t of DOC_TEMPLATES) expect(docTemplate(t.key)).toBe(t);
  expect(docTemplate("nope")).toBeUndefined();
});
