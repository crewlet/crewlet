// @vitest-environment node
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "vitest";

/**
 * The chrome's layout rules that fail SILENTLY and correctly in every other
 * gate — frame.css, shell.css and the shared panel in components.css.
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
    const css = sheet("frame.css");
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
