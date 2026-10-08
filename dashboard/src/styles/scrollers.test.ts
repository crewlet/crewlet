// @vitest-environment node
import { describe, expect, test } from "vitest";
import { sheetRules } from "../test/sheets.ts";
import { lineOf, modules, parse, stringValue, walk, type Node } from "../test/source.ts";

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
 *    guesses at it, wrong in both directions. EVERY scroller in the sheets is
 *    held to it, not a list of the ones somebody remembered: the gate used to
 *    check two named boxes, and the board's lanes sat beside them at
 *    `min(70dvh, 760px)` with nothing to say so. A box with a literal bound
 *    passes only from the roster below, with the reason it cannot hide
 *    anything — and a roster entry that stopped being true fails too.
 *
 * And one rule rides on the second: THE RECORD CEILING, which bounds every
 * block of machine text the kit's CodeBlock draws, is declared once in the
 * cascade from the same view, rather than passed as a number at each call.
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

/**
 * One declaration's value in a rule body, or undefined — the FIRST one, which
 * in a joined body is the selector's base rule: a later one is usually an
 * at-rule's override for another width (the Settings column goes back into
 * the flow on a phone), and the base rule is the box this gate is about.
 */
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

/** Whether a rule scrolls its block axis: `overflow-y`, `overflow-block`, or the shorthand's last word. */
function scrollsBlock(body: string): boolean {
  const block =
    decl(body, "overflow-y") ??
    decl(body, "overflow-block") ??
    decl(body, "overflow")?.split(" ").at(-1);
  return block !== undefined && /^(auto|scroll)$/.test(block);
}

/**
 * A scroller's declared height bound, after one hop through a custom property
 * declared in the same rule (the transcript names its bound once, because its
 * records are bounded by it too) — or undefined for a scroller that declares
 * none, whose height is the layout it FILLS: a full-height screen's column, the
 * peek's body. Those are bounded by a view rather than a guess at one, and so
 * is a box that is exactly `100%` of what holds it (the task page's frame, on
 * the widths where its column and rail scroll as one): it fills, it does not
 * guess.
 */
function boundOf(body: string): string | undefined {
  let bound = ["max-height", "height", "max-block-size", "block-size"]
    .map((p) => decl(body, p))
    .find((v) => v !== undefined);
  if (bound === undefined) return undefined;
  const via = /^var\((--[\w-]+)\)$/.exec(bound);
  if (via) bound = decl(body, via[1]!) ?? bound;
  return bound === "100%" ? undefined : bound;
}

/** Whether a bound is derived from the view: `100cqb`, and no `px` or viewport-unit literal. */
function fromTheView(bound: string): boolean {
  return bound.includes("100cqb") && !/\d\s*px\b/.test(bound) && !/\d\s*[ds]?v[hb]\b/.test(bound);
}

/**
 * The boxes that may keep the wheel, each with why it cannot hide anything.
 */
const KEEPS_THE_WHEEL: Record<string, string> = {
  "frame.css .section-column":
    "sticky and exactly the view (100cqb), so all of it is on screen at every scroll position; chaining at its end would move the section beside it instead",
};

/**
 * The scrollers whose bound is a literal, each with why that cannot hide
 * anything below the fold. Two reasons hold, and only two: the box is not in
 * the page's flow at all (an overlay or a dialog, which the page's wheel and
 * fold do not reach), or it is shorter than any view it can sit in.
 */
const LITERAL_BOUND: Record<string, string> = {
  "components.css .input-suggest-list":
    "an absolutely positioned completion list over the field that opened it, in a dialog: an overlay, never a box in the page's flow",
  "screens.css .work-menu-list":
    "the list inside a filter menu's Popover: an overlay, never a box in the page's flow",
  "screens.css .composer-picklist":
    "a pick list inside the composer's Modal, which is portaled outside the page",
  "screens.css .int-manifest-text":
    "the manifest inside the integration setup Modal, which is portaled outside the page",
  "screens.css .int-subjects":
    "9rem of chips on a finding — shorter than any view it can sit in, so once scrolled to it is wholly on screen",
};

/** In-page scrollers the scan must find bounded by the view: the control that it read them. */
const VIEW_BOUNDED: Record<string, string> = {
  "screens.css .tail-scroll.tailing": "a running phase's transcript",
  "screens.css .thread-split .thread-list": "a seat's thread list, beside the sticky detail",
  "screens.css .work-col-body": "a board lane's body",
  "frame.css .section-column": "the Settings column",
};

describe("a box that scrolls inside the page", () => {
  test("the detectors read every spelling, and only those", () => {
    // THE CONTROL. A gate whose predicate never fires certifies nothing.
    expect(holdsBlockWheel("overscroll-behavior: contain;")).toBe(true);
    expect(holdsBlockWheel("overscroll-behavior: auto contain;")).toBe(true);
    expect(holdsBlockWheel("overscroll-behavior: none;")).toBe(true);
    expect(holdsBlockWheel("color: red; overscroll-behavior-y: contain;")).toBe(true);
    expect(holdsBlockWheel("overscroll-behavior-block: none;")).toBe(true);
    expect(holdsBlockWheel("overscroll-behavior: contain auto;")).toBe(false);
    expect(holdsBlockWheel("overscroll-behavior-x: contain;")).toBe(false);
    expect(holdsBlockWheel("overflow-y: auto;")).toBe(false);

    expect(scrollsBlock("overflow-y: auto;")).toBe(true);
    expect(scrollsBlock("overflow: hidden auto;")).toBe(true);
    expect(scrollsBlock("overflow: auto;")).toBe(true);
    expect(scrollsBlock("overflow-block: scroll;")).toBe(true);
    expect(scrollsBlock("overflow: auto hidden;")).toBe(false);
    expect(scrollsBlock("overflow-x: auto;")).toBe(false);

    expect(boundOf("max-height: var(--b); --b: calc(100cqb - 2px);")).toBe("calc(100cqb - 2px)");
    expect(boundOf("overflow-y: auto;")).toBeUndefined();
    expect(boundOf("height: 100%; overflow-y: auto;")).toBeUndefined();
    expect(fromTheView("max(calc(var(--size-row-md) * 4), calc(100cqb - var(--x)))")).toBe(true);
    expect(fromTheView("min(70dvh, 760px)")).toBe(false);
    expect(fromTheView("calc(100cqb - 40px)")).toBe(false);
    expect(fromTheView("calc(100dvh - 100cqb)")).toBe(false);
    expect(fromTheView("16rem")).toBe(false);
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

  test("is bounded by the scroller's view, never by a literal — every one of them", () => {
    const all = rules();
    expect(all.size).toBeGreaterThan(500);
    const bounded = [...all]
      .filter(([, body]) => scrollsBlock(body))
      .map(([where, body]) => ({ where, bound: boundOf(body) }))
      .filter((s): s is { where: string; bound: string } => s.bound !== undefined);
    // THE CONTROL: the scan found the boxes it is known to hold, view-bound.
    for (const [where, what] of Object.entries(VIEW_BOUNDED)) {
      const found = bounded.find((s) => s.where === where);
      expect(
        found,
        `${what} (${where}) is gone from the sheets, or no longer scrolls`,
      ).toBeDefined();
      expect(fromTheView(found!.bound), `${what} is bounded by the scroller's view`).toBe(true);
    }
    // EVERY ROSTER ENTRY STILL EARNS ITS PLACE: a box that was rebound from the
    // view, or deleted, leaves an excuse behind for whatever takes its name.
    for (const [where, why] of Object.entries(LITERAL_BOUND)) {
      const found = bounded.find((s) => s.where === where);
      expect(
        found,
        `${where} is on the roster (${why}) but no longer scrolls with a bound`,
      ).toBeDefined();
      expect(
        fromTheView(found!.bound),
        `${where} is on the roster, so its bound is a literal`,
      ).toBe(false);
    }
    const guessed = bounded
      .filter((s) => !fromTheView(s.bound) && !(s.where in LITERAL_BOUND))
      .map((s) => `${s.where} — ${s.bound}`);
    expect(
      guessed,
      "these scroll inside the page with a bound that is a guess at the view: too tall on a " +
        "short window, where the box's bottom sits below the fold, and too short on a tall one " +
        "for nothing. Derive the bound from `100cqb` less what covers or must accompany the " +
        "box — or, for an overlay or a box shorter than any view, add it to LITERAL_BOUND " +
        "with the reason.",
    ).toEqual([]);
  });

  test("the view it is bounded by is the shell's scroller, on every screen", () => {
    // `100cqb` resolves against the nearest SIZE container; without this rule
    // it falls back to the small viewport, which is the `dvh` guess again.
    const main = rules().get("frame.css .app .crewlet-app-shell__main");
    expect(main).toMatch(/container:\s*scroller\s*\/\s*size/);
    // And the peek's, for a record in a peek, which is not inside `main`.
    expect(rules().get("frame.css .peek-body")).toMatch(/container:\s*peek\s*\/\s*size/);
  });
});

/**
 * THE RECORD CEILING IS DECLARED ONCE, from the view. Only the phase card's
 * tool records followed the view; nineteen other blocks passed a bare 460, so
 * on a window whose view was shorter, a record's bottom sat below the fold
 * while the wheel was inside it. And a ceiling passed as a prop arrives as an
 * inline custom property, which the sheet half of this gate cannot see.
 */
describe("the record ceiling", () => {
  const SCROLLERS = "frame.css .app .crewlet-app-shell__main, .app .peek-body";

  test("is the view, capped and floored, on both scrollers a record can sit in", () => {
    const body = rules().get(SCROLLERS);
    expect(body, `the ceiling's rule (${SCROLLERS}) is gone`).toBeDefined();
    const ceiling = decl(body!, "--record-ceiling") ?? "";
    expect(fromTheView(ceiling), "the record ceiling is the scroller's view").toBe(true);
    expect(ceiling).toContain("var(--record-max)");
    expect(ceiling).toContain("var(--sticky-top)");
    // The kit's property, which every CodeBlock inside reads.
    expect(decl(body!, "--crewlet-codeblock-max-height")).toBe("var(--record-ceiling)");
    // The cap is declared once, at the root, for the one block outside both.
    expect(decl(rules().get("tokens.css :root") ?? "", "--record-max")).toBe("460px");
  });

  test("narrows to its box inside a running ledger, re-declared for the kit", () => {
    // `var()` in a custom property resolves where it is declared, so the
    // shell's settled value is what a descendant inherits unless this says
    // otherwise here.
    const tail = rules().get("screens.css .tail-scroll.tailing") ?? "";
    expect(decl(tail, "--record-ceiling")).toMatch(/var\(--tail-bound\)/);
    expect(decl(tail, "--record-ceiling")).toContain("var(--record-max)");
    expect(decl(tail, "--crewlet-codeblock-max-height")).toBe("var(--record-ceiling)");
  });

  /** Every `maxHeight` a `<CodeBlock>` is given, as `path:line — value`. */
  function ceilingsPassed(): { where: string; path: string; value: string | null }[] {
    const out: { where: string; path: string; value: string | null }[] = [];
    for (const { path, text, lang } of modules()) {
      if (lang !== "tsx" || path.includes(".test.")) continue;
      const line = lineOf(text);
      walk(parse(text, lang), (node, parent) => {
        if (node.type !== "JSXAttribute") return true;
        const name = node.name as Node;
        const tag = parent?.name as Node | undefined;
        if (name.type !== "JSXIdentifier" || String(name.name) !== "maxHeight") return true;
        if (tag?.type !== "JSXIdentifier" || String(tag.name) !== "CodeBlock") return true;
        const value = node.value as Node | null;
        const literal =
          value?.type === "JSXExpressionContainer"
            ? stringValue(value.expression as Node)
            : stringValue(value ?? undefined);
        out.push({ where: `${path}:${line(node.start)}`, path, value: literal });
        return true;
      });
    }
    return out;
  }

  /** The blocks that state a ceiling of their own, and why each one differs. */
  const STATES_ITS_OWN: Record<string, string> = {
    "routes/org/builder/AfterSaveStrip.tsx":
      "the YAML dialog: a Modal is portaled outside both scrollers, so the inherited ceiling never reaches it",
    "routes/live/trace/Waterfall.tsx":
      "a span's Input and Output, read as a pair: half the ceiling each, derived from it",
  };

  test("is passed at no call but the ones that differ, and derived even there", () => {
    const passed = ceilingsPassed();
    // THE CONTROL: the scan reads the tree's calls, so it cannot pass by
    // reading nothing.
    for (const path of Object.keys(STATES_ITS_OWN)) {
      expect(
        passed.some((p) => p.path === path),
        `${path} is on the roster, so it must still state a ceiling`,
      ).toBe(true);
    }
    const unexcused = passed.filter((p) => !(p.path in STATES_ITS_OWN)).map((p) => p.where);
    expect(
      unexcused,
      "these pass a CodeBlock its own ceiling. The shell declares one from the view " +
        "(`--record-ceiling`, frame.css), and a number passed here is the literal it replaced. " +
        "Drop the prop, or — for a block outside the shell's scrollers — add it to " +
        "STATES_ITS_OWN with the reason.",
    ).toEqual([]);
    // EVEN THERE, NEVER A NUMBER: a value names the root's cap or derives from
    // the ceiling, so the 460 is written once.
    const numbers = passed
      .filter((p) => p.value === null || !/var\(--record-(max|ceiling)\)/.test(p.value))
      .map((p) => `${p.where} — ${p.value ?? "(not a constant string)"}`);
    expect(numbers).toEqual([]);
  });
});
