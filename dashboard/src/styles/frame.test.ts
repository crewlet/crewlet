// @vitest-environment node
import { readFileSync } from "node:fs";
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

describe("the frame's layout", () => {
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
  });

  test("a panel title is capped by its head, so a long one ellipses", () => {
    // `flex: 0 0 auto` is about the competition with the subtitle — the title
    // must not be the one that gives way — and on its own it also means the
    // item is never clamped at all: its used main size is its max-content
    // width, the inner `truncate` span never shrinks, `text-overflow` never
    // fires, and `.panel-flush`'s clip cuts a checklist's or a sprint's name
    // mid-word at the panel edge. A max main size is what flexbox still
    // applies at a flex factor of 0.
    const head = block(sheet("components.css"), ".panel-head > .panel-title");
    expect(head).toMatch(/flex:\s*0 0 auto/);
    expect(head).toMatch(/max-width:\s*100%/);
  });

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
  const found: string[] = [];
  for (const name of ["base.css", "components.css", "frame.css", "screens.css", "shell.css"]) {
    const text = sheet(name);
    for (const m of text.matchAll(/[:\s(]-var\(/g)) {
      found.push(`${name}:${text.slice(0, m.index).split("\n").length}`);
    }
  }
  expect(found, "a minus in front of var() drops the declaration — use calc(-1 * …)").toEqual([]);
});
