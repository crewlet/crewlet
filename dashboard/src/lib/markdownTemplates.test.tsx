/**
 * Every Markdown template a writer starts from reads as the outline it is:
 * each list marker drawn as a list item, and no marker left showing as text.
 *
 * A BARE MARKER IS NOT A LIST ITEM in the grammar: "1." with no space after
 * it reads as text, so a template whose trailing spaces were lost drew
 * "1. 2. 3." inside a paragraph, in the editor and on the page alike. An
 * empty item keeps its space ("1. "), or better, says what goes there.
 */

import { render } from "@testing-library/react";
import { expect, test } from "vitest";
import { renderMarkdown } from "./markdown.ts";
import { DOC_TEMPLATES } from "./artifacts/docTemplates.ts";
import { TEMPLATES } from "~/routes/doclet/Composer.tsx";

const corpus = [
  ...TEMPLATES.filter((t) => t.body !== "").map((t) => [`knowledge ${t.value}`, t.body] as const),
  ...DOC_TEMPLATES.map((t) => [`doc ${t.key}`, t.body] as const),
];

test.each(corpus)("the %s template draws every list marker as a list item", (_name, body) => {
  const { container } = render(<div>{renderMarkdown(body)}</div>);
  const markers = body.split("\n").filter((line) => /^(\d+\.|-)(\s|$)/.test(line)).length;
  expect(container.querySelectorAll("li").length).toBe(markers);
  for (const p of container.querySelectorAll("p")) {
    expect(p.textContent).not.toMatch(/^\s*(\d+\.|-)(\s|$)/);
  }
});
