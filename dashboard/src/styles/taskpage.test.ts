// @vitest-environment node
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "vitest";

import { TASK_RAIL_STACKS } from "~/app/layout.ts";

/**
 * The task page's layout rules that jsdom cannot see: it computes no layout and
 * no contrast, so every suite that renders the page stays green through each of
 * these. Measured in a browser at 390 when they were written.
 */

const STYLES = fileURLToPath(new URL(".", import.meta.url));
const screens = readFileSync(join(STYLES, "screens.css"), "utf8").replace(/\/\*[\s\S]*?\*\//g, "");

/** Every declaration block of the rules whose selector list names `selector`. */
function rules(selector: string): string[] {
  const out: string[] = [];
  for (const m of screens.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
    const names = m[1]!.split(",").map((s) => s.trim());
    if (names.includes(selector)) out.push(m[2]!);
  }
  return out;
}

function declared(selector: string, property: string): string | undefined {
  for (const body of rules(selector)) {
    const m = new RegExp(`(?:^|;|\\s)${property}\\s*:\\s*([^;]+);`).exec(body);
    if (m) return m[1]!.trim();
  }
  return undefined;
}

describe("the description is bounded by its column", () => {
  // SIZED TO ITS CONTENT the prose grew to a wide table's minimum, 636px on a
  // 390px window, with nothing able to scroll to the rest.
  test("the body stretches the prose to its width", () => {
    expect(declared(".task-body", "align-items")).toBe("stretch");
  });

  test("the edit control still hugs its own words", () => {
    expect(declared(".task-body > .task-edit", "align-self")).toBe("flex-start");
  });

  test("a table scrolls inside its own box", () => {
    expect(declared(".prose.md .md-table", "overflow-x")).toBe("auto");
  });
});

describe("the edit control passes contrast at rest", () => {
  // OPACITY COMPOSITES THE GHOST'S INK: 2.81:1 in light and 3.42:1 in dark, and
  // only hover or focus brought it back, which a touch reader never does.
  test("no rule dims it", () => {
    for (const selector of [".task-edit", ".task-body > .task-edit"]) {
      expect(declared(selector, "opacity"), selector).toBeUndefined();
    }
    expect(screens).not.toMatch(/\.task-edit[^{}]*\{[^{}]*opacity/);
  });
});

describe("the task's words sit on the kit's type scale", () => {
  test("the title takes the size and line-height tokens", () => {
    expect(declared(".task-title", "font-size")).toBe("var(--font-size-2xl)");
    expect(declared(".task-title", "line-height")).toBe("var(--font-line-height-tight)");
  });

  // ONE LEADING FOR RENDERED MARKDOWN: `.prose`'s relaxed token, which a comment
  // and an answer already take. A local 1.65 put a description off the scale.
  test("the description takes the leading every Markdown surface takes", () => {
    expect(declared(".task-prose", "line-height")).toBeUndefined();
    expect(declared(".prose", "line-height")).toBe("var(--font-line-height-relaxed)");
  });
});

describe("the page and the sheet agree on where the rail stacks", () => {
  // THE PAGE DRAWS THE LEAD BY THIS NUMBER AND THE SHEET STACKS BY THE LITERAL: a
  // gap between them is a width with the fields drawn twice, or nowhere.
  test("the container query that stacks the frame is the page's own constant", () => {
    const at = screens.indexOf(`@container page (max-width: ${TASK_RAIL_STACKS}px)`);
    expect(at, "the stacking query is not at the page's width").toBeGreaterThan(-1);
    expect(screens.slice(at)).toMatch(/^[^{]*\{\s*\.task-frame\s*\{\s*display: block;/);
  });
});
