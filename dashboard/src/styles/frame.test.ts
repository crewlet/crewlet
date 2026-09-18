// @vitest-environment node
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "vitest";

/**
 * The chrome's layout rules that fail SILENTLY and correctly in every other
 * gate — frame.css, shell.css, the shared panel in components.css, and the
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

function sheet(name: string): string {
  return readFileSync(join(STYLES, name), "utf8");
}

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
    // THE WIDE LAYOUT ALONE. Below the drawer breakpoint a row is deliberately
    // NOT a row — the heads go and each cell draws its own label — so the
    // narrow block unwinds this chain on purpose and would read here as the
    // very drift this case exists to catch. See "a grid becomes labelled
    // cards" above for the other half.
    const css = sheet("frame.css").split("@media (max-width: 860px)")[0]!;
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

  test("the peek column exists only above the drawer threshold", () => {
    // Below it the rail is `position: fixed` — and a fixed grid child does NOT
    // shrink an explicitly sized track, so an unconditional fourth track
    // reserved 420px of empty page at exactly the widths the drawer exists to
    // rescue. The guard is a literal because a custom property cannot be read
    // in a media query, which is the whole reason this assertion exists: the
    // two numbers have to move together and nothing else holds them.
    const css = sheet("frame.css");
    const drawerMax = /--peek-drawer-max:\s*(\d+)px/.exec(css);
    expect(drawerMax, "--peek-drawer-max is not declared").not.toBeNull();

    const guard = /@media \(min-width:\s*(\d+)px\)\s*\{\s*\.app\[data-peek="true"\]/.exec(css);
    expect(guard, "the peek tracks are not behind a min-width guard").not.toBeNull();
    expect(Number(guard![1])).toBe(Number(drawerMax![1]) + 1);

    // And every peek template is inside it: one left outside would out-specify
    // the narrow two-column reset and keep both empty tracks on its own.
    const guarded = css.slice(guard!.index, css.indexOf("\n}", guard!.index));
    expect(guarded.match(/\.app\[data-peek="true"\]/g) ?? []).toHaveLength(
      (css.match(/\.app\[data-peek="true"\]/g) ?? []).length,
    );
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
      /top:\s*calc\(var\(--sticky-top\) \+ var\(--row-h\)\)/,
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
  test("a screen that asked for the height is a column that can give it one", () => {
    const css = sheet("shell.css");
    const fill = block(css, ".screen[data-fill]");
    expect(fill).toMatch(/display:\s*flex/);
    expect(fill).toMatch(/flex-direction:\s*column/);
    expect(fill).toMatch(/overflow-y:\s*hidden/);
    // AND THE COLUMN STILL ASKS FOR WHAT IS LEFT. Either half alone is a
    // scroller that does not scroll and a canvas with nothing to fill.
    const inner = block(css, ".screen[data-fill] > .screen-inner");
    expect(inner).toMatch(/flex:\s*1 1 auto/);
    expect(inner).toMatch(/min-height:\s*0/);
  });

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
  test("a grid becomes labelled cards below the drawer breakpoint", () => {
    const css = sheet("frame.css");
    const narrow = css.slice(css.indexOf("@media (max-width: 860px)"));

    // THE SUBGRID CHAIN IS UNWOUND. The wide layout is one grid whose head,
    // body, bands and rows adopt its tracks; a card is the opposite shape, so
    // each goes back to a block and the row becomes its own two-column grid.
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
    // AND THE LABELS LINE UP without a shared track: a fixed flex basis is
    // what a flex line has instead of a grid column.
    expect(narrow).toMatch(/content:\s*attr\(data-label\)/);
    expect(narrow.slice(narrow.indexOf(".grid-cell[data-label]::before"))).toMatch(
      /flex:\s*0 0 \d/,
    );
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
    const narrow = css.slice(css.indexOf("@media (max-width: 860px)"));

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

  // NOTHING IN THE CHROME SITS OUTSIDE A PHONE'S VIEWPORT.
  //
  // The page bar is one non-wrapping row whose middle — a screen's own control
  // group, portalled in — was `flex: 0 0 auto`: 364px on the audit and 478px
  // on the turns list, inside a 390px viewport, shoving everything after it
  // off the right edge of a bar whose `overflow` is `visible`. Off the edge
  // meant UNREACHABLE: the star, Copy link, the viewer chip and the search
  // trigger — the command palette's only pointer affordance — were all past
  // x=390 with nothing to scroll. The trail shrank to exactly 0px wide.
  //
  // Asserted in the SHEET for the same reason as everything else in this file:
  // jsdom computes no layout, and a media query is invisible to every suite
  // that renders the frame.
  // A CLOSED DRAWER IS OUT OF REACH, NOT MERELY OUT OF SIGHT.
  //
  // The workspace sidebar becomes a drawer under 960px and leaves on a
  // `transform`, which moves pixels and nothing else. Closed, it still held
  // EIGHT TAB STOPS — the filter box, the four section links and every project
  // in the tree — so a reader tabbing the page lost focus into a panel 260px
  // off the left edge with no scroll that could reach it, and a screen reader
  // read out a whole navigation nobody could see. On twenty of the
  // twenty-four screens.
  //
  // Asserted in the SHEET because that is where the fact lives: jsdom computes
  // no layout, applies no media query and gives `visibility` no effect on
  // `document.activeElement`, so a render test would pass either way.
  test("the drawer that is closed is out of the focus order, not just off-screen", () => {
    const css = sheet("frame.css");
    const narrow = css.slice(css.indexOf("@media (max-width: 960px)"));
    const shut = block(narrow, ".workspace-side");
    const open = block(narrow, '.workspace-side[data-open="true"]');

    // VISIBILITY IS THE PROPERTY THAT DOES IT. `opacity` and `transform` leave
    // a subtree focusable; `display` would skip the slide outright.
    expect(shut).toMatch(/visibility:\s*hidden/);
    expect(open).toMatch(/visibility:\s*visible/);

    // AND IT IS DELAYED ONE WAY AND NOT THE OTHER, which is the whole reason
    // it can sit beside a slide: hidden only once the drawer has finished
    // leaving, visible the moment it starts arriving.
    expect(shut).toMatch(/visibility 0s linear var\(--dur\)/);
    expect(open).toMatch(/visibility 0s(?!\s+linear)/);
    // A SHORTHAND STILL CARRIES THE SLIDE. Writing `transition: visibility ...`
    // alone would replace it and the drawer would jump.
    expect(shut).toMatch(/transform var\(--dur\) var\(--ease\)/);
    expect(open).toMatch(/transform var\(--dur\) var\(--ease\)/);
  });

  // A FIXED-COLUMN FIGURE BLOCK IS REACHABLE.
  //
  // The turn page's prompt weights are three `.num-col` figures at 5.5rem
  // under their own headings, beside a phase tag: 370px in the 332px a phone
  // leaves inside the card, so the tokens column was cut off — and lining the
  // three up under their headings is the entire reason `.num-col` exists.
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
    const row = block(css, ".num-block > .row");
    expect(row).toMatch(/width:\s*max-content/);
    // And the min-width is what leaves the spacer able to push the figures
    // right on a screen the block already fits in.
    expect(row).toMatch(/min-width:\s*100%/);
  });

  // A RESET DOES NOT TAKE THE FOCUS RING WITH IT.
  //
  // `all: unset` is how a <button> becomes a plain box, and `all` is every
  // property — `outline` among them. So each of these rules sets
  // `outline-style: none`, and `base.css`'s `:focus-visible` then loses a TIE
  // rather than a fight: one pseudo-class against one class is the same
  // specificity, and frame.css is imported second. The same failure the
  // comment above that `:focus-visible` already describes for the design
  // system's components — visible to somebody using the keyboard and to
  // nobody else.
  //
  // Measured with a tab walk against a running engine: the rail's collapse
  // control took focus with no ring on EVERY screen in the product, and the
  // sidebar's twist on every screen that has a tree.
  //
  // TWO-SIDED, because the list is the whole mechanism: a sixth `all: unset`
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
    expect(reset.size, "nothing in this sheet resets itself any more").toBeGreaterThan(3);

    // The one rule that hands the ring back, and what it hands back.
    const ring = new Set<string>();
    for (const m of bare.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
      if (!/outline:\s*2px solid var\(--focus\)/.test(m[2]!)) continue;
      expect(m[2]!, "an offset ring is what the baseline draws").toMatch(/outline-offset:/);
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
    const narrow = css.slice(css.indexOf("@media (max-width: 860px)"));
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

  test("the page bar wraps rather than pushing the chrome off a phone", () => {
    const css = sheet("frame.css");
    const narrow = css.slice(css.indexOf("@media (max-width: 860px)"));
    expect(narrow, "the narrow breakpoint is gone").toContain(".page-bar");

    // THE BAR IS TWO LINES HERE, and a fixed height would clip the second.
    const bar = block(narrow, ".page-bar");
    expect(bar).toMatch(/flex-wrap:\s*wrap/);
    expect(bar).toMatch(/min-height:\s*var\(--page-bar-h\)/);
    expect(bar).toMatch(/height:\s*auto/);

    // AND THE SECOND LINE IS THE PAGE'S OWN CONTROLS: a full basis is what
    // breaks the line, an order past the globals is what keeps the viewer
    // chip and the search trigger on the first one, and the scroll is what
    // makes a group wider than the phone reachable rather than merely
    // out of sight.
    const controls = block(narrow, ".page-controls");
    expect(controls).toMatch(/flex:\s*0 0 100%/);
    expect(controls).toMatch(/order:\s*\d/);
    expect(controls).toMatch(/overflow-x:\s*auto/);

    // AND THE BOTTOM BAR IS NAVIGATION. Its foot carried the engine pill, the
    // theme switch and the density switch — 250px of 390, leaving about two
    // and a half of eight destinations on screen. Both settings are in the
    // command palette, and the collapsed rail already drops them.
    expect(narrow).toContain(".rail-foot > .crewlet-segmented");
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
    const shell = sheet("shell.css");
    expect(block(shell, ".toolbar")).toMatch(/min-height:\s*var\(--toolbar-h\)/);
    expect(block(shell, ".screen:has(.toolbar)")).toMatch(/--sticky-top:\s*var\(--toolbar-h\)/);
    expect(sheet("tokens.css")).toMatch(/--sticky-top:\s*0px/);
  });
});

test("no stylesheet negates a var() with a unary minus", () => {
  // `left: -var(--space-1)` is not CSS. There is no unary minus in front of a
  // function, so the value is invalid at computed-value time and the WHOLE
  // declaration is dropped — silently, with the property falling back to its
  // initial value. It shipped on `.rail-row.active::before`, where `left:
  // auto` resolves to the static position and painted the active workspace's
  // accent bar straight down the middle of its icon. The spelling that works
  // is `calc(-1 * var(--x))`.
  // EVERY SHEET IN THE DIRECTORY, read rather than listed. The five names
  // that used to be written here were a list nobody was going to extend:
  // `tokens.css`, `uilet.css` and `fonts.css` were never in it, and a sheet
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

describe("a work item's history", () => {
  // A HISTORY ENTRY IS AS LONG AS THE CHANGE WAS.
  //
  // The work item's history row held its description in `.truncate`: one line,
  // ellipsed. A create moves every field `TaskDeltas` compares and
  // `describeHistory` renders a clause each, so the cut landed inside `Status: `
  // — and took the `quiet` marker and the `turn →` link with it, clipped out of
  // sight and still in the tab order, on a panel with a page of unused height
  // beneath it.
  //
  // IN THE SHEET because there is nowhere else: jsdom computes no layout, so
  // every case that renders this panel stays green whether the cell wraps or
  // ellipses. The other direction is covered already — `.work-hist-what` is
  // declared here and written only by WorkItem.tsx, so classes.test.ts fails if
  // either side alone goes back.
  test("a work item's history entry wraps rather than ellipsing what changed", () => {
    const css = sheet("screens.css");
    const what = block(css, ".work-hist-what");
    expect(what).toMatch(/overflow-wrap:\s*anywhere/);
    // The three halves of `.truncate`, none of which may come back here.
    expect(what).not.toMatch(/white-space:\s*nowrap/);
    expect(what).not.toMatch(/text-overflow/);
    expect(what).not.toMatch(/overflow:\s*hidden/);

    // AND THE ROW AROUND IT IS BUILT FOR MORE THAN ONE LINE. A centred glyph
    // tracks the row's height and slides to the middle of a four-line entry;
    // entries 8px apart whose own lines are 18px apart have no boundary. And the
    // tail has a track of its own, so the sentence cannot swallow the control.
    expect(block(css, ".work-hist-row > svg")).toMatch(/align-self:\s*start/);
    const row = block(css, ".work-hist-row");
    expect(row).toMatch(/border-bottom:\s*1px solid var\(--border-subtle\)/);
    expect(row).toMatch(/align-items:\s*baseline/);
    expect(row).toMatch(/grid-template-columns:\s*18px minmax\(0, 1fr\) auto auto/);
  });
});
