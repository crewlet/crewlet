// @vitest-environment node
import { describe, expect, test } from "vitest";
import { sheetRules } from "../test/sheets.ts";

/**
 * A BOX THAT SCROLLS INSIDE THE PAGE HANDS THE WHEEL BACK AT EITHER END.
 *
 * A running phase's transcript was `max-height: 620px` with
 * `overscroll-behavior: contain`. The literal was unrelated to the view — the
 * page bar wraps, and the object header, the tabs and two card heads sit above
 * the box — so on most windows the box's own bottom was below the fold. And
 * `useTail` keeps a following box pinned at its END, which is exactly where
 * containment swallows a wheel-down: the reader scrolled, nothing moved, and
 * half the transcript stayed out of sight until they moved the pointer off the
 * box. "I only get half of the content", in the reader's words. A seat's thread
 * list carried the same pair, with a `100dvh` sum standing in for the literal.
 *
 * Two rules end it, and both live in the sheets:
 *
 *  - NO BLOCK-AXIS CONTAINMENT. `contain` or `none` on the block axis stops a
 *    wheel at a box's end from moving the page. The one box that may keep it
 *    is on the roster below with its reason: a box that is always wholly in
 *    view cannot hide anything below the fold.
 *  - AN IN-PAGE SCROLLER IS BOUNDED BY THE SCROLLER'S VIEW, never a literal.
 *    `100cqb` against the shell's `main` (a size container on every screen) is
 *    the view less the bar, the inset and any banner; `px` and `dvh` are
 *    guesses at it, wrong in both directions.
 *
 * ASSERTED IN THE SHEETS because jsdom computes no layout and resolves no
 * custom property: whether a wheel chains is a fact no rendered-DOM suite can
 * observe. The horizontal axis is not this gate's subject — a strip scrolled
 * sideways (`overscroll-behavior-x`) never holds the page's own wheel.
 */

/**
 * Every rule as `file selector -> its body`, rules sharing a selector joined —
 * through the shared walk, which also reads the first rule inside an at-rule
 * (where the thread list's bound sits).
 */
function rules(): Map<string, string> {
  const out = new Map<string, string>();
  for (const { file, selector, body } of sheetRules()) {
    const key = `${file} ${selector}`;
    out.set(key, `${out.get(key) ?? ""};${body}`);
  }
  return out;
}

/** One declaration's value in a rule body, or undefined. */
function decl(body: string, property: string): string | undefined {
  const m = new RegExp(`(?:^|;)\\s*${property}\\s*:\\s*([^;]+)`).exec(body);
  return m?.[1]?.trim().replace(/\s+/g, " ");
}

/**
 * Whether a rule stops the wheel at its block-axis ends.
 *
 * The SHORTHAND takes one value for both axes or two as `x y`, so its block
 * half is the last word; `-y` and `-block` are the longhands. `none` stops the
 * chain as surely as `contain` — it only adds dropping the bounce.
 */
function holdsBlockWheel(body: string): boolean {
  const stops = (v: string | undefined) => v !== undefined && /^(contain|none)$/.test(v);
  const short = decl(body, "overscroll-behavior");
  const block = short?.split(" ").at(-1);
  return (
    stops(block) ||
    stops(decl(body, "overscroll-behavior-y")) ||
    stops(decl(body, "overscroll-behavior-block"))
  );
}

/**
 * The boxes that may keep the wheel, each with why it cannot hide anything.
 */
const KEEPS_THE_WHEEL: Record<string, string> = {
  "frame.css .section-column":
    "sticky and exactly the view (100cqb), so all of it is on screen at every scroll position; chaining at its end would move the section beside it instead",
};

/** The in-page scrollers whose bound has to be the view. */
const VIEW_BOUNDED: Record<string, string> = {
  "screens.css .tail-scroll.tailing": "a running phase's transcript",
  "screens.css .thread-split .thread-list": "a seat's thread list, beside the sticky detail",
};

describe("a box that scrolls inside the page", () => {
  test("the detector reads every spelling of a block-axis stop, and only those", () => {
    // THE CONTROL. A gate whose predicate never fires certifies nothing.
    expect(holdsBlockWheel("overscroll-behavior: contain;")).toBe(true);
    expect(holdsBlockWheel("overscroll-behavior: auto contain;")).toBe(true);
    expect(holdsBlockWheel("overscroll-behavior: none;")).toBe(true);
    expect(holdsBlockWheel("color: red; overscroll-behavior-y: contain;")).toBe(true);
    expect(holdsBlockWheel("overscroll-behavior-block: none;")).toBe(true);
    expect(holdsBlockWheel("overscroll-behavior: contain auto;")).toBe(false);
    expect(holdsBlockWheel("overscroll-behavior-x: contain;")).toBe(false);
    expect(holdsBlockWheel("overflow-y: auto;")).toBe(false);
  });

  test("hands the wheel back to the page at either end", () => {
    const all = rules();
    // A scan over nothing passes; this one has to have read the sheets.
    expect(all.size).toBeGreaterThan(500);
    const holding = [...all].filter(([, body]) => holdsBlockWheel(body)).map(([where]) => where);
    for (const where of Object.keys(KEEPS_THE_WHEEL)) {
      expect(holding, `${where} is on the roster, so it must still keep the wheel`).toContain(
        where,
      );
    }
    const unexcused = holding.filter((where) => !(where in KEEPS_THE_WHEEL));
    expect(
      unexcused,
      "these hold the wheel at their block-axis end: a box pinned there (a tailing " +
        "transcript) or whose bottom is below the fold swallows every wheel-down while " +
        "the page has more to show. Drop the containment, or — for a box that is always " +
        "wholly in view — add it to KEEPS_THE_WHEEL with the reason.",
    ).toEqual([]);
  });

  test("is bounded by the scroller's view, never by a literal", () => {
    const all = rules();
    for (const [where, what] of Object.entries(VIEW_BOUNDED)) {
      const body = all.get(where);
      expect(body, `${what} (${where}) is gone from the sheets`).toBeDefined();
      // THE BOUND, after one hop through a custom property declared in the
      // same rule: the transcript names its bound once, because its records
      // are bounded by it too.
      let bound = decl(body!, "max-height") ?? "";
      const via = /^var\((--[\w-]+)\)$/.exec(bound);
      if (via) bound = decl(body!, via[1]!) ?? "";
      expect(bound, `${what} is bounded by the scroller's view`).toContain("100cqb");
      expect(bound, `${what}: a px literal is a guess at the view`).not.toMatch(/\d\s*px\b/);
      expect(bound, `${what}: a viewport unit counts the page bar and inset wrong`).not.toMatch(
        /\d\s*[ds]?v[hb]\b/,
      );
    }
  });

  test("the view it is bounded by is the shell's scroller, on every screen", () => {
    // `100cqb` resolves against the nearest SIZE container; without this rule
    // it falls back to the small viewport, which is the `dvh` guess again.
    const main = rules().get("frame.css .app .crewlet-app-shell__main");
    expect(main).toMatch(/container:\s*scroller\s*\/\s*size/);
  });
});
