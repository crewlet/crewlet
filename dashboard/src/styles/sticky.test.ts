// @vitest-environment node
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { expect, test } from "vitest";
import { STYLES, sheetRules } from "../test/sheets.ts";

/**
 * WHAT STOPS WHERE, when a screen is scrolled.
 *
 * Several things stick to the top of one scroller at once — a screen's
 * toolbar, a grid's column heads, a grouped list's group heads, an item's side
 * rail, the inbox's detail pane — and only ONE of them owns the top. The
 * toolbar does: it is opaque, it is at `--z-index-sticky`, and the kit's
 * scroller (`.crewlet-app-shell__main:has(.toolbar)`) publishes its height as
 * `--sticky-top` so everything else can start below it.
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

/** Every rule that sticks, as `file selector -> its own `top` declaration`. */
function stickyTops(): Map<string, string> {
  const out = new Map<string, string>();
  // THE SHARED WALK (`test/sheets.ts`), which reads the first rule inside an
  // at-rule too. This file's own copy took everything before a rule's brace
  // as its selector, so the first rule of every `@media` and `@container`
  // block came out as `@media (…) { .x`, was discarded as an at-rule, and a
  // band that stuck at 0 there was invisible to this gate.
  for (const { file, selector, body } of sheetRules()) {
    if (!/position:\s*sticky/.test(body)) continue;
    const top = /(?:^|;)\s*top:\s*([^;]+);/.exec(body);
    // A sticky rule with no `top` at all is not in this conversation — it is
    // pinning a column with `left`, or it is a rule that only turns stickiness
    // off. Named with its file so a failure says where to look.
    if (top) out.set(`${file} ${selector}`, top[1]!.trim());
  }
  return out;
}

test("the toolbar owns the top of the scroller and publishes where it ends", () => {
  const frame = readFileSync(join(STYLES, "frame.css"), "utf8");
  // THE PUBLISHER. Without this rule `--sticky-top` never leaves its `0px`
  // default and every offset below is a no-op that still reads as deliberate.
  expect(frame).toMatch(
    /\.crewlet-app-shell__main:has\(\.toolbar\)\s*\{[^}]*--sticky-top:\s*var\(--toolbar-h\)/,
  );
  // And the toolbar is the one band entitled to stop at 0, because it is what
  // the others are being offset BY.
  expect(stickyTops().get("frame.css .toolbar")).toBe("0");
});

/**
 * The bands that stick at 0 on purpose, because they never share a column
 * with a toolbar: each names why, and each must still stick at 0.
 */
const BESIDE: Record<string, string> = {
  // The Settings column is the first track of `.section-frame` and a screen's
  // toolbar is inside the SECOND, `.section-body` — so the two never overlap,
  // and an offset would park the column a toolbar's height down for nothing.
  "frame.css .section-column": "beside the section body, never under its toolbar",
};

test("every other sticky band starts below it, never at zero", () => {
  const tops = stickyTops();
  // A gate over an empty roster certifies nothing, and this one is a scan.
  expect(tops.size).toBeGreaterThan(3);
  for (const where of Object.keys(BESIDE)) {
    expect(tops.get(where), `${where} is exempt, so it must still stick at 0`).toBe("0");
  }
  for (const [where, top] of tops) {
    if (where === "frame.css .toolbar" || where in BESIDE) continue;
    expect(
      top,
      `${where} sticks at ${top}: on a screen that draws a toolbar this band ` +
        `stops underneath an opaque box at --z-index-sticky and vanishes. Offset it ` +
        `by var(--sticky-top), which is 0px on a screen with no toolbar.`,
    ).toContain("var(--sticky-top)");
  }
});
