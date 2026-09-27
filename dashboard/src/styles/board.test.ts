// @vitest-environment node
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

/**
 * The board's lane arithmetic, evaluated rather than read.
 *
 * A LANE IS SIZED FROM THE BOARD (`screens.css`, "The board"): `--lanes` steps
 * with the board's container width and `--lane-w` divides the width among
 * them. jsdom computes no layout, so a board whose last in-view lane ends ON
 * the scroller's edge — where the fade saying "the board goes on" is drawn
 * over its right border — renders green in every component suite. At 1280 it
 * did exactly that: In review's cards lost their right edge. So this reads the
 * rule, substitutes a width, and asserts the one invariant a reader sees.
 */

const STYLES = fileURLToPath(new URL(".", import.meta.url));
const css = readFileSync(join(STYLES, "screens.css"), "utf8").replace(/\/\*[\s\S]*?\*\//g, "");

/** The `.work-board` rule's own body (the first, unconditional one). */
const board = /\n\.work-board \{([^}]*)\}/.exec(css)?.[1] ?? "";

/** `--name: value` from that body. */
function prop(name: string): string {
  const m = new RegExp(`${name}:\\s*([^;]+);`).exec(board);
  if (!m) throw new Error(`.work-board declares no ${name}`);
  return m[1]!.trim();
}

const GAP = 16; // --spacing-4 at the default density
const INSET = Number(/^(\d+)px$/.exec(prop("--board-inset"))?.[1]);
const FLOOR = 260;

/** Every `@container board (width >= Npx) { .work-board { --lanes: n } }` step. */
const steps = [
  ...css.matchAll(
    /@container board \(width >= (\d+)px\)\s*\{\s*\.work-board\s*\{\s*--lanes:\s*(\d+);/g,
  ),
].map((m) => ({ from: Number(m[1]), lanes: Number(m[2]) }));

/** How many lanes the board holds at `width`. */
function lanesAt(width: number): number {
  let n = Number(prop("--lanes"));
  for (const s of steps) if (width >= s.from) n = s.lanes;
  return n;
}

/** `--lane-w` at `width`: the calc, its variables substituted, evaluated. */
function laneWidth(width: number): number {
  const expr = prop("--lane-w")
    .replace(/^calc/, "")
    .replace(/100cqi/g, String(width))
    .replace(/var\(--board-inset\)/g, String(INSET))
    .replace(/var\(--spacing-4\)/g, String(GAP))
    .replace(/var\(--lanes\)/g, String(lanesAt(width)));
  if (!/^[\d\s.+\-*/()]+$/.test(expr)) throw new Error(`--lane-w is not plain arithmetic: ${expr}`);
  return Function(`return (${expr});`)() as number;
}

test("the board steps up one lane at a time", () => {
  expect(steps.length).toBeGreaterThan(3);
  steps.forEach((s, i) => expect(s.lanes).toBe(i + 2));
});

// EVERY LANE IN VIEW IS WHOLE AND FOLLOWED BY ITS GAP, so the scroller's edge
// (and the fade drawn there) falls between two lanes rather than on a card's
// border — and no lane is narrower than the floor a card's title needs. Swept
// over every board width from one lane to past the last step, the 1280 and
// 1440 windows' boards (984px and 1142px) among them.
test("at every board width the lanes in view end a gap short of the edge, none below the floor", () => {
  for (let width = steps[0]!.from; width <= steps[steps.length - 1]!.from + 300; width++) {
    const n = lanesAt(width);
    const lane = laneWidth(width);
    const used = INSET + n * (lane + GAP) + INSET;
    expect(used, `at ${width}px, ${n} lanes of ${lane}px`).toBeLessThanOrEqual(width + 1e-6);
    expect(lane, `at ${width}px`).toBeGreaterThanOrEqual(FLOOR - 1e-6);
  }
});
