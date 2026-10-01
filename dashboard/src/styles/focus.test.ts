// @vitest-environment node
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

/**
 * NO RULE OF OURS TAKES THE FOCUS OUTLINE AWAY.
 *
 * The kit's baseline (`@crewlethq/tokens/css/base.css`) draws keyboard focus as
 * an OUTLINE, and says why: forced-colors mode drops every box-shadow and keeps
 * outlines, so a ring drawn as a shadow alone disappears for exactly the
 * readers who most need one. A rule that wants the shadow ring pairs it with
 * `outline: 2px solid transparent`, which forced colors repaints in a system
 * colour.
 *
 * Two shapes broke that here and neither is visible in a normal-contrast
 * screenshot: `.pulse-fact:focus-visible` and `.inbox-row:focus-visible` set
 * `outline: none` beside `box-shadow: var(--shadow-focus)`, so under forced
 * colors those rows had no focus indicator at all; and `.tabpanel:focus {
 * outline: none }` at (0,2,0) outranked the baseline's `:focus-visible` ring
 * on a panel that takes a tab stop, so it had none in any mode.
 *
 * So: a rule whose selector names `:focus` or `:focus-visible` (or
 * `:focus-within`) may not set the outline to nothing. `:not(:focus-visible)`
 * is exempt — removing a ring the keyboard never asked for is the baseline's
 * own `:focus { outline: none }`.
 */

const STYLES = fileURLToPath(new URL(".", import.meta.url));

function sheets(): { name: string; css: string }[] {
  return readdirSync(STYLES)
    .filter((f) => f.endsWith(".css"))
    .map((name) => ({
      name,
      css: readFileSync(join(STYLES, name), "utf8").replace(/\/\*[\s\S]*?\*\//g, ""),
    }));
}

const NOTHING = /(^|;|\s)outline\s*:\s*(none|0(px)?)\s*(;|$|!)/m;

test("a focus rule never removes the outline", () => {
  const offenders: string[] = [];
  for (const { name, css } of sheets()) {
    for (const m of css.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
      const selectors = m[1]!.split(",").map((s) => s.trim());
      const focused = selectors.filter((s) =>
        /:focus(-visible|-within)?\b/.test(s.replace(/:not\(:focus-visible\)/g, "")),
      );
      if (focused.length > 0 && NOTHING.test(m[2]!)) {
        offenders.push(`${name}: ${focused.join(", ")}`);
      }
    }
  }
  expect(
    offenders,
    "pair a shadow ring with `outline: 2px solid transparent` instead — forced colors keeps outlines",
  ).toEqual([]);
});

test("the gate reads a rule the way the sheets write it", () => {
  // THE CONTROL: the scan is only worth something if it would have caught the
  // two rules it was written for. Held against their literal shape.
  const probe = ".row:focus-visible {\n  outline: none;\n  box-shadow: var(--shadow-focus);\n}";
  const m = /([^{}]+)\{([^{}]*)\}/.exec(probe)!;
  expect(NOTHING.test(m[2]!)).toBe(true);
  expect(NOTHING.test("outline: 2px solid transparent;")).toBe(false);
});
