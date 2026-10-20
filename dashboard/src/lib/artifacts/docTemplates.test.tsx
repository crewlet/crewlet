/**
 * The doc templates: each named once, found by its key, and an outline of
 * sections. That each one draws its lists as lists is held with every other
 * template's in `lib/markdownTemplates.test.tsx`.
 */

import { render } from "@testing-library/react";
import { expect, test } from "vitest";
import { renderMarkdown } from "~/lib/markdown.ts";
import { DOC_TEMPLATES, docTemplate } from "./docTemplates.ts";

test("every template is named once, found by its key and drawn as sections", () => {
  const keys = DOC_TEMPLATES.map((t) => t.key);
  expect(new Set(keys).size).toBe(keys.length);
  for (const t of DOC_TEMPLATES) {
    expect(docTemplate(t.key)).toBe(t);
    const { container, unmount } = render(<div>{renderMarkdown(t.body)}</div>);
    // A SECTION'S "##" draws one level under the doc's own title.
    expect(container.querySelectorAll("h3").length).toBeGreaterThan(0);
    unmount();
  }
  expect(docTemplate("nope")).toBeUndefined();
});
