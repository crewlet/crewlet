// @vitest-environment node
import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

/**
 * WHAT STOPS WHERE, when a screen is scrolled.
 *
 * Several things stick to the top of one scroller at once — a screen's
 * toolbar, a grid's column heads, a grouped list's group heads, an item's side
 * rail, the inbox's detail pane — and only ONE of them owns the top. The
 * toolbar does: it is opaque, it is at `--z-sticky`, and `.screen:has(.toolbar)`
 * publishes its height as `--sticky-top` so everything else can start below it.
 * A band that stops at 0 instead stops UNDERNEATH it and is simply gone, with
 * no error and nothing in the markup to read: the head is in the DOM, it is
 * painted, and an opaque box at a higher stacking level is over it.
 *
 * That is what the work screen shipped. `.work-filters` was a byte-for-byte
 * copy of `.toolbar` — same `position`, same `top`, same `z-index`, same
 * background — under a different class name, so `:has(.toolbar)` never matched,
 * `--sticky-top` stayed at its `0px` default, and the grid's own column heads
 * (which do offset themselves correctly) parked under the filter bar because
 * the offset they were given was zero.
 *
 * ASSERTED IN THE SHEETS, because jsdom computes no layout and resolves no
 * custom property: a rendered-DOM suite cannot see one box cover another, and
 * the failure has no DOM signature at all.
 *
 * TWO-SIDED. The publisher has to exist, or every offset here resolves to the
 * `0px` fallback and the whole roster is decorative; and a `top: 0` on anything
 * but the toolbar is the bug itself.
 */

const STYLES = fileURLToPath(new URL(".", import.meta.url));
const sheets = () =>
  readdirSync(STYLES)
    .filter((f) => f.endsWith(".css"))
    .map((f) => [f, readFileSync(join(STYLES, f), "utf8")] as const);

/** Every rule that sticks, as `selector -> its own `top` declaration`. */
function stickyTops(): Map<string, string> {
  const out = new Map<string, string>();
  for (const [file, css] of sheets()) {
    // SPLIT ON THE CLOSING BRACE rather than matching whole rules. A single
    // `/\}\s*([^{}]*)\{([^{}]*)\}/g` has to consume the `}` that precedes a
    // rule in order to anchor on it, which leaves the next rule without one —
    // so it silently reports every OTHER rule, and this gate came back empty
    // the first time it was written that way.
    const bare = css.replace(/\/\*[\s\S]*?\*\//g, "");
    for (const chunk of bare.split("}")) {
      const at = chunk.lastIndexOf("{");
      if (at < 0) continue;
      const selector = chunk.slice(0, at).trim().replace(/\s+/g, " ");
      const body = chunk.slice(at + 1);
      if (!selector || selector.startsWith("@")) continue;
      if (!/position:\s*sticky/.test(body)) continue;
      const top = /(?:^|;)\s*top:\s*([^;]+);/.exec(body);
      // A sticky rule with no `top` at all is not in this conversation — it is
      // pinning a column with `left`, or it is a rule that only turns stickiness
      // off. Named with its file so a failure says where to look.
      if (top) out.set(`${file} ${selector}`, top[1]!.trim());
    }
  }
  return out;
}

test("the toolbar owns the top of the scroller and publishes where it ends", () => {
  const shell = readFileSync(join(STYLES, "shell.css"), "utf8");
  // THE PUBLISHER. Without this rule `--sticky-top` never leaves its `0px`
  // default and every offset below is a no-op that still reads as deliberate.
  expect(shell).toMatch(/\.screen:has\(\.toolbar\)\s*\{[^}]*--sticky-top:\s*var\(--toolbar-h\)/);
  // And the toolbar is the one band entitled to stop at 0, because it is what
  // the others are being offset BY.
  expect(stickyTops().get("shell.css .toolbar")).toBe("0");
});

test("every other sticky band starts below it, never at zero", () => {
  const tops = stickyTops();
  // A gate over an empty roster certifies nothing, and this one is a scan.
  expect(tops.size).toBeGreaterThan(3);
  for (const [where, top] of tops) {
    if (where === "shell.css .toolbar") continue;
    expect(
      top,
      `${where} sticks at ${top}: on a screen that draws a toolbar this band ` +
        `stops underneath an opaque box at --z-sticky and vanishes. Offset it ` +
        `by var(--sticky-top), which is 0px on a screen with no toolbar.`,
    ).toContain("var(--sticky-top)");
  }
});
