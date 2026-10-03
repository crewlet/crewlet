import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { join } from "node:path";
import { cleanup, render } from "~/test/inCase.ts";
import { afterEach, expect, test } from "vitest";
import { StatCard, StatGroup } from "@crewlethq/ui";

/**
 * A ROW OF STAT TILES SHARES ITS LINES, so no label can push its figure below
 * the others.
 *
 * uilet stacks a tile's label, number and sub in a flex column of its own, so
 * the lines of one tile know nothing about the tile beside it: beside a node's
 * peek at 1440, Nodes' "Behind on config" broke over two lines and its figure
 * sat 18px below the other three. `screens.css` makes each tile in a group a
 * SUBGRID over the group's rows, one row per line it renders, so a line is as
 * tall as the tallest of it across the row and every figure starts together.
 *
 * Two halves, because the rule is only right while they agree: the span is a
 * number written in a stylesheet, and the number of lines is what the kit's
 * component renders. A kit that grew a fourth line would put it in an implicit
 * row of its own under a span of three — so the count is read off a rendered
 * tile rather than restated here.
 */

const STYLES = join(process.cwd(), "src/styles");
const require_ = createRequire(join(process.cwd(), "package.json"));
const TILE = ".crewlet-stat-group > .crewlet-statcard";

/** A sheet with its comments blanked, so a selector in prose is not a rule. */
function bare(css: string): string {
  return css.replace(/\/\*[\s\S]*?\*\//g, (c) => c.replace(/[^\n]/g, " "));
}

/** Every top-level (unconditional) block whose selector list is exactly `selector`. */
function topLevel(css: string, selector: string): string[] {
  const out: string[] = [];
  let depth = 0;
  let start = 0;
  for (let i = 0; i < css.length; i++) {
    const ch = css[i];
    if (ch === "{") {
      if (depth === 0) {
        const prelude = css.slice(start, i).trim().replace(/\s+/g, " ");
        if (prelude === selector) out.push(css.slice(i + 1, css.indexOf("}", i)));
      }
      depth++;
    } else if (ch === "}") {
      depth--;
      if (depth === 0) start = i + 1;
    } else if (ch === ";" && depth === 0) {
      start = i + 1;
    }
  }
  return out;
}

afterEach(cleanup);

test("every tile in a stat row takes its lines from the row, one row per line it draws", () => {
  const { container } = render(
    <StatGroup columns={3}>
      <StatCard label="Live nodes" value={1} sub="holding an unexpired lease" />
      <StatCard label="Behind on config" value={0} />
      <StatCard label="Unplaceable" value={0} sub="" />
    </StatGroup>,
  );
  const tiles = [...container.querySelectorAll(TILE)];
  expect(tiles).toHaveLength(3);
  // THE SAME NUMBER OF LINES WHETHER OR NOT A SUB WAS GIVEN: an absent sub is
  // still an element holding the reserve, which is what lets one span serve
  // every tile.
  const lines = new Set(tiles.map((t) => t.children.length));
  expect([...lines]).toHaveLength(1);
  const [count] = [...lines];

  const ours = topLevel(bare(readFileSync(join(STYLES, "screens.css"), "utf8")), TILE);
  expect(ours, "one unconditional rule, at every width").toHaveLength(1);
  const rule = ours[0]!;
  expect(rule).toMatch(/(^|[\s;])display:\s*grid\s*;/);
  expect(rule).toMatch(/grid-template-rows:\s*subgrid\s*;/);
  expect(rule).toMatch(new RegExp(`grid-row:\\s*span ${count}\\s*;`));

  // A LABEL SITS ON ITS NUMBER: where one label in the row wraps, the others
  // end at the foot of the label row, one gap above their figure, rather than
  // floating in its middle. The rule names the tile's first line, which is the
  // label the kit renders.
  for (const tile of tiles) {
    expect(tile.firstElementChild?.classList.contains("crewlet-statcard__label")).toBe(true);
  }
  const label = topLevel(
    bare(readFileSync(join(STYLES, "screens.css"), "utf8")),
    `${TILE} > :first-child`,
  );
  expect(label).toHaveLength(1);
  expect(label[0]).toMatch(/align-self:\s*end\s*;/);
});

// UNCONTESTED BY THE KIT. uilet's own two-class rule on the same selector
// clears the tile's surface; if it ever set a display or a row placement, which
// of the two won would be settled by stylesheet order rather than by weight.
test("the kit's rule for a tile in a group sets no display or row of its own", () => {
  const kit = bare(readFileSync(require_.resolve("@crewlethq/ui/styles.css"), "utf8"));
  const theirs = topLevel(kit, TILE);
  expect(theirs.length).toBeGreaterThan(0);
  for (const rule of theirs) {
    expect(rule).not.toMatch(/(^|[\s;])(display|grid-row|grid-template-rows)\s*:/);
  }
});
