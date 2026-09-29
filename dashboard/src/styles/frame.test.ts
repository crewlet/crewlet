// @vitest-environment node
import { readdirSync, readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "vitest";
import { density, size } from "@crewlethq/tokens";

import {
  CONTENT_PADDING,
  LIST_FLOOR,
  PEEK_WIDTH,
  PHONE_BREAKPOINT,
  SCROLLBAR_GUTTER,
  SHELL_BREAKPOINT,
  densityScale,
  listReserve,
  peekColumnMin,
} from "~/app/layout.ts";

/**
 * The chrome's layout rules that fail SILENTLY and correctly in every other
 * gate — frame.css (the page column's own furniture included), the shared
 * panel in components.css, and the
 * screens whose own rules have to agree with the chrome's sticky band.
 *
 * `classes.test.ts` proves a class is declared and `tokens.test.ts` proves a
 * property is. Neither can see a declaration the CSS parser drops, a track
 * reserved at a width where the element that fills it has left the flow, or a
 * sticky box confined to a scrollport that never scrolls. All three shipped
 * here at once, all three are invisible in a diff, and none of them can be
 * asserted through the DOM — jsdom computes no layout, so the suites that
 * render the frame would stay green through every one.
 */

const STYLES = fileURLToPath(new URL(".", import.meta.url));
const require_ = createRequire(import.meta.url);

/** The kit's own stylesheet, comments blanked as [sheet] blanks ours. */
function kitSheet(): string {
  return readFileSync(require_.resolve("@crewlethq/ui/styles.css"), "utf8").replace(
    /\/\*[\s\S]*?\*\//g,
    (c) => c.replace(/[^\n]/g, " "),
  );
}

/**
 * Every `@media` rule's whole body, braces balanced — a rule's selector inside
 * one is at a width, and a gate about "no width" has to see every one.
 */
function mediaBodies(css: string): { prelude: string; body: string }[] {
  const out: { prelude: string; body: string }[] = [];
  for (const m of css.matchAll(/@media[^{]*\{/g)) {
    let depth = 1;
    let i = m.index + m[0].length;
    for (; i < css.length && depth > 0; i++) {
      if (css[i] === "{") depth++;
      else if (css[i] === "}") depth--;
    }
    out.push({ prelude: m[0].slice(0, -1).trim(), body: css.slice(m.index + m[0].length, i - 1) });
  }
  return out;
}

/**
 * One stylesheet, WITH ITS COMMENTS BLANKED.
 *
 * Every case here reads CSS as text, and several of them slice it at a
 * breakpoint — `split("@media (width < 640px)")` for the wide half,
 * `indexOf("@container page (width <")` for the broken bar. A COMMENT
 * naming one of those at-rules truncates that slice at the prose instead of
 * at the rule, and this file's comments name at-rules constantly, because
 * explaining why a rule sits at a breakpoint means writing the breakpoint
 * down.
 *
 * It has happened: a note on `.page` mentioning a phone breakpoint's query
 * moved the wide half's end 480 lines up the file, and three cases failed
 * naming `.grid-wrap` and `.rail-label` — selectors nothing in that change
 * had touched. A gate that fails for the wrong reason is a gate somebody
 * fixes by deleting an assertion.
 *
 * BLANKED RATHER THAN REMOVED: every comment becomes spaces of the same
 * length, so an offset into this string is still an offset into the file and
 * a failure's line number still points at the rule.
 */
function sheet(name: string): string {
  return readFileSync(join(STYLES, name), "utf8").replace(/\/\*[\s\S]*?\*\//g, (c) =>
    c.replace(/[^\n]/g, " "),
  );
}

/**
 * Every width a sheet's media queries name, with the query it is in.
 *
 * EVERY WIDTH IN THE PRELUDE, not the first: a range is two of them,
 * `(width >= 640px) and (width < 1024px)`, and a reading that stopped at the
 * first left the second free to be any number at all.
 */
function mediaWidths(css: string): { prelude: string; width: number; written: string }[] {
  return [...css.matchAll(/@media[^{]*/g)].flatMap((p) =>
    [...p[0].matchAll(/(\d+(?:\.\d+)?)px/g)].map((w) => ({
      prelude: p[0].trim(),
      width: Number(w[1]),
      written: w[0],
    })),
  );
}

/** The body of the first at-rule whose prelude is exactly `prelude`, braces balanced. */
function atRuleBody(css: string, prelude: string): string {
  const at = css.indexOf(prelude);
  expect(at, `${prelude} is not in the sheet`).toBeGreaterThan(-1);
  const open = css.indexOf("{", at);
  let depth = 1;
  let i = open + 1;
  for (; i < css.length && depth > 0; i++) {
    if (css[i] === "{") depth++;
    else if (css[i] === "}") depth--;
  }
  return css.slice(open + 1, i - 1);
}

/** The phone breakpoint's query, as every sheet writes it. */
const PHONE = `@media (width < ${PHONE_BREAKPOINT}px)`;

/** The body of the first rule whose selector is exactly `selector`. */
function block(css: string, selector: string): string {
  const at = new RegExp(
    `(^|\\})\\s*${selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}\\s*\\{`,
    "m",
  );
  const m = at.exec(css);
  expect(m, `${selector} is not declared at all`).not.toBeNull();
  const from = m!.index + m![0].length;
  const to = css.indexOf("}", from);
  expect(to, `${selector} has no closing brace`).toBeGreaterThan(-1);
  return css.slice(from, to);
}

/**
 * Every declaration block whose selector LIST names `selector`.
 *
 * `block` above wants a rule of its own; these live in a grouped selector,
 * which is the point — the four steps of the subgrid chain have to say the
 * same thing, so they say it once.
 */
function rules(css: string, selector: string): string[] {
  const bare = css.replace(/\/\*[\s\S]*?\*\//g, "");
  const out: string[] = [];
  for (const m of bare.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
    if (m[1]!.split(",").some((name) => name.trim() === selector)) out.push(m[2]!);
  }
  expect(out.length, `${selector} is not declared at all`).toBeGreaterThan(0);
  return out;
}

/**
 * EVERY declaration block whose selector list MENTIONS `selector`, wherever it
 * sits — a rule of its own, one member of a grouped selector, a descendant
 * form, or a copy nested inside an `@media`.
 *
 * `block` above reads the FIRST rule with an exact selector and `rules` wants
 * an exact one too, so both are blind to a second declaration of the same
 * thing — and a gate that forbids a declaration has to see all of them or it
 * forbids nothing. Measured: appending
 *
 *     @media (min-width: 900px) { .num-block { max-width: 38rem } }
 *
 * to components.css restored the 608px clamp at exactly the widths the bug
 * was visible at, and every test in this directory stayed green.
 *
 * Each entry keeps its own selector text, because what a gate has to say
 * about `.num-block` is usually not what it has to say about
 * `.num-block > .row`.
 */
function mentioning(css: string, selector: string): { sel: string; body: string }[] {
  const bare = css.replace(/\/\*[\s\S]*?\*\//g, "");
  const token = new RegExp(`${selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}(?![\\w-])`);
  const out: { sel: string; body: string }[] = [];
  for (const m of bare.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
    for (const name of m[1]!.split(",")) {
      if (token.test(name)) out.push({ sel: name.trim(), body: m[2]! });
    }
  }
  expect(out.length, `${selector} is not declared at all`).toBeGreaterThan(0);
  return out;
}

describe("the frame's layout", () => {
  // A CELL LANDS UNDER THE HEADER THAT NAMED IT.
  //
  // The head and every row were separate grid containers once, each handed the
  // same track list as an inline string. A track list is not a layout: an
  // intrinsic track resolves against the content of ITS OWN container, so each
  // row sized its columns against its own cells alone. Measured against a
  // running engine at 1600px — the work table's fifth column started at 78.4px
  // in the head and 74px in the rows, and the audit's last column sat 245px
  // from its own heading, with nothing to the right of WHO under the right
  // name on any line.
  //
  // Asserted in the SHEET because it cannot be asserted anywhere else: jsdom
  // computes no layout, so the suites that render a grid stay green through
  // every value of this, and a screenshot is the only other witness.
  test("one grid owns the columns and every row adopts them", () => {
    // THE WIDE LAYOUT ALONE. Below the phone breakpoint a row is deliberately
    // NOT a row — the heads go and each cell draws its own label — so the
    // narrow block unwinds this chain on purpose and would read here as the
    // very drift this case exists to catch. See "a grid becomes labelled
    // cards" above for the other half.
    const css = sheet("frame.css").split(PHONE)[0]!;
    expect(block(css, ".grid-wrap")).toMatch(/display:\s*grid/);
    // The track list itself is the component's, per screen — what belongs here
    // is that nothing BELOW the wrap resolves a list of its own.
    for (const step of [".grid-head", ".grid-body", ".grid-band", ".grid-row"]) {
      const decl = rules(css, step).join(";");
      expect(decl, step).toMatch(/display:\s*grid/);
      expect(decl, step).toMatch(/grid-template-columns:\s*subgrid/);
      // A SUBGRID THAT DOES NOT SPAN THE TRACKS ADOPTS NONE OF THEM: it is a
      // one-column grid that looks exactly like the bug it replaced.
      expect(decl, step).toMatch(/grid-column:\s*1\s*\/\s*-1/);
      expect(decl, step).not.toMatch(/grid-template-columns:(?!\s*subgrid)/);
    }
    // And what is NOT a row of cells is one element across every column,
    // rather than a value sized by — and sizing — the first one.
    expect(rules(css, ".grid-wrap > *").join(";")).toMatch(/grid-column:\s*1\s*\/\s*-1/);
    for (const full of [".grid-band-head", ".grid-band-foot"]) {
      expect(rules(css, full).join(";"), full).toMatch(/grid-column:\s*1\s*\/\s*-1/);
    }
    // THE COLUMN RESET IS THE ONE THAT HAS TO SAY IT TWICE. It is a direct
    // child, so `.grid-wrap > *` covers it — and its own `all: unset` is later
    // in the sheet at the same specificity, so it undoes exactly that. The
    // order inside the block is what the assertion is for.
    expect(rules(css, ".grid-cols-reset").join(";")).toMatch(
      /all:\s*unset[\s\S]*grid-column:\s*1\s*\/\s*-1/,
    );
  });

  // THE PEEK'S SHAPE IS ONE ATTRIBUTE WITH TWO VALUES, AND NO WIDTH HERE.
  //
  // It was two media queries on one literal — a column at `width >= 1112px`,
  // a drawer at `width < 1112px` — and the literal was wrong twice over: it
  // left out the screen's padding and scroller gutter, so the list beside a
  // peek was 396px at the threshold that promised it 444, and it could not
  // know that Settings draws a 236px column INSIDE the screen, so a peek at
  // 1280 left Settings' nodes grid 328px and cut its columns off. A media
  // query cannot see which screen is on or the reader's density, so the shell
  // decides (`app/layout.ts`) and writes `data-peek`. What this holds is that
  // nothing in a stylesheet decides it a second time.
  test("the peek is a column or a drawer by attribute, and no stylesheet gives it a width", () => {
    const css = sheet("frame.css");
    // EVERY VALUE THE SHEET ANSWERS is one of the shell's two. `true` was the
    // last spelling; a rule still keyed on it would match nothing and say so
    // to nobody.
    const values = [...css.matchAll(/\[data-peek(?:="([^"]*)")?\]/g)].map((m) => m[1] ?? "");
    expect(new Set(values)).toEqual(new Set(["column", "drawer"]));
    // AND NONE OF IT IS BEHIND A WIDTH, in any sheet: a peek rule inside a
    // media query is a second opinion about where the column fits.
    for (const name of readdirSync(STYLES).filter((f) => f.endsWith(".css"))) {
      for (const { prelude, body } of mediaBodies(sheet(name))) {
        expect(body, `${name}: ${prelude} decides the peek's shape`).not.toMatch(
          /data-peek|\.peek-(?:rail|veil|grip)\b/,
        );
      }
    }
    // The column side: the sheet becomes a grid whose second track is the
    // reader's width, CAPPED at what the sheet can give with the list at its
    // floor — and whose fallback is the resting width the threshold assumes.
    const grid = block(css, '.app[data-peek="column"] .crewlet-app-shell__sheet');
    expect(grid).toMatch(/display:\s*grid/);
    const track =
      /grid-template-columns:\s*minmax\(0, 1fr\)\s+min\(var\(--peek-w, (\d+)px\), 100% - var\(--peek-reserve\)\)/.exec(
        grid,
      );
    expect(
      track,
      "the peek's track is not the reader's width capped by the reserve",
    ).not.toBeNull();
    expect(Number(track![1])).toBe(PEEK_WIDTH);
    for (const part of [".page-head", ".crewlet-app-shell__main", ".peek-rail"]) {
      expect(block(css, `.app[data-peek="column"] ${part}`), part).toMatch(/grid-row:\s*\d/);
    }
    // The drawer side: fixed, over a veil, with no grip to drag.
    expect(block(css, '.app[data-peek="drawer"] .peek-rail')).toMatch(/position:\s*fixed/);
    expect(block(css, '.app[data-peek="drawer"] .peek-veil')).toMatch(/display:\s*block/);
    expect(block(css, '.app[data-peek="drawer"] .peek-grip')).toMatch(/display:\s*none/);
  });

  // EVERY TERM OF THE THRESHOLD IS HELD AGAINST THE RULE THAT DRAWS IT.
  //
  // The arithmetic in `app/layout.ts` is only as good as its list of what
  // stands between the window's edge and the list, and the list was short by
  // three terms once. Each term is asserted against the stylesheet — the
  // kit's or ours — that actually lays it out, so a width that changes there
  // fails here rather than silently narrowing a list.
  test("the peek's threshold counts every width in front of the list", () => {
    const kit = kitSheet();
    const ours = sheet("frame.css");
    // The sheet: inset on the right only — the rail is the gap on the left —
    // and a hairline each side.
    expect(block(kit, ".crewlet-app-shell__sheet")).toMatch(
      /margin:\s*var\(--size-shell-inset\) var\(--size-shell-inset\) var\(--size-shell-inset\) 0;/,
    );
    expect(block(kit, ".crewlet-app-shell__sheet")).toMatch(/border:\s*1px solid/);
    // The scroller keeps its gutter whether or not it scrolls, and the gutter
    // is the width base.css gives the scrollbar.
    expect(block(kit, ".crewlet-app-shell__main")).toMatch(/scrollbar-gutter:\s*stable/);
    expect(block(sheet("base.css"), "::-webkit-scrollbar")).toMatch(
      new RegExp(`width:\\s*${SCROLLBAR_GUTTER}px`),
    );
    // The screen's padding: the kit's content column, and the section body
    // beside Settings' column, which takes the kit's back and gives the same.
    expect(block(kit, ".crewlet-app-shell__content")).toMatch(
      /padding:\s*var\(--spacing-5\) var\(--spacing-5\)/,
    );
    expect(block(ours, ".section-body")).toMatch(
      /padding:\s*var\(--spacing-5\) var\(--spacing-5\)/,
    );
    expect(CONTENT_PADDING).toBe(20);
    // Settings' column is one rail wide, inside the screen.
    expect(block(ours, ".section-frame")).toMatch(
      /grid-template-columns:\s*var\(--size-shell-rail\) minmax\(0, 1fr\)/,
    );

    // THE NUMBERS, at the normal density: 236 + 8 + 2 + 420 + 10 + 40 + 444,
    // and 236 more beside Settings' column.
    const normal = densityScale("normal");
    expect(peekColumnMin({ sectionColumn: false, scale: normal })).toBe(1160);
    expect(peekColumnMin({ sectionColumn: true, scale: normal })).toBe(1396);

    // AND AT EVERY DENSITY, the list the column leaves at the threshold —
    // walked term by term here rather than through `listReserve`, so a term
    // dropped from the module is a list under the floor in this sum — is at
    // least the floor, and one pixel narrower it would not be.
    for (const choice of Object.keys(density) as (keyof typeof density)[]) {
      const scale = densityScale(choice);
      for (const sectionColumn of [false, true]) {
        const at = peekColumnMin({ sectionColumn, scale });
        const list = (window: number) =>
          window -
          Number.parseFloat(size.shell.rail) -
          Number.parseFloat(size.shell.inset) * scale -
          2 -
          PEEK_WIDTH -
          SCROLLBAR_GUTTER -
          2 * CONTENT_PADDING * scale -
          (sectionColumn ? Number.parseFloat(size.shell.rail) : 0);
        const frame = `${choice}${sectionColumn ? ", beside Settings' column" : ""}`;
        expect(list(at), frame).toBeGreaterThanOrEqual(LIST_FLOOR);
        expect(list(at - 1), frame).toBeLessThan(LIST_FLOOR);
        // The cap on the track is exactly the resting width at the threshold,
        // so a column never opens narrower than the width it was decided at.
        const sheetInside =
          at - Number.parseFloat(size.shell.rail) - Number.parseFloat(size.shell.inset) * scale - 2;
        expect(sheetInside - listReserve({ sectionColumn, scale }), frame).toBeGreaterThanOrEqual(
          PEEK_WIDTH,
        );
      }
    }
  });

  // EVERY WIDTH A STYLESHEET WRITES IN A MEDIA QUERY IS ONE `app/layout.ts`
  // DECLARES. A media query cannot read a custom property, so each is a
  // literal — and a literal nobody holds is how a drawer comes to open at one
  // width while the grid folds at another. Measured before this: the frame
  // folded at 860 and 960 against a kit that switches at 1024 and 640, and the
  // builder's buttons swapped for its menu at 860 in the sheet and at 640 in
  // the script.
  test("every media query's width is a breakpoint app/layout.ts declares", () => {
    const allowed = new Set([PHONE_BREAKPOINT, SHELL_BREAKPOINT]);
    const stray: string[] = [];
    for (const name of readdirSync(STYLES).filter((f) => f.endsWith(".css"))) {
      const css = sheet(name);
      for (const { prelude, width, written } of mediaWidths(css)) {
        if (!allowed.has(width)) stray.push(`${name}: ${prelude} — ${written}`);
      }
      // AND IN ONE SPELLING. `max-width: 639px` and `width < 640px` are the
      // same range written two ways, and a gate holding the number cannot see
      // that the second one is off by a pixel.
      for (const m of css.matchAll(/@media[^{]*(?:min|max)-width[^{]*/g)) {
        stray.push(`${name}: ${m[0].trim()} — write the range as width < / width >=`);
      }
    }
    expect(stray).toEqual([]);
  });

  // THE READING ITSELF, on the shape the old pattern missed.
  test("a media range's second width is read as well as its first", () => {
    const css = "@media (width >= 640px) and (width < 1000px) { .x { a: b } }";
    expect(mediaWidths(css).map((w) => w.width)).toEqual([640, 1000]);
  });

  test("the grid makes no scrollport, so its sticky heads resolve against the page", () => {
    // `.grid-wrap` has no height constraint, so an `overflow: hidden` on it
    // was a scroll container whose offset is permanently 0 — and a sticky box
    // is confined to its nearest scrollport, so `top` was never applied and
    // the column heads left the viewport with the rows. `clip` still rounds
    // the rows off at the radius and is not a scroll container.
    const css = sheet("frame.css");
    expect(block(css, ".grid-wrap")).toMatch(/overflow:\s*clip/);
    expect(block(css, ".grid-body")).not.toMatch(/overflow/);

    // And they stop below the toolbar rather than at 0, which is where the
    // toolbar itself is parked, opaque and ten z-levels up.
    expect(block(css, ".grid-head")).toMatch(/top:\s*var\(--sticky-top\)/);
    expect(block(css, ".grid-band-head")).toMatch(
      /top:\s*calc\(var\(--sticky-top\) \+ var\(--size-row-md\)\)/,
    );

    // NOR MAY THE CARD A GRID SITS IN. The rule is about the whole chain
    // between a sticky head and `.screen`, and uilet's flush card broke it one
    // level up: `overflow: hidden` for a clip its own comment describes as
    // rounding, which also makes the card a scroll container — and a flush
    // card grows to its content, so its offset is permanently 0. Measured
    // against a running engine, every `DataGrid` in one had a dead head: the
    // audit's 84 rows and the cost breakdown's 50 scrolled their column names
    // off the top of the window. `clip` rounds without scrolling, which is the
    // same correction `.grid-wrap` above already carries.
    expect(block(sheet("screens.css"), ".crewlet-card--flush")).toMatch(/overflow:\s*clip/);
  });

  // A SCREEN THAT ASKED FOR THE HEIGHT IS GIVEN ONE.
  //
  // `data-fill` is the shell's answer to `useFillScreen`, and it set
  // `overflow-y: hidden` on the scroller and `flex: 1 1 auto` on the column
  // inside it — but `.screen` is a BLOCK container, so the flex shorthand on
  // its child meant nothing and the column kept taking its height from its
  // content. The org builder's canvas lens therefore rendered its toolbar and
  // then a chart 2px tall, on a company the chart lens draws as six seats in
  // three units: every card was in the DOM, laid out in a world of zero height
  // inside a viewport of zero height.
  //
  // The rule this replaced put the request on `.screen-inner`, which was the
  // flex container already — so moving it onto the scroller moved the
  // container with it, and this declaration did not follow.
  // A ROW IS A CARD ON A PHONE, BECAUSE A GRID IS COLUMNS AND 390px HAS NONE.
  //
  // The audit's six columns want 808px and the work trash's twelve want more,
  // and the wrap is `overflow: clip` — deliberately, since a scrollport is
  // what stopped the sticky head working — so everything past the viewport was
  // CUT OFF with nothing to scroll to.
  //
  // Sideways scrolling cannot rescue it, which is the half worth pinning: the
  // flexible tracks are `minmax(0, 1fr)`, so the moment the wrap becomes a
  // scroller its content box is the viewport, the `max-content` columns alone
  // exceed it, and every `1fr` resolves to ZERO — the title column vanishing
  // to make room for a due date.
  test("a grid becomes labelled cards below the phone breakpoint", () => {
    const css = sheet("frame.css");
    const narrow = css.slice(css.indexOf(PHONE));

    // THE SUBGRID CHAIN IS UNWOUND. The wide layout is one grid whose head,
    // body, bands and rows adopt its tracks; a card is the opposite shape, so
    // each goes back to a block and each CELL becomes its own flex line —
    // label, then value. Not a two-column grid on the row: that is the shape
    // the next assertion but one says was tried and is wrong.
    expect(block(narrow, ".grid-wrap")).toMatch(/display:\s*block/);
    expect(block(narrow, ".grid-head")).toMatch(/display:\s*none/);
    expect(block(narrow, ".grid-row")).toMatch(/display:\s*block/);
    // ONE FLEX LINE PER CELL, not a grid track per value. `display: contents`
    // on the cell looked right and is not: a cell's children become grid items
    // in their own right, so a type mark beside a title lands in the value
    // track and pushes the title onto the next row's label track — measured at
    // 390px as four spans breaking out past the viewport.
    expect(block(narrow, ".grid-cell")).toMatch(/display:\s*flex/);

    // AND THE LABEL IS THE COLUMN'S OWN NAME, read from the attribute
    // DataGrid.tsx sets from a string header. A cell with no such header
    // carries no attribute, draws no label, and takes both tracks rather than
    // leaving an empty label column beside it.
    expect(narrow).toContain(".grid-cell[data-label]::before");

    // AND A CELL WITH NOTHING IN IT IS NOT A LINE. A column draws no value on
    // a row that has none — `PriorityMark` renders null for `normal`, which
    // nearly every task is — and in a table that is an empty track under a
    // head, which is what keeps the columns lined up. Here the label is the
    // cell's own, so the card opened with `PRIORITY` alone on a line, on every
    // ordinary row. `:empty` is the only thing that can see it: a container
    // cannot ask a child that drew nothing whether it did, and the element has
    // to stay in the DOM regardless, because the wide layout's tracks are
    // positional. Generated content is not a child node, so the `::before`
    // above does not defeat it, and a cell drawing a marked absence has
    // content and keeps its line. The DOM half — that an empty cell really is
    // childless — is `app/frame/DataGrid.test.tsx`'s.
    expect(block(narrow, ".grid-cell:empty")).toMatch(/display:\s*none/);
    // AND THE LABELS LINE UP without a shared track: a fixed flex basis is
    // what a flex line has instead of a grid column.
    expect(narrow).toMatch(/content:\s*attr\(data-label\)/);
    expect(narrow.slice(narrow.indexOf(".grid-cell[data-label]::before"))).toMatch(
      /flex:\s*0 0 \d/,
    );
  });

  // A LIST SCANNED BY THE DOZEN IS TWO LINES A ROW, not a labelled card. The
  // work list's card stood eight lines — 230px — per task at 390px, so a phone
  // showed two and a half; `phoneRows="compact"` draws what the row is, then its
  // marks. ONE FLEX RUN THAT BREAKS ONCE: the lead cells first, the row's own
  // full-width `::after` next, every other cell after it — and no label on any
  // of them, which is what makes it two lines rather than eight.
  test("a compact grid draws a phone row as its lead, then a line of marks", () => {
    const css = sheet("frame.css");
    const narrow = css.slice(css.indexOf(PHONE));
    const C = '.grid-wrap[data-phone-rows="compact"]';
    const row = block(narrow, `${C} .grid-row`);
    expect(row).toMatch(/display:\s*flex/);
    expect(row).toMatch(/flex-wrap:\s*wrap/);
    const brk = block(narrow, `${C} .grid-row::after`);
    expect(brk).toMatch(/flex-basis:\s*100%/);
    expect(brk).toMatch(/order:\s*1/);
    expect(block(narrow, `${C} .grid-cell`)).toMatch(/order:\s*2/);
    expect(block(narrow, `${C} .grid-cell[data-lead]`)).toMatch(/order:\s*0/);
    expect(block(narrow, `${C} .grid-cell[data-label]::before`)).toMatch(/content:\s*none/);
    // TWO LINES, NEVER THREE: a cell declared `phoneOmit` is not drawn there,
    // or the list's "3h ago" wrapped onto a line of its own under every task
    // with a status and a due date.
    expect(block(narrow, `${C} .grid-cell[data-phone-omit]`)).toMatch(/display:\s*none/);
  });

  // AND THAT BASIS IS A WIDTH, NOT A REQUEST.
  //
  // `flex: 0 0 7rem` reads like a fixed column and is not one while the label
  // cannot break: `min-width` on a flex item is `auto`, which floors the item
  // at its own min-content size, and the min-content size of a `nowrap` line
  // is the whole string. A head wider than 7rem therefore grew its OWN label
  // box and pushed only ITS value right, in a card whose entire point is that
  // the labels line up without a track to line them up with.
  //
  // It shipped, and it is invisible in every other gate: jsdom computes no
  // layout, so a rendered grid stays green through every value of this, and
  // only ONE head of the 111 this build declares is over the basis — which is
  // why no screenshot of the tracker or the audit ever showed it either.
  // Measured at 390px on the provisioning-passes grid: RAN, SURFACE, HOW IT
  // ENDED, TOOK and FINDINGS each put their value 136px from the card's edge,
  // and WHAT IT CONCLUDED drew a 128.03px label box and put its own at
  // 152.03px, in 197.97px of room instead of 214px. CONVERSATION, the widest
  // head that does fit, is 93.09px.
  //
  // BOTH DECLARATIONS, and neither holds alone — the reason this is one case
  // rather than two. Dropping `nowrap` only moves the floor from the string
  // to its longest WORD, measured at 184.06px for a 24-letter one; and
  // `overflow-wrap` with `nowrap` still in place has nothing to break at, so
  // the pair either fixes the column or does not.
  test("a card's label column holds its basis whatever the head says", () => {
    const css = sheet("frame.css");

    // EVERY block that draws the label, wherever it sits, because a gate that
    // forbids a declaration has to see all of them — a second copy in a
    // narrower breakpoint would otherwise put `nowrap` back unseen.
    const drawn = mentioning(css, "[data-label]::before");
    for (const { sel, body } of drawn) {
      expect(body, `${sel} cannot floor the label at its own string`).not.toMatch(
        /white-space:\s*nowrap/,
      );
    }

    const label = block(css, ".grid-cell[data-label]::before");
    // THE SCAN FOUND THE RULE, not an empty match: a renamed selector would
    // make every assertion above vacuous and still green.
    expect(label).toMatch(/content:\s*attr\(data-label\)/);
    expect(label).toMatch(/flex:\s*0 0 7rem/);
    // A HEAD WITH NO SPACE TO BREAK AT still has to break, which is the same
    // declaration `.grid-cell .truncate` carries one level down for values.
    expect(label).toMatch(/overflow-wrap:\s*anywhere/);
  });

  // A VALUE OWNS ITS LINE IN A CARD, so the wide layout's pixel cut is wrong
  // one level down as well. `.truncate` is right for a cell that IS one line
  // of a fixed column; with the column gone it cut the one field the reader
  // opened the list for and left the rest of the card empty under it —
  // measured at 390px as "Interview three desks about hou…" on the tracker's
  // title and "Backfill is running. 22 of 30 day…" on the audit's detail.
  //
  // WRAPPED RATHER THAN CLAMPED, and the clamp's own gate is why this test
  // can say so: `text.test.ts` holds `.clamp` to a single copy in base.css, so
  // a second one here would fail there. What bounds the value instead is the
  // engine's own `tracker.MaxTitle`, and there is no uniform row height to
  // protect — a card is already a stack of six to twelve labelled lines.
  test("a card's value wraps, because the column that justified cutting it is gone", () => {
    const css = sheet("frame.css");
    const narrow = css.slice(css.indexOf(PHONE));

    const cut = block(narrow, ".grid-cell .truncate");
    // ALL THREE, and they only work together: `.truncate` is three
    // declarations, and neutralising one of them leaves the other two cutting.
    // `white-space` alone still hides the overflow; `overflow` alone still has
    // nothing to wrap at.
    expect(cut).toMatch(/white-space:\s*normal/);
    expect(cut).toMatch(/overflow:\s*visible/);
    expect(cut).toMatch(/text-overflow:\s*clip/);
    // AND A VALUE WITH NO SPACE TO BREAK AT — a uuid, a key — still has to
    // break, for the same reason `.clamp` carries this.
    expect(cut).toMatch(/overflow-wrap:\s*anywhere/);
  });

  // A FIXED-COLUMN FIGURE BLOCK IS REACHABLE.
  //
  // The turn page's prompt weights are five `.num-col` figures at 7rem under
  // their own headings, beside a phase tag: far past the 332px a phone leaves
  // inside the card, so the tokens column was cut off — and lining the five up
  // under their headings is the entire reason `.num-col` exists. (It was
  // already cut off at three, before the tool-definition array and a resumed
  // phase's own conversation each earned a column.)
  //
  // SIDEWAYS HERE, where a grid gets a card. The shapes take opposite answers
  // for a reason: a grid's flexible tracks resolve to zero the moment its wrap
  // becomes a scroller and its cells are prose, while a fixed figure column
  // has no such collapse and wrapping it would break the alignment outright —
  // the header row and each value row would break at different points.
  test("the figure block scrolls rather than cutting a column off", () => {
    const css = sheet("components.css");
    expect(block(css, ".num-block")).toMatch(/overflow-x:\s*auto/);
    // `max-content` IS THE HALF THAT MAKES IT WORK: a flex line inside a
    // scrollport is otherwise sized to the port, so its items shrink or
    // overflow and the headings stop lining up with the figures they head.
    const rows = block(css, ".num-rows");
    expect(rows).toMatch(/width:\s*max-content/);
    // And the min-width is what leaves the spacer able to push the figures
    // right on a screen the block already fits in.
    expect(rows).toMatch(/min-width:\s*100%/);
    // AND THE COLUMN ITSELF, which this test named as the entire reason the
    // block exists and then never read. Nothing in the tree asserted
    // `.num-col`'s own declarations: turning it from `flex: none` into a
    // flexible track — which is what every other cell in this file is —
    // collapses five figure columns onto their own text widths and leaves the
    // headings above them at five different x positions, with every gate
    // green. It is the same splay `.num-rows` exists to prevent, one level
    // down.
    const col = block(css, ".num-col");
    expect(col, "a column that can flex is not a column").toMatch(/flex:\s*none/);
    // A FLOOR, NOT THE VALUE. 7rem is derived from a RENDERED heading —
    // APPROX. TOKENS measures 102px at `--font-size-2xs` with `--font-letter-spacing-wide` — which
    // is font metrics and lives in no file, so a gate cannot re-derive it.
    // What it can hold is that the column did not shrink back under the
    // heading it was widened for. Measured by stepping the width: the heading
    // is two lines at 6.25rem and below and one from 6.4rem up, while every
    // figure under it sits on one at every width — so 6.5rem is the last
    // value that still clears it, and anything under that is the 5.5rem this
    // column was widened away from.
    const rem = /width:\s*(\d+(?:\.\d+)?)rem/.exec(col);
    expect(rem, ".num-col's width is what makes the figures a column").not.toBeNull();
    expect(Number(rem![1]), "APPROX. TOKENS wraps below 6.5rem").toBeGreaterThanOrEqual(6.5);
  });

  // ONE BOX FOR EVERY ROW, WHICH IS WHAT KEEPS THE COLUMNS COLUMNS.
  //
  // The width above used to sit on `.num-block > .row`, one row at a time, and
  // a row's own `max-content` is its own phase tag and its own chips. So the
  // moment the block was clamped — a phone — every row resolved to a DIFFERENT
  // width and the figures splayed: 12 KB, 23 KB and 5.3 KB at three x
  // positions under one heading. Sized together they scroll together.
  //
  // TWO-SIDED, because putting the width back on the rows is exactly how it
  // returns and neither half fails on its own: the rows' box has to carry it,
  // and nothing per-row may.
  //
  // THE FORBIDDEN HALF IS A PROPERTY, NOT A SPELLING. It used to test the
  // literal string `.num-block > .row`, which is where the width sat before
  // `.num-rows` existed — so the one spelling a regression would naturally
  // take today, `.num-rows > .row`, was the one the gate did not mention.
  // Measured: appending `.num-rows > .row { width: max-content }` restored
  // the splay in full and every test in this directory stayed green. A gate
  // keyed on a selector that has already been renamed once is a gate that
  // retires itself at the next rename.
  test("the rows of a figure block are sized as one box, not one at a time", () => {
    const css = sheet("components.css");
    const inside = [...mentioning(css, ".num-block"), ...mentioning(css, ".num-rows")];
    for (const { sel, body } of inside) {
      if (!/\.row(?![\w-])/.test(sel)) continue;
      expect(body, `${sel} sizes one row at a time — that is the splay`).not.toMatch(
        /(^|[\s;])(min-)?width\s*:/,
      );
    }
    expect(block(css, ".num-rows")).toMatch(/width:\s*max-content/);
  });

  // AND THE BLOCK IS SIZED TO ITS ROWS RATHER THAN TO THE PANEL.
  //
  // A `.spacer` puts a figure at the end of whatever box it is in, which is
  // right in a rail and wrong in a full-bleed card: the turn's prompt ledger
  // drew a phase tag at x=37 and its token count at x=645 on a 1570px screen,
  // with most of a panel of nothing in between, and a reader tracking a row
  // across that gap arrives at the wrong one.
  //
  // `fit-content`, NOT A CHOSEN CAP — which is what this was first written as.
  // A cap is a number invented to be wider than the content, so it still
  // leaves a gap, and it has to be re-invented the day a column is added.
  //
  // AND THE CEILING IS THE BOX, ASSERTED AS AN ABSENCE. The `max-width` read
  // `min(100%, 38rem)` and the rem half was that same invented cap under a
  // second property name: it cleared five columns at the 5.5rem `.num-col`
  // then had, `.num-col` went to 7rem so APPROX. TOKENS would stop wrapping,
  // and nobody re-derived 608px. It stopped being a ceiling and became the
  // layout — inside the 1163px a 1570px viewport leaves the card, the weights
  // block clamped to 608, scrolled 138px of itself out of sight and cut that
  // same heading down to `APPR…`.
  //
  // THE ABSENCE IS THE ONLY SIDE THIS SUITE CAN HOLD, and that is the finding
  // rather than a shortcut. The width such a number must clear is the column
  // count in Turn.tsx, `.num-col` here, a gap in a token package, WHICH ROW
  // the turn produced — a re-delivered self-iterating phase measures 746px
  // against a header's 684 — and the rendered width of a heading at
  // `--font-size-2xs`, which is font metrics and lives in no file at all. A gate
  // checking a literal against those needs a slack constant of its own, set
  // from the same measurement that set 38rem, and it would have stayed green
  // through this exact regression. So the rule is that there is no literal:
  // whatever its value, a length on this ceiling is the bug.
  //
  // `100%` IS NOT REDUNDANT WITH `fit-content` and must not be tidied away
  // with the rem: `.num-rows` is `width: max-content`, so this box's
  // min-content equals its max-content, the floor swallows the available term
  // and the block resolves to its content at every width — which costs it the
  // scroller, since `overflow-x: auto` only scrolls a box narrower than its
  // content. Measured with the line deleted: on a 390px phone the weights
  // block draws 746px out of a 332px card content box, `.app` and `.screen`
  // are both `overflow-x: hidden` so nothing takes the overflow, and three of
  // the five figure columns sit past x=390 with no way to reach them.
  //
  // AND THE ABSENCE IS ASSERTED OVER EVERY RULE THAT NAMES THE BLOCK, not
  // over the first one. Reading a single rule is how a forbidden declaration
  // walks back in under a second selector: measured, appending
  // `@media (min-width: 900px) { .num-block { max-width: 38rem } }` put the
  // 608px clamp back at exactly the widths the bug was visible at, and every
  // test in this directory stayed green.
  test("a figure block's only ceiling is the box it sits in", () => {
    const css = sheet("components.css");
    const b = block(css, ".num-block");
    expect(b).toMatch(/width:\s*fit-content/);
    expect(b, "`fit-content` alone resolves to max-content here — see .num-rows").toMatch(
      /max-width:\s*100%\s*;/,
    );
    for (const { sel, body } of mentioning(css, ".num-block")) {
      expect(
        body,
        `${sel}: a rem or px ceiling is a sixth number nothing can hold in step with the five it clears`,
      ).not.toMatch(/max-width:[^;]*\d+(\.\d+)?(rem|px|em|ch|vw|vmin)/);
    }
    // AND THE CLAMP IT REPLACED HAS TO LAND SOMEWHERE. Without this the gate
    // is one-sided — deleting the cell measure passes it, and one prose-long
    // note sizes the whole `max-content` row again, which is the x=37/x=645
    // gap restored from the other end. `.truncate` alone does not do it:
    // inside a `max-content` box every item is drawn at its full intrinsic
    // width, so the class is decoration until something bounds it.
    //
    // A FLOOR RATHER THAN THE SHAPE, for the reason the paragraph above gives
    // about `.num-block`'s own ceiling: `/\d+rem/` accepts a measure narrower
    // than the notes it exists to let through, and those notes are the whole
    // derivation. The three the blocks must render WHOLE — `the thread could
    // not be read from that node` at 248px is the widest, against 204px and
    // 198px — are what 20rem was set to clear, and telling them apart is why
    // the engine puts `thread_context_read` on the wire at all. Below 16rem
    // the widest of them is cut and the block starts lying about which state
    // it is in; the ceiling side is free, because the row's own figures bound
    // it.
    const measure = /max-width:\s*(\d+(?:\.\d+)?)rem/.exec(block(css, ".num-rows .truncate"));
    expect(measure, "the prose is the half of the row that can absorb a clamp").not.toBeNull();
    expect(
      Number(measure![1]),
      "a measure under 16rem cuts the diagnostic notes this block exists to tell apart",
    ).toBeGreaterThanOrEqual(16);
  });

  // AND A CAPPED CELL DOES NOT PAINT OUTSIDE ITS TRACK.
  //
  // `shrink` means the cell never wraps, and `SHRINK_CAP` in DataGrid.tsx means
  // its track can now be narrower than its content — so the two together make a
  // cell that spills sideways over the column beside it, and a flex row of tags
  // has no ellipsis to give. Measured on the schedules grid at 1000px: a 128px
  // track holding 164px of tags, the last one 36px into its neighbour.
  //
  // `clip` rather than `hidden`, for the same reason `.grid-wrap` gives: a
  // scrollport here is one nothing can scroll, and it would confine the sticky
  // head to the cell.
  test("a cell narrower than its content is cut, not spilled", () => {
    const shrink = rules(sheet("frame.css"), ".grid-cell.shrink").join(" ");
    expect(shrink).toMatch(/white-space:\s*nowrap/);
    expect(shrink).toMatch(/overflow:\s*clip/);
    expect(shrink, "`hidden` makes it a scrollport and kills the sticky head").not.toMatch(
      /overflow:\s*hidden/,
    );
  });

  // A RESET DOES NOT TAKE THE FOCUS RING WITH IT.
  //
  // `all: unset` is how a <button> becomes a plain box, and `all` is every
  // property — `outline` among them. So each of these rules sets
  // `outline-style: none`, and the kit baseline's `:focus-visible` then loses
  // a TIE rather than a fight: one pseudo-class against one class is the same
  // specificity, and frame.css is imported after the baseline. Visible to
  // somebody using the keyboard and to nobody else.
  //
  // Measured with a tab walk against a running engine: the rail's collapse
  // control took focus with no ring on EVERY screen in the product, and the
  // sidebar's twist on every screen that has a tree. Both are the kit's now;
  // what resets itself here is the grid's sort head, its column reset and a
  // histogram bar.
  //
  // TWO-SIDED, because the list is the whole mechanism: another `all: unset`
  // nobody adds here is a control that silently loses its ring, and a name
  // here whose reset is gone is a rule nobody will notice has died.
  test("every control that resets itself gets its focus ring back", () => {
    const css = sheet("frame.css");
    const bare = css.replace(/\/\*[\s\S]*?\*\//g, "");

    const reset = new Set<string>();
    for (const m of bare.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
      if (!/\ball:\s*unset/.test(m[2]!)) continue;
      for (const one of m[1]!.split(",")) reset.add(one.trim());
    }
    expect(reset.size, "nothing in this sheet resets itself any more").toBeGreaterThan(0);

    // The one rule that hands the ring back, and what it hands back: the
    // BASELINE's ring, outset by 2px. A control that draws an INSET ring of
    // its own (a sidebar card, whose outset ring the rail's edge would clip)
    // is not handing anything back, and is not counted here.
    const ring = new Set<string>();
    for (const m of bare.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
      if (!/outline:\s*2px solid var\(--color-focus\)/.test(m[2]!)) continue;
      if (!/outline-offset:\s*2px/.test(m[2]!)) continue;
      for (const one of m[1]!.split(",")) ring.add(one.trim().replace(/:focus-visible$/, ""));
    }

    expect(
      [...reset].filter((sel) => !ring.has(sel)),
      "these reset their outline away and never get it back",
    ).toEqual([]);
    expect(
      [...ring].filter((sel) => !reset.has(sel)),
      "these are named as needing the ring back but no longer reset it",
    ).toEqual([]);
  });

  // A ROW LIST LINES UP WITH ITS OWN HEADING.
  //
  // A row list usually sits in a `Card`, and `Card.Header` pads `--spacing-4`.
  // Every row class in the tree wrote `--spacing-3` instead, so on the turn page
  // four panels drew their content four pixels inside their own titles while
  // the panels between them did not, and the screen read as though two people
  // had built it. It is one token now, which is the only shape under which
  // six classes in two files can be said to agree.
  test("every row in the product takes the one inset", () => {
    const ROWS = [
      [".list-row", "components.css"],
      [".turn-row", "screens.css"],
      [".feed-row", "screens.css"],
      [".work-row", "screens.css"],
      [".wl-row", "screens.css"],
      [".thread-entry", "screens.css"],
    ] as const;
    for (const [row, file] of ROWS) {
      expect(block(sheet(file), row), `${row} writes its own inset`).toMatch(
        /padding:[^;]*var\(--row-inline\)/,
      );
    }
    // AND THE TOKEN IS THE HEADING'S OWN STEP. `Card.Header` is the design
    // system's and pads with the kit's own `--spacing-4`, which this tree reads
    // under the same name; a row list that took any other step would be back
    // to the misalignment with one place to change it instead of seven.
    expect(block(sheet("tokens.css"), ":root")).toMatch(/--row-inline:\s*var\(--spacing-4\)/);
  });

  // A DISCLOSURE HEAD WRAPS, which is rule 15 of the design doc.
  //
  // A phase's head is up to fifteen items — the phase tag, the iteration, a
  // delegated task id, a worker template, a role, then the decision, the round
  // cap, the empty-answer count, the rescue, the sandbox, the failure, the live
  // dot, the model, the rounds, the tokens, the duration and the time — and
  // every one is a tag or a `nowrap` figure that cannot break its own label.
  //
  // Nothing wrapped and `.phase-card` hides its overflow, so at 390px the head
  // was 690px of content in a 330px box: everything from the decision chip
  // rightward was CUT OFF rather than off-screen, a hidden box being a
  // scrollport whose offset nothing can move. The half that went is the half
  // the card is opened for.
  test("a turn and a phase head wrap rather than cutting their own tail off", () => {
    const css = sheet("screens.css");
    for (const head of [".turn-head", ".phase-head"]) {
      expect(rules(css, head).join(" "), `${head} does not wrap`).toMatch(/flex-wrap:\s*wrap/);
    }
  });

  // ONE CLASS, ONE LAYOUT. The stylesheets are one global namespace, so a
  // screen that names its header after a component's head quietly becomes
  // that component: the trace page called its header `.turn-head`, which is
  // every TurnCard's collapsible head, and a new `display: grid` rule for the
  // page restacked every card head on a seat's Turns tab one item per line
  // while the page header took the card's pointer and hover slab. Two
  // unconditional rules giving one class two different `display` values is
  // how that collision reads in a sheet — and so is one rule patching another
  // further down, which is a recipe nobody can read in one place.
  test("no class is given two different displays by two unconditional rules", () => {
    const seen = new Map<string, string[]>();
    for (const name of readdirSync(STYLES).filter((f) => f.endsWith(".css"))) {
      const css = sheet(name);
      let depth = 0;
      let start = 0;
      let open = 0;
      for (let i = 0; i < css.length; i++) {
        if (css[i] === "{") {
          if (depth === 0) open = i;
          depth++;
        } else if (css[i] === "}" && --depth === 0) {
          const selector = css.slice(start, open).trim();
          start = i + 1;
          // Top-level rules only: a media or container query is a
          // DELIBERATE second answer, for a width the first does not cover.
          if (selector.startsWith("@")) continue;
          const display = /(?:^|[;{\s])display:\s*([^;]+)/.exec(css.slice(open + 1, i));
          if (!display) continue;
          for (const one of selector.split(",").map((x) => x.trim())) {
            if (!/^\.[\w-]+$/.test(one)) continue;
            const at = `${display[1]!.trim()} (${name})`;
            seen.set(one, [...(seen.get(one) ?? []), at]);
          }
        }
      }
    }
    const clashes = [...seen].filter(
      ([, all]) => new Set(all.map((a) => a.replace(/ \(.*\)$/, ""))).size > 1,
    );
    expect(clashes).toEqual([]);
  });

  // AND WHAT IS INVISIBLE TAKES NO SPACE ANYWHERE.
  //
  // `.sr-only` is `position: absolute` with no offset, so its box sits at its
  // STATIC position — and an absolutely positioned box contributes to the
  // scrollable overflow of its containing block, which with no positioned
  // ancestor is the page. One inside a horizontal SCROLLER therefore sits at
  // the scroller's own content coordinate rather than anywhere the viewport
  // can see, and stretches the document to reach it.
  //
  // Measured on the tracker board at 390px: four 1px spans inside the second
  // lane's priority marks sat at x=573 and took the page to 574, so the
  // heading, the filters and the fixed bottom bar all dragged sideways to
  // reach a span nobody can see. The lanes themselves were contained the whole
  // time, which is what made it unreadable from the markup.
  test("a screen-reader-only box cannot stretch the page", () => {
    const only = block(sheet("base.css"), ".sr-only");
    expect(only).toMatch(/position:\s*absolute/);
    // BOTH AXES. A vertical scroller has the same hole, and an offset on one
    // axis leaves the box at its static position on the other.
    expect(only).toMatch(/left:\s*0/);
    expect(only).toMatch(/top:\s*0/);
  });

  // A TAB ROW SCROLLS RATHER THAN PUSHING THE WHOLE SCREEN SIDEWAYS.
  //
  // uilet states this rule for its underline row and not for its pill one: a
  // `.crewlet-tabs` is `display: inline-flex` and the pill variant is
  // `width: fit-content`, and neither caps at the container, because a flex
  // row of nowrap labels has a MIN-content width equal to the sum of them.
  //
  // Measured on the tracker at 390px: the view switcher stood 497px wide and
  // took the DOCUMENT to 642, so the reader dragged the page — heading,
  // filters and summary card with it — to reach a Table tab past the edge.
  // The set is data-driven there (`views.map`), so it is not five tabs by
  // construction: a founder who pins two more makes it worse at every width.
  //
  // `max-width` is the half that matters, and the reason `overflow-x` alone
  // changes nothing: without it the box is still as wide as its content and a
  // scrollport has nothing to hide.
  test("a pill tab row is bounded by its container, not by its content", () => {
    const pill = block(sheet("screens.css"), ".crewlet-tabs--pill");
    expect(pill).toMatch(/max-width:\s*100%/);
    expect(pill).toMatch(/overflow-x:\s*auto/);
  });

  // AND THE ONE TABLE LEFT IN THE PRODUCT STACKS THERE TOO.
  //
  // The retention screen's per-domain terms are a real `<table>` — a block
  // that already titled itself, with no header row and nothing to sort — and
  // four columns, two of them whole sentences, do not have 390px between them.
  // Measured at 536px wide inside a 390px page, with no scroller anywhere
  // above it, so 146px of every remedy was simply gone: on the one screen
  // whose subject is "what is blocking the trim and what do I do about it".
  //
  // A `<table>` IGNORES ITS CHILDREN'S DISPLAY unless the table itself stops
  // being one — blocking only the rows leaves the anonymous table box in place
  // and the columns come straight back — which is why all four levels are
  // asserted rather than just the row.
  test("the one remaining table stacks on a phone", () => {
    const css = sheet("components.css");
    const narrow = css.slice(css.indexOf(PHONE));
    expect(narrow, "components.css has no phone breakpoint").toContain(".table");
    for (const level of [".table", ".table tbody", ".table tr", ".table td"]) {
      expect(rules(narrow, level).join(" "), `${level} still lays out as a table at 390px`).toMatch(
        /display:\s*block/,
      );
    }
    // A RULE BETWEEN TERMS, since a row is no longer a line. Between rather
    // than under, so the last term does not draw one against the block's edge.
    expect(block(narrow, ".table tr + tr")).toMatch(/border-top:/);
  });

  // A FACT LINE IS A ROW OF FACTS, so it has to have a pitch. This one is a
  // flex row of column boxes, which means each fact is as wide as its widest
  // CHILD — and a note is prose where the value above it is a word.
  test("a footnote cannot set its fact's column or its row", () => {
    const css = sheet("frame.css");
    const line = block(css, ".fact-line");
    // A GRID WITH A REGULAR TRACK, not a flex row that sizes to content.
    // Measured before: `v1` under a 146px "set by Agent CEO · 2m ago" beside
    // three note-less facts at 66 to 75px, so the gaps between five labels ran
    // 85, 162, 90, 169 — five columns placed at random rather than one row.
    expect(line).toMatch(/display:\s*grid/);
    expect(line).toMatch(/grid-template-columns:\s*repeat\(/);
    // AND A TURN'S SEVEN FACTS FIT ONE LINE AT 1280, where the fact line is
    // 984px wide (measured): at an 8rem floor "Wall clock" took a row alone.
    const floor = Number(/minmax\(([\d.]+)rem/.exec(line)?.[1]) * 16;
    const gap = /column-gap:\s*var\(--spacing-4\)/.test(line) ? 16 : NaN;
    expect(7 * floor + 6 * gap).toBeLessThanOrEqual(984);

    // AND EVERY FACT SITS ON THE SAME TWO BANDS. Subgrid is what keeps the
    // labels on one line and the values on the next across the whole row: as
    // independent boxes, one three-line note pushed its own value up and left
    // the rest of the row hanging. TWO, not four: with a band per footnote a
    // neighbour's value wrapping to a second line pushed every note in the
    // row away from the value it qualifies — "37%", a blank line, then "of
    // 46.0k input read from cache" — so a note is its value's, in one cell.
    const fact = block(css, ".fact");
    expect(fact).toMatch(/grid-template-rows:\s*subgrid/);
    expect(fact).toMatch(/grid-row:\s*span 2/);
    expect(block(css, ".fact-body")).toMatch(/flex-direction:\s*column/);

    // AND THE CAP THAT USED TO STAND IN FOR ALL OF IT IS GONE. `max-width:
    // 16ch` could only narrow the problem — sixteen characters at `--font-size-2xs`
    // is still wider than most values — and left with the track doing the
    // bounding it would cut a note the column had room for.
    expect(
      rules(css, ".fact-note").join(" "),
      "the track bounds a note now; a cap on top of it only cuts one early",
    ).not.toMatch(/max-width:/);
  });

  test("the page bar wraps by what it holds, a phone's included, and folds there into More", () => {
    const css = sheet("frame.css");
    // THE BREAK'S OWN BODY, braces balanced — not everything after it. Sliced
    // to the end of the file, the "nothing else reorders at this break" case
    // below read every later `@media` rule as if it were inside this one, and
    // failed on the compact grid row's `order`, a phone rule about a different
    // element at a different query.
    const atBreak = atRuleBody(css, `@container page (width < ${PHONE_BREAKPOINT}px)`);
    const base = css.slice(0, css.indexOf("@container page ("));

    // A FLOOR RATHER THAN A HEIGHT, so the second line the bar can create is
    // not clipped, and a bar that needs none is exactly the kit's height.
    const bar = block(base, ".app .page-bar");
    expect(bar).toMatch(/flex-wrap:\s*wrap/);
    expect(bar).toMatch(/min-height:\s*var\(--page-bar-h\)/);
    expect(bar).toMatch(/height:\s*auto/);
    expect(
      mentioning(css, ".page-bar")
        .map((r) => r.body)
        .join(" "),
      "a fixed height clips the second line the bar creates",
    ).not.toMatch(/(^|[;\s])height:\s*var\(--page-bar-h\)/);

    // IT WRAPS BY WHAT IT HOLDS. A flex container assigns items to lines by
    // their UNSHRUNK size, so a trail at its content width broke the line as
    // soon as a long title and the controls exceeded the bar, with free space
    // left on the first line — and a width threshold measured on the busiest
    // bar forced the controls down on every screen under it, a sparse one
    // included. A basis of 0 is what makes the break depend on the trail's
    // FLOOR beside the controls; the grow factor hands the trail the rest.
    const crumbs = block(base, ".crumbs");
    expect(crumbs).toMatch(/flex:\s*1 100 0(?![.\d])/);
    // AND IT STOPS BEFORE THE WAY BACK OUT IS GONE: at the MEASURED floor
    // (`--crumb-floor`, the ancestors and the last crumb's stub, written by
    // `Breadcrumb`), never under 20ch — what the narrowest line can give,
    // measured at 178.8px on a 310px viewport — and never past the whole bar,
    // where a trail whose ancestry alone is wider has to scroll. A fixed 20ch
    // let a seat's trail share a phone's line with its button and cut its
    // unit crumb mid-letter.
    expect(crumbs).toMatch(
      /min-width:\s*min\(100%,\s*max\(20ch,\s*var\(--crumb-floor,\s*0px\)\)\)/,
    );
    // PAST ITS FLOOR IT SCROLLS rather than clipping as a box, on the inline
    // axis only — `overflow-x: auto` alone computes the block axis to `auto`
    // too, which puts a vertical scrollport on a one-line box.
    expect(crumbs, "the trail clips its overflow again").not.toMatch(/(^|[;\s])overflow:\s*hidden/);
    expect(crumbs).toMatch(/overflow-x:\s*auto/);
    expect(crumbs).toMatch(/overflow-y:\s*hidden/);
    expect(crumbs).toMatch(/overscroll-behavior-x:\s*contain/);
    expect(crumbs).toMatch(/scrollbar-width:\s*none/);
    expect(base, "the trail's scrollbar is hidden in one engine and drawn in the other").toMatch(
      /\.crumbs::-webkit-scrollbar\s*\{[^}]*display:\s*none/,
    );

    // THE CONTROL GROUP DOES NOT SHRINK BY SO MUCH AS A FRACTION: it wraps, so
    // a shrink drops its last control onto a row of its own. `max-width` is
    // the valve for a group wider than the whole bar. And on a line of its
    // own it keeps to the right-hand edge it holds on the first one.
    const controls = block(base, ".page-controls");
    expect(controls).toMatch(/flex:\s*0 0 auto/);
    expect(controls).toMatch(/max-width:\s*100%/);
    expect(controls).toMatch(/flex-wrap:\s*wrap/);
    expect(controls).toMatch(/justify-content:\s*flex-end/);
    expect(controls).toMatch(/margin-left:\s*auto/);

    // THE PHONE BREAK IS A CONTAINER QUERY, because what overflows is the
    // HEADER and a viewport query cannot see a sidebar or a peek beside it.
    expect(block(base, ".page-head"), "the header is no longer a container").toMatch(
      /container:\s*page \/ inline-size/,
    );
    expect(atBreak, "the page bar's own break is gone").toContain(".page-controls");
    const broken = block(atBreak, ".page-controls");
    // BY WHAT IT HOLDS ON A PHONE TOO: the controls share the trail's or the
    // lenses' line when they fit and take one of their own when they do not.
    // A line of their own BY RULE was right for five controls; folded into
    // "More", a task's controls are one button, and a line for one ellipsis
    // was a row of chrome with nothing on it.
    expect(broken).toMatch(/flex:\s*0 0 auto/);
    expect(broken, "the controls take a line of their own by rule again").not.toMatch(
      /flex:\s*0 0 100%/,
    );
    // PAST EVERYTHING ELSE ON THE BAR, WHICH IS A COMPARISON AND NOT A SHAPE:
    // `order: 0` is the initial value and puts the controls back where
    // document order had them.
    const order = /order:\s*(-?\d+)/.exec(broken);
    expect(order, "the controls carry no order, so they break where they sit").not.toBeNull();
    expect(Number(order![1]), "order 0 is where document order already put them").toBeGreaterThan(
      0,
    );
    const others = [...atBreak.matchAll(/([^{}]+)\{([^{}]*)\}/g)].filter(
      (m) => /(^|[\s;])order\s*:/.test(m[2]!) && !/\.page-controls/.test(m[1]!),
    );
    expect(others.map((m) => m[1]!.trim())).toEqual([]);
    // AND A LINE OF CONTROLS WIDER THAN A PHONE SCROLLS rather than stacking
    // rows that push the screen a third of the way down.
    expect(broken).toMatch(/overflow-x:\s*auto/);
    expect(broken).toMatch(/flex-wrap:\s*nowrap/);

    // ONE ACTION IN VIEW, THE REST IN "MORE" — and never both at once: the
    // menu is not drawn above the break, and what it holds is not drawn
    // inline below it. The frame's pair is one element only so this can fold
    // it; above the break it lays out as if it had no wrapper.
    expect(block(base, ".page-more")).toMatch(/display:\s*none/);
    expect(block(base, ".page-frame-actions")).toMatch(/display:\s*contents/);
    expect(block(atBreak, ".page-more")).toMatch(/display:\s*inline-flex/);
    expect(atBreak).toMatch(/\.page-frame-actions,\s*\.page-action-folds\s*\{[^}]*display:\s*none/);
  });

  // THE PANEL TITLE'S CAP IS GONE WITH THE PANEL, deliberately and not by
  // oversight. It asserted `flex: 0 0 auto` and `max-width: 100%` on
  // `.panel-head > .panel-title`, and Panel is one of the fifteen components
  // `@crewlethq/ui` replaced — the recipe, the class and the element are all
  // deleted, so there is no markup left to repoint the assertion at.
  //
  // Its two claims did not survive together, which is the actual reason this
  // is a deletion rather than a rewrite:
  //
  //  - "The title never gives way first" SURVIVES, and it is uilet's now,
  //    made the other way round: `.crewlet-card__subtitle { flex-shrink: 100 }`
  //    lets the subtitle absorb the shrink instead of pinning the title at a
  //    flex factor of 0. Restating that here would be a second idea of one
  //    rule with nothing comparing the two — the failure `textcut` and
  //    `whsec` are named after in this repository — and the package states it
  //    beside the value, where a bump moves both at once.
  //  - "A long one ELLIPSES" does NOT survive. uilet's `.crewlet-card__title`
  //    is `white-space: nowrap` with no `text-overflow`, and the clip is on
  //    `.crewlet-card__header-main`, so a title with nowhere left to go is CUT
  //    at the row's end. That is a real difference from what this rule drew
  //    and it belongs to whoever owns the Card call sites; a stylesheet rule
  //    cannot fix it from here, because `text-overflow` on a flex container
  //    does not reach its flex items.

  test("the toolbar's band is a real height, and only a screen with one offsets", () => {
    // `--sticky-top` is what the heads above are offset by, so the band has to
    // have a height — and a screen with no toolbar must keep 0, or every
    // header on it parks below a strip of scrolling page.
    const shell = sheet("frame.css");
    expect(block(shell, ".toolbar")).toMatch(/min-height:\s*var\(--toolbar-h\)/);
    expect(block(shell, ".crewlet-app-shell__main:has(.toolbar)")).toMatch(
      /--sticky-top:\s*var\(--toolbar-h\)/,
    );
    expect(sheet("tokens.css")).toMatch(/--sticky-top:\s*0px/);
  });
});

test("no stylesheet negates a var() with a unary minus", () => {
  // `left: -var(--spacing-1)` is not CSS. There is no unary minus in front of a
  // function, so the value is invalid at computed-value time and the WHOLE
  // declaration is dropped — silently, with the property falling back to its
  // initial value. It shipped on `.rail-row.active::before`, where `left:
  // auto` resolves to the static position and painted the active workspace's
  // accent bar straight down the middle of its icon. The spelling that works
  // is `calc(-1 * var(--x))`.
  // EVERY SHEET IN THE DIRECTORY, read rather than listed. The five names
  // that used to be written here were a list nobody was going to extend:
  // `tokens.css` and `fonts.css` were never in it, and a sheet
  // added tomorrow would not be either — so the one check that catches this
  // would silently stop covering the file it was added for.
  const found: string[] = [];
  for (const name of readdirSync(STYLES).filter((f) => f.endsWith(".css"))) {
    const text = sheet(name);
    for (const m of text.matchAll(/[:\s(]-var\(/g)) {
      found.push(`${name}:${text.slice(0, m.index).split("\n").length}`);
    }
  }
  expect(found, "a minus in front of var() drops the declaration — use calc(-1 * …)").toEqual([]);
});

describe("a task's activity", () => {
  // AN ENTRY IS AS LONG AS THE CHANGE WAS.
  //
  // The work item's history row once held its description in `.truncate`: one
  // line, ellipsed. A create moves every field `TaskDeltas` compares and the
  // sentence renders a clause each, so the cut landed inside `Status: ` — and
  // took the `turn →` link with it, clipped out of sight and still in the tab
  // order. The task page's activity keeps the rule: every line a change, a
  // turn's account or a reviewer's request is written in WRAPS.
  //
  // IN THE SHEET because there is nowhere else: jsdom computes no layout, so
  // every case that renders these stays green whether the text wraps or
  // ellipses. The other direction is covered already — each class is declared
  // here and written only by `routes/work/item/Activity.tsx`, so
  // classes.test.ts fails if either side alone goes back.
  test("a task's activity wraps rather than ellipsing what changed", () => {
    const css = sheet("screens.css");
    for (const selector of [".task-entry-line p", ".task-turn-summary", ".task-turn-review p"]) {
      const rule = block(css, selector);
      expect(rule, selector).toMatch(/overflow-wrap:\s*anywhere/);
      // The three halves of `.truncate`, none of which may come back here.
      expect(rule, selector).not.toMatch(/white-space:\s*nowrap/);
      expect(rule, selector).not.toMatch(/text-overflow/);
      expect(rule, selector).not.toMatch(/overflow:\s*hidden/);
    }
  });
});

// A GRID ROW WITH TWO CONTENT-SIZED TRACKS AT THE END DECLARES A GAP.
//
// `.work-hist-row` shipped `18px minmax(0, 1fr) auto auto` with no gap, and on
// every entry that carried both the row read "quiet5m ago": two `auto` tracks
// are sized to their own content and butt against each other, with no leading,
// no padding and no border between them. One `auto` cannot show it — it sits
// against a flexible track or the row's edge — and a FIXED track hides it,
// which is what happened here: the 18px glyph column held a 12px mark, and the
// 6px of slack inside it read as a gap that was never declared.
//
// IN THE SHEET, because that is the only place it is visible. jsdom computes
// no layout, so every suite that renders one of these rows stays green at any
// value; a screenshot is the only other witness, and this one survived one.
//
// THE GAP MAY COME FROM THE BASE RULE, which is why this looks the class up
// rather than reading one body: `.work-rows[data-ordinals="true"]` restates
// the track list to insert an ordinal column and takes its gap from
// `.work-rows`, which is correct and is not a second declaration to keep in
// step.
test("a grid row with more than one content-sized track declares a gap", () => {
  const offenders: string[] = [];
  for (const name of readdirSync(STYLES).filter((f) => f.endsWith(".css"))) {
    const css = sheet(name).replace(/\/\*[\s\S]*?\*\//g, "");
    const rulesIn = [...css.matchAll(/([^{}]+)\{([^{}]*)\}/g)];
    const gapped = new Set<string>();
    for (const rule of rulesIn) {
      if (!/(?:^|[;{\s])(?:gap|column-gap|grid-column-gap)\s*:/.test(rule[2]!)) continue;
      for (const one of rule[1]!.split(",")) gapped.add(one.trim());
    }
    for (const rule of rulesIn) {
      const tracks = /grid-template-columns:\s*([^;]+)/.exec(rule[2]!);
      if (!tracks) continue;
      // FUNCTIONS FIRST: `minmax(auto, 1fr)` and `repeat(auto-fill, …)` both
      // spell the word and neither is a bare track, so they are collapsed
      // before the count.
      const bare = tracks[1]!
        .replace(/[a-z-]+\([^()]*\)/g, "X")
        .trim()
        .split(/\s+/);
      if (bare.filter((track) => track === "auto").length < 2) continue;
      for (const one of rule[1]!.split(",")) {
        const sel = one.trim();
        // THE SELECTOR'S OWN LEADING COMPOUND, so a variant that only
        // restates the tracks is answered by the rule it varies.
        const base = /^(\.[\w-]+)/.exec(sel)?.[1];
        if (gapped.has(sel) || (base && gapped.has(base))) continue;
        offenders.push(`${name}: ${sel}`);
      }
    }
  }
  expect(
    offenders,
    "two `auto` tracks butt against each other — declare a gap, as `.work-feed-row` does",
  ).toEqual([]);
});

// ONE GAP BETWEEN EVERY CRUMB. The 5ch floor that keeps the object's own
// crumb an ellipsised stub rather than nothing was on `.crumb-here`, which
// also draws an ancestor with no page of its own — "Item", "seats" — and a
// floor on a part that never shrinks only pads a short word: "Work / Item   /
// items". Scoped to the last part, which is the only one that gives way.
// THE ELLIPSIS NEEDS A BOX OF ITS OWN. `.crumb-link` and `.crumb-here` are flex
// containers, and text directly inside one is an anonymous flex item that
// `text-overflow` never reaches: the page's h1 was cut mid-word at the bar's
// edge. `PageHeader.test.tsx` holds that every label is inside `.crumb-text`;
// this holds that `.crumb-text` is the box that cuts with an ellipsis.
test("a crumb's words are the box that ellipsises", () => {
  const css = sheet("frame.css");
  const text = block(css, ".crumb-text");
  expect(text).toMatch(/min-width:\s*0\b/);
  expect(text).toMatch(/overflow:\s*hidden/);
  expect(text).toMatch(/white-space:\s*nowrap/);
  expect(text).toMatch(/text-overflow:\s*ellipsis/);
  expect(text, "a flex or grid box here would be the anonymous item again").not.toMatch(
    /display:\s*(inline-)?(flex|grid)/,
  );
});

// A LINK INSIDE A CLAMPED FACT WRAPS. `.t-link` is `nowrap`, and inside the
// fact's two-line clamp a linked unit chain was one line cut at the track's
// edge with no ellipsis.
test("a linked fact wraps with its clamp", () => {
  const css = sheet("frame.css");
  expect(block(css, ".fact-value .t-link")).toMatch(/white-space:\s*normal/);
});

test("the crumb floor is the last part's alone", () => {
  const css = sheet("frame.css");
  // `min-width: 0` is not a floor — it is what lets the ellipsis happen — so
  // the rule forbids any OTHER value.
  expect(block(css, ".crumb-here")).not.toMatch(/min-width:\s*(?!0\b)[^;\s]/);
  expect(block(css, ".crumb-part:last-child > .crumb-here")).toMatch(/min-width:\s*5ch/);
});

// A TIME-SERIES BAR IS A BAR, NOT ITS SLOT. Filled edge to edge, a one-day
// spend window was one column the width of the card — a solid block. The bar
// takes the kit's own proportion (63% of the slot, capped at `--spacing-7`,
// as `.crewlet-stacked-columns__column` does), centred, and the ghost sits
// over the bar rather than the slot; and a segment that exists has a pixel.
test("a stacked time-series column is capped and centred in its slot", () => {
  const css = sheet("components.css");
  expect(block(css, ".stackseries-col")).toMatch(/justify-content:\s*center/);
  for (const sel of [".stackseries-stack", ".stackseries-ghost"]) {
    const b = block(css, sel);
    expect(b, sel).toMatch(/width:\s*63%/);
    expect(b, sel).toMatch(/max-width:\s*var\(--spacing-7\)/);
  }
  expect(block(css, ".stackseries-stack > span")).toMatch(/min-height:\s*1px/);
});

// A HISTOGRAM BAR IS A BAR TOO, AND IT IS DATA. The activity log's time axis
// filled its whole slot, so a week holding one busy day drew a block a seventh
// of the card wide, and it painted that block in the accent the kit reserves
// for HERE/ACT. The fill takes the same capped, centred proportion as the
// spend columns, is drawn on the data ramp, and an empty bucket's floor is a
// tone that shows on the card rung rather than one within a shade of it.
test("a histogram bar is a capped data-coloured fill centred in its slot", () => {
  const css = sheet("frame.css");
  expect(block(css, ".histogram-bar")).toMatch(/justify-content:\s*center/);
  expect(block(css, ".histogram-bar")).not.toMatch(/background/);
  const fill = block(css, ".histogram-fill");
  expect(fill).toMatch(/width:\s*63%/);
  expect(fill).toMatch(/max-width:\s*var\(--spacing-7\)/);
  expect(fill).toMatch(/background:\s*var\(--color-data-1\)/);
  expect(block(css, ".histogram-bar[data-empty] > .histogram-fill")).toMatch(
    /background:\s*var\(--color-border-strong\)/,
  );
});

// A UNIT'S NAME IS WHAT ITS HEAD SAYS, SO IT IS NOT WHAT GIVES WAY. One
// unwrapped row shrank the name alone — the only part with text that could —
// and a phone drew "Developer Relations" in 18px beside a full kind tag, lead
// chip and count. The head wraps, and the name keeps its own width.
test("an org unit's head wraps rather than shrinking its name", () => {
  const css = sheet("screens.css");
  expect(block(css, ".org-unit-head")).toMatch(/flex-wrap:\s*wrap/);
  const name = block(css, ".org-unit-name");
  expect(name).toMatch(/flex:\s*0 0 auto/);
  expect(name).toMatch(/max-width:\s*100%/);
  expect(block(css, ".org-unit-count")).toMatch(/white-space:\s*nowrap/);
});

// A SEGMENTED GROUP WIDER THAN ITS CONTAINER WRAPS. Its options never shrink,
// so on one unbroken row a phone clipped the spend chart's "Split by" at
// "Worke", with the rest of the choice unreachable.
test("a segmented control wraps within its container rather than overflowing it", () => {
  const b = block(sheet("components.css"), ".segmented");
  expect(b).toMatch(/flex-wrap:\s*wrap/);
  expect(b).toMatch(/max-width:\s*100%/);
});

// A STAT'S SECOND LINE KEEPS ITS ENDING. The kit's sub is one `nowrap` line
// that ellipsises, which cut "6 seats idle and waiting for work" on a phone and
// a caption at 1440 — the qualifying half of the sentence is its end.
test("a stat card's sub wraps rather than ellipsising", () => {
  expect(block(sheet("screens.css"), ".crewlet-statcard__sub")).toMatch(/white-space:\s*normal/);
});

// A CARD'S TITLE DOES NOT GIVE WAY WHILE ITS SUBTITLE CAN. The kit's 100-to-1
// shrink is proportional, so a long subtitle on a phone still took a quarter
// pixel off the name block and the title's ellipsis drew "When" as "Wh…".
// Where the head holds nothing else rigid the name block is out of the shrink,
// with `max-width` as the valve for a title longer than the whole head.
test("a card's name block is out of the shrink beside a subtitle alone", () => {
  const css = sheet("screens.css").replace(/\s+/g, " ");
  const m =
    /\.crewlet-card__header-main:not\( :has\(~ \.crewlet-card__header-actions, ~ \.crewlet-card__header-chrome:not\(:empty\)\) \) \{([^}]*)\}/.exec(
      css,
    );
  expect(m, "the rule is gone").not.toBeNull();
  expect(m![1]).toMatch(/flex-shrink:\s*0/);
  expect(m![1]).toMatch(/max-width:\s*100%/);
});

// WORK › SEARCH'S PHRASE KEEPS THE WIDTH A PHRASE IS READ AT. The field, the
// three modes and Search shared one row that could not wrap, so at 390px the
// field was what gave way — measured at 20px, a magnifier with no phrase in
// it. The form wraps by what it holds, and the field's floor is what decides
// when: it takes a line of its own rather than going under 18rem.
test("the work search form wraps before its phrase field goes under a readable width", () => {
  const css = sheet("screens.css");
  expect(block(css, ".work-search-form")).toMatch(/flex-wrap:\s*wrap/);
  const phrase = block(css, ".work-search-phrase");
  expect(phrase).toMatch(/flex:\s*1 1 18rem/);
  expect(phrase).toMatch(/min-width:\s*min\(100%,\s*18rem\)/);
});

// AN OBJECT'S LENSES SIT BESIDE ITS NAME, and a page without any costs the bar
// nothing. The empty slot is no flex item (else it is a gap before the
// controls on every page), and with lenses the TRAIL stops taking the slack —
// its grow would push the lenses against the controls at the far end, away
// from the name they are lenses on — and is capped so a long name ellipsises
// beside them rather than pushing them onto a line of their own.
test("the lens slot vanishes when empty and takes the slack beside the trail when not", () => {
  const css = sheet("frame.css");
  expect(block(css, ".page-lenses:empty")).toMatch(/display:\s*none/);
  expect(block(css, ".page-lenses")).toMatch(/flex:\s*1 0 auto/);
  const trail = block(css, ".page-bar:has(> .page-lenses:not(:empty)) > .crumbs");
  expect(trail).toMatch(/flex:\s*0 100 auto/);
  expect(trail).toMatch(/max-width:\s*60%/);
});

// AN OBJECT'S TAB STRIP IS AS WIDE AS WHAT HOLDS IT. `ObjectTabs` folds the
// tabs that do not fit by measuring the box it is given, so a box sized by
// its own content measures ITSELF: the turn trace's strip, set
// `justify-self: start` in its header grid, was measured before its Tools
// count and its web font arrived, folded three tabs into "More", shrank to
// "Timeline | More" — and the observer never saw room to unfold. Any rule on
// the strip's own box, or on a class a screen hands it, that shrinks it to
// its content repeats that. jsdom has no layout, so this is where it shows.
test("an object tab strip spans its container rather than shrinking to its tabs", () => {
  const src = fileURLToPath(new URL("..", import.meta.url));
  const classes = new Set([".object-tabs"]);
  for (const file of readdirSync(src, { recursive: true }) as string[]) {
    if (!file.endsWith(".tsx") || file.endsWith(".test.tsx")) continue;
    const text = readFileSync(join(src, file), "utf8");
    for (const m of text.matchAll(/<ObjectTabs\b/g)) {
      const tag = text.slice(m.index, text.indexOf("/>", m.index));
      const named = /className="([^"]+)"/.exec(tag)?.[1] ?? "";
      for (const c of named.split(/\s+/).filter(Boolean)) classes.add(`.${c}`);
    }
  }
  // The profile's and the trace's, at least — a reader that found none has
  // stopped reading the screens, and would pass everything.
  expect(classes).toContain(".prof-tabs");
  expect(classes).toContain(".trace-tabs");

  const SHRINKS = [
    /justify-self:\s*(start|end|center|flex-start|flex-end|self-start|self-end|left|right)/,
    /align-self:\s*(start|end|center|flex-start|flex-end|baseline)/,
    /(^|[;\s])width:\s*(max-content|min-content|fit-content)/,
    /display:\s*inline/,
    /float:/,
    /margin-inline(-start|-end)?:\s*[^;]*auto/,
  ];
  const offenders: string[] = [];
  for (const name of readdirSync(STYLES).filter((f) => f.endsWith(".css"))) {
    const css = sheet(name);
    for (const m of css.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
      for (const sel of m[1]!.split(",")) {
        // THE STRIP'S OWN BOX: the class in the selector's LAST compound.
        // `.object-tabs > .object-tabs-strip` is the row inside it, which is
        // meant to be as wide as its tabs.
        const subject =
          sel
            .trim()
            .split(/[\s>+~]+/)
            .pop() ?? "";
        const hit = [...classes].find((c) =>
          new RegExp(`${c.replace(/[.]/g, "\\.")}(?![\\w-])`).test(subject),
        );
        if (!hit) continue;
        const bad = SHRINKS.find((re) => re.test(m[2]!));
        if (bad) offenders.push(`${name}: ${sel.trim()} { ${bad.source} }`);
      }
    }
  }
  expect(offenders, "a tab strip shrunk to its content folds tabs it has room for").toEqual([]);
});

// A SPAN OPENS BESIDE THE WATERFALL ON A LAPTOP. The page column at 1280 is
// about 1024px (the fact line inside it measures 984, above), and the
// threshold sat at 1100 — so on the one width a laptop has, the detail went
// UNDER sixteen rows of waterfall, below the fold, and a click on a span
// seemed to do nothing. And not so low that the waterfall beside a 380px
// detail loses its bars: its label floor is 140px and its length 64.
test("the span detail sits beside the waterfall at a laptop's page width", () => {
  const css = sheet("screens.css");
  const at = [
    ...css.matchAll(/@container page \(width >= (\d+)px\)\s*\{\s*\.trace-timeline\.has-detail/g),
  ];
  expect(at, "the side-by-side rule is not where this reads it").toHaveLength(1);
  const threshold = Number(at[0]![1]);
  const LAPTOP_PAGE = 1024;
  expect(threshold).toBeLessThanOrEqual(LAPTOP_PAGE);
  const detail = Number(
    /minmax\(\d+px,\s*(\d+)px\)/.exec(
      atRuleBody(css, `@container page (width >= ${threshold}px)`),
    )?.[1],
  );
  expect(detail).toBe(380);
  // What the bars keep beside the widest detail at the threshold: the row's
  // padding, the label's floor, the length and two gaps come off first.
  const bars = threshold - detail - 16 - 2 * 12 - 140 - 64 - 2 * 12;
  expect(bars, "the waterfall beside the detail has no room for its bars").toBeGreaterThanOrEqual(
    240,
  );
});
